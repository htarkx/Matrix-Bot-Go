package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// History of every target: hourly buckets (for availability and latency)
// and incidents (for "what happened"). Hours are kept hourBuckets long,
// enough for a monthly report written on the 1st; incidents a year.
const (
	hourBuckets  = 35 * 24
	incidentKeep = 400 * 24 * time.Hour
	stateVersion = 1
)

// latencyBins are upper bounds (ms) of the latency histogram per bucket;
// the last bin is everything slower.
var latencyBins = [...]int64{50, 100, 200, 300, 500, 750, 1000, 2000, 5000, 1 << 62}

// bucket is one hour (UTC-aligned) of one target.
type bucket struct {
	Start  int64                   `json:"t"`  // unix seconds
	OK     int                     `json:"ok"` // successful probes
	Fail   int                     `json:"fail"`
	LatSum int64                   `json:"lat_ms"` // sum over successful probes
	Lat    [len(latencyBins)]int32 `json:"lat"`    // histogram over latencyBins
}

// Incident is one outage: from the first failed probe to the first good one.
type Incident struct {
	Start  int64  `json:"start"`
	End    int64  `json:"end,omitempty"` // 0 while open
	Reason string `json:"reason"`
}

type targetHist struct {
	Hours     []bucket   `json:"hours"` // ascending Start
	Incidents []Incident `json:"incidents"`
	Open      *Incident  `json:"open,omitempty"`
}

// state is everything the module keeps across restarts.
type state struct {
	Version int                    `json:"version"`
	Targets map[string]*targetHist `json:"targets"`
	// LastWeekly / LastMonthly are the end (unix) of the last reported
	// period, so a report missed while the bot was down is sent later.
	LastWeekly  int64 `json:"last_weekly"`
	LastMonthly int64 `json:"last_monthly"`
}

func newState() *state { return &state{Version: stateVersion, Targets: map[string]*targetHist{}} }

func (s *state) target(name string) *targetHist {
	h := s.Targets[name]
	if h == nil {
		h = &targetHist{}
		s.Targets[name] = h
	}
	return h
}

// record adds one probe result at time at.
func (s *state) record(name string, at time.Time, err error, latency time.Duration) {
	h := s.target(name)
	start := at.Truncate(time.Hour).Unix()
	if n := len(h.Hours); n == 0 || h.Hours[n-1].Start < start {
		h.Hours = append(h.Hours, bucket{Start: start})
		if len(h.Hours) > hourBuckets {
			h.Hours = h.Hours[len(h.Hours)-hourBuckets:]
		}
	}
	b := &h.Hours[len(h.Hours)-1] // the newest hour, also if the clock went back
	if err != nil {
		b.Fail++
		if h.Open == nil {
			h.Open = &Incident{Start: at.Unix(), Reason: err.Error()}
		}
		return
	}
	b.OK++
	ms := latency.Milliseconds()
	b.LatSum += ms
	for i, upper := range latencyBins {
		if ms <= upper {
			b.Lat[i]++
			break
		}
	}
	if h.Open != nil {
		h.Open.End = at.Unix()
		h.Incidents = append(h.Incidents, *h.Open)
		h.Open = nil
	}
	cut := at.Add(-incidentKeep).Unix()
	for len(h.Incidents) > 0 && h.Incidents[0].End < cut {
		h.Incidents = h.Incidents[1:]
	}
}

// Window is availability and latency of one target over a period.
type Window struct {
	OK, Fail int
	LatSum   int64
	Lat      [len(latencyBins)]int32
}

// window sums the hours that start in [from, to).
func (h *targetHist) window(from, to time.Time) Window {
	var w Window
	if h == nil {
		return w
	}
	f, t := from.Unix(), to.Unix()
	i := sort.Search(len(h.Hours), func(i int) bool { return h.Hours[i].Start >= f })
	for ; i < len(h.Hours) && h.Hours[i].Start < t; i++ {
		b := h.Hours[i]
		w.OK += b.OK
		w.Fail += b.Fail
		w.LatSum += b.LatSum
		for j, n := range b.Lat {
			w.Lat[j] += n
		}
	}
	return w
}

// Availability is the share of good probes in percent; ok is false
// without any probe.
func (w Window) Availability() (pct float64, ok bool) {
	n := w.OK + w.Fail
	if n == 0 {
		return 0, false
	}
	return 100 * float64(w.OK) / float64(n), true
}

// MeanMs is the mean latency of good probes.
func (w Window) MeanMs() int64 {
	if w.OK == 0 {
		return 0
	}
	return w.LatSum / int64(w.OK)
}

// PercentileMs is an upper bound of the q-th latency percentile (0..1),
// from the histogram: "p95 ≤ 500ms". 0 without data; -1 when it falls in
// the open last bin.
func (w Window) PercentileMs(q float64) int64 {
	var total int64
	for _, n := range w.Lat {
		total += int64(n)
	}
	if total == 0 {
		return 0
	}
	need := int64(q*float64(total) + 0.999999)
	var seen int64
	for i, n := range w.Lat {
		seen += int64(n)
		if seen >= need {
			if i == len(latencyBins)-1 {
				return -1
			}
			return latencyBins[i]
		}
	}
	return -1
}

// incidentsIn lists incidents overlapping [from, to), oldest first,
// including the open one (End 0).
func (h *targetHist) incidentsIn(from, to time.Time) []Incident {
	if h == nil {
		return nil
	}
	var out []Incident
	for _, in := range append(append([]Incident(nil), h.Incidents...), openList(h.Open)...) {
		end := in.End
		if end == 0 {
			end = 1 << 62
		}
		if in.Start < to.Unix() && end > from.Unix() {
			out = append(out, in)
		}
	}
	return out
}

func openList(o *Incident) []Incident {
	if o == nil {
		return nil
	}
	return []Incident{*o}
}

// load reads the state file; a missing file is a fresh state.
func load(path string) (*state, error) {
	s := newState()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("%s: state version %d, want %d", path, s.Version, stateVersion)
	}
	if s.Targets == nil {
		s.Targets = map[string]*targetHist{}
	}
	return s, nil
}

// writeFile writes atomically: a crash leaves the old file or the new one.
func writeFile(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".probe-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
