package hostwatch

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// traffic is rx/tx bytes.
type traffic struct{ rx, tx uint64 }

func (t *traffic) add(o traffic) { t.rx += o.rx; t.tx += o.tx }

// netUsage is what /status and /net show, from vnstat's database.
type netUsage struct {
	iface string
	since time.Time // vnstat started counting
	h24   traffic
	d30   traffic
	month traffic
	days  []dayTraffic // last 7 days, oldest first
}

type dayTraffic struct {
	day time.Time
	traffic
}

type vnstatJSON struct {
	Interfaces []struct {
		Name    string `json:"name"`
		Created struct {
			Timestamp int64 `json:"timestamp"`
		} `json:"created"`
		Traffic struct {
			Hour  []vnEntry `json:"hour"`
			Day   []vnEntry `json:"day"`
			Month []vnEntry `json:"month"`
		} `json:"traffic"`
	} `json:"interfaces"`
}

type vnEntry struct {
	Timestamp int64  `json:"timestamp"`
	RX        uint64 `json:"rx"`
	TX        uint64 `json:"tx"`
}

// readVnstat runs vnstat; nil when it is missing or has not seen iface yet.
func readVnstat(ctx context.Context, iface string, now time.Time) *netUsage {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "vnstat", "--json", "-i", iface).Output()
	if err != nil {
		return nil
	}
	return parseVnstat(out, iface, now)
}

func parseVnstat(raw []byte, iface string, now time.Time) *netUsage {
	var v vnstatJSON
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	for _, i := range v.Interfaces {
		if i.Name != iface {
			continue
		}
		sum := func(es []vnEntry, since time.Time) traffic {
			var t traffic
			for _, e := range es {
				if !time.Unix(e.Timestamp, 0).Before(since) {
					t.add(traffic{e.RX, e.TX})
				}
			}
			return t
		}
		y, m, d := now.Date()
		dayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		u := &netUsage{
			iface: iface,
			since: time.Unix(i.Created.Timestamp, 0),
			h24:   sum(i.Traffic.Hour, now.Add(-24*time.Hour)),
			d30:   sum(i.Traffic.Day, dayStart.AddDate(0, 0, -29)),
		}
		if ms := i.Traffic.Month; len(ms) > 0 {
			u.month = traffic{ms[len(ms)-1].RX, ms[len(ms)-1].TX}
		}
		days := i.Traffic.Day
		if len(days) > 7 {
			days = days[len(days)-7:]
		}
		for _, e := range days {
			u.days = append(u.days, dayTraffic{time.Unix(e.Timestamp, 0).In(now.Location()), traffic{e.RX, e.TX}})
		}
		return u
	}
	return nil
}
