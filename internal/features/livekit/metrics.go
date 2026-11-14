package livekit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

// Line quality comes from LiveKit's Prometheus endpoint (prometheus.port in
// livekit.yaml). Its counters and histograms are cumulative and aggregated
// over every participant, labelled only by direction ("incoming" =
// publishers' uplink to the SFU, "outgoing" = the SFU's downlink to
// subscribers, from their RTCP reports), track source/type and country. So
// the bot scrapes on a schedule and reports the difference over a recent
// window, for audio: what makes voice garbled.

// snapshot is one scrape: series key -> value.
type snapshot struct {
	at   time.Time
	vals map[string]float64
}

// DirStats is one direction's audio quality over the window.
type DirStats struct {
	Samples  int     // loss observations (one per stream per stats interval)
	LossPct  float64 // mean loss over those observations
	BadPct   float64 // share of observations with more than 5 % loss
	JitterMs float64 // mean jitter; 0 when none was reported
	RTTMs    float64 // mean round trip; 0 when none was reported
}

// Quality is both directions over Window.
type Quality struct {
	Window   time.Duration
	Up, Down DirStats
}

// metrics keeps recent scrapes.
type metrics struct {
	url    string
	window time.Duration
	http   *http.Client

	mu    sync.Mutex
	snaps []snapshot // oldest first, a few windows long
}

var labelRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// wanted are the series the bot sums, per direction, for audio only.
var wanted = map[string]bool{
	"livekit_packet_loss_percent_sum":    true,
	"livekit_packet_loss_percent_count":  true,
	"livekit_packet_loss_percent_bucket": true, // only le="5"
	"livekit_jitter_us_sum":              true,
	"livekit_jitter_us_count":            true,
	"livekit_rtt_ms_sum":                 true,
	"livekit_rtt_ms_count":               true,
}

// parse sums the wanted series of a Prometheus text exposition over all
// labels except direction (and le), keyed "name|direction".
func parse(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest, _ := strings.Cut(line, "{")
		if !wanted[name] {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		l := map[string]string{}
		for _, m := range labelRe.FindAllStringSubmatch(labels, -1) {
			l[m[1]] = m[2]
		}
		if l["type"] != "AUDIO" {
			continue
		}
		if name == "livekit_packet_loss_percent_bucket" && l["le"] != "5" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		out[name+"|"+l["direction"]] += v
	}
	return out, sc.Err()
}

// scrape reads the endpoint once and keeps the result.
func (x *metrics) scrape(ctx context.Context, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x.url, nil)
	if err != nil {
		return err
	}
	resp, err := x.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics: HTTP %d", resp.StatusCode)
	}
	vals, err := parse(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.snaps = append(x.snaps, snapshot{at: now, vals: vals})
	keep := now.Add(-3 * x.window)
	for len(x.snaps) > 2 && x.snaps[0].at.Before(keep) {
		x.snaps = x.snaps[1:]
	}
	return nil
}

// quality compares the newest scrape with the newest one at least a window
// older (or the oldest kept). ok is false without two scrapes, after a
// LiveKit restart reset the counters, or when no audio was measured.
func (x *metrics) quality() (q Quality, ok bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.snaps) < 2 {
		return q, false
	}
	b := x.snaps[len(x.snaps)-1]
	a := x.snaps[0]
	for _, s := range x.snaps[:len(x.snaps)-1] {
		if !s.at.After(b.at.Add(-x.window)) {
			a = s
		}
	}
	d := func(name, dir string) float64 { return b.vals[name+"|"+dir] - a.vals[name+"|"+dir] }
	dir := func(name string) (DirStats, bool) {
		var s DirStats
		n := d("livekit_packet_loss_percent_count", name)
		if n < 0 || d("livekit_jitter_us_count", name) < 0 || d("livekit_rtt_ms_count", name) < 0 {
			return s, false // counters reset: LiveKit restarted in between
		}
		if n > 0 {
			s.Samples = int(n)
			s.LossPct = d("livekit_packet_loss_percent_sum", name) / n
			s.BadPct = 100 * (n - d("livekit_packet_loss_percent_bucket", name)) / n
		}
		if c := d("livekit_jitter_us_count", name); c > 0 {
			s.JitterMs = d("livekit_jitter_us_sum", name) / c / 1000
		}
		if c := d("livekit_rtt_ms_count", name); c > 0 {
			s.RTTMs = d("livekit_rtt_ms_sum", name) / c
		}
		return s, true
	}
	up, okUp := dir("incoming")
	down, okDown := dir("outgoing")
	if !okUp || !okDown || up.Samples+down.Samples == 0 {
		return q, false
	}
	return Quality{Window: b.at.Sub(a.at), Up: up, Down: down}, true
}

// line renders a quality, e.g.
// "上行丢包 0.3% · 下行丢包 1.2% · 抖动 7/9ms · 往返 176ms（最近 1 分钟，音频）".
func (q Quality) line() string {
	pct := func(s DirStats) string {
		if s.Samples == 0 {
			return "—"
		}
		v := fmt.Sprintf("%.1f%%", s.LossPct)
		if s.BadPct >= 10 {
			v += fmt.Sprintf("（%.0f%% 时段 >5%%）", s.BadPct)
		}
		return v
	}
	ms := func(v float64) string {
		if v == 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f", v)
	}
	rtt := q.Up.RTTMs
	if rtt == 0 {
		rtt = q.Down.RTTMs
	}
	mins := max(1, int((q.Window+30*time.Second)/time.Minute))
	return fmt.Sprintf("上行丢包 %s · 下行丢包 %s · 抖动 %s/%sms · 往返 %sms（最近 %d 分钟，音频）",
		pct(q.Up), pct(q.Down), ms(q.Up.JitterMs), ms(q.Down.JitterMs), ms(rtt), mins)
}

// Jobs implements bot.Module: the scraper, when metrics are configured.
func (m *Module) Jobs() []bot.Job {
	if m.metrics == nil {
		return nil
	}
	return []bot.Job{{Name: "livekit-metrics", Every: m.cfg.MetricsEvery.Duration, Run: func(ctx context.Context) error {
		return m.metrics.scrape(ctx, m.now())
	}}}
}

// Checks implements bot.Module: audio loss above the threshold, in either
// direction, over the window. Quiet when nobody is calling (no samples).
func (m *Module) Checks() []bot.Check {
	if m.metrics == nil || m.lossAlert <= 0 {
		return nil
	}
	return []bot.Check{{Name: "livekit-loss", Run: func(context.Context) []bot.Finding {
		q, ok := m.metrics.quality()
		if !ok || q.Up.Samples+q.Down.Samples < minAlertSamples {
			return nil
		}
		if q.Up.LossPct < m.lossAlert && q.Down.LossPct < m.lossAlert {
			return nil
		}
		return []bot.Finding{{Key: "call:loss", Msg: "通话丢包偏高：" + q.line()}}
	}}}
}

// minAlertSamples keeps a single bad report (one stream, one interval)
// from alerting.
const minAlertSamples = 4
