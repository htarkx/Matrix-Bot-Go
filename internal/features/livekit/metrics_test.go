package livekit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

// exposition renders LiveKit-style series: audio loss/jitter/RTT for two
// tracks per direction (summed by the parser), plus video and unrelated
// series it must ignore.
func exposition(upLossSum, upLossN, upBad, downLossSum, downLossN, jitterUs, rttMs, n float64) string {
	var b strings.Builder
	const node = `node_id="ND_x",node_type="SERVER",`
	b.WriteString("# HELP livekit_packet_loss_percent loss\n# TYPE livekit_packet_loss_percent histogram\n")
	for _, d := range []struct {
		dir          string
		sum, cnt, gt float64
	}{{"incoming", upLossSum, upLossN, upBad}, {"outgoing", downLossSum, downLossN, 0}} {
		for _, country := range []string{"", "CN"} { // split in two series: must be summed
			fmt.Fprintf(&b, "livekit_packet_loss_percent_bucket{%scountry=%q,direction=%q,source=\"MICROPHONE\",type=\"AUDIO\",le=\"1\"} 999\n", node, country, d.dir)
			fmt.Fprintf(&b, "livekit_packet_loss_percent_bucket{%scountry=%q,direction=%q,source=\"MICROPHONE\",type=\"AUDIO\",le=\"5\"} %g\n", node, country, d.dir, (d.cnt-d.gt)/2)
			fmt.Fprintf(&b, "livekit_packet_loss_percent_sum{%scountry=%q,direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, country, d.dir, d.sum/2)
			fmt.Fprintf(&b, "livekit_packet_loss_percent_count{%scountry=%q,direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, country, d.dir, d.cnt/2)
		}
		fmt.Fprintf(&b, "livekit_packet_loss_percent_sum{%scountry=\"\",direction=%q,source=\"CAMERA\",type=\"VIDEO\"} 12345\n", node, d.dir)
		fmt.Fprintf(&b, "livekit_jitter_us_sum{%scountry=\"\",direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, d.dir, jitterUs*n)
		fmt.Fprintf(&b, "livekit_jitter_us_count{%scountry=\"\",direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, d.dir, n)
		fmt.Fprintf(&b, "livekit_rtt_ms_sum{%scountry=\"\",direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, d.dir, rttMs*n)
		fmt.Fprintf(&b, "livekit_rtt_ms_count{%scountry=\"\",direction=%q,source=\"MICROPHONE\",type=\"AUDIO\"} %g\n", node, d.dir, n)
	}
	b.WriteString("livekit_packet_total{node_id=\"ND_x\",country=\"\",direction=\"incoming\",transmission=\"initial\"} 381\n")
	return b.String()
}

func TestParse(t *testing.T) {
	v, err := parse(strings.NewReader(exposition(10, 20, 2, 4, 8, 7000, 170, 6)))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{
		"livekit_packet_loss_percent_sum|incoming":    10,
		"livekit_packet_loss_percent_count|incoming":  20,
		"livekit_packet_loss_percent_bucket|incoming": 18, // only le="5"
		"livekit_jitter_us_sum|outgoing":              42000,
		"livekit_rtt_ms_count|incoming":               6,
	} {
		if v[k] != want {
			t.Errorf("%s = %g, want %g", k, v[k], want)
		}
	}
	if len(v) != 14 {
		t.Errorf("%d series kept (video or unrelated ones leaked?): %v", len(v), v)
	}
}

// promServer serves whatever exposition is current.
type promServer struct {
	mu   sync.Mutex
	body string
}

func (p *promServer) set(s string) { p.mu.Lock(); p.body = s; p.mu.Unlock() }
func (p *promServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	io.WriteString(w, p.body)
}

func newMetricsModule(t *testing.T, url string, extra string) *Module {
	t.Helper()
	raw := json.RawMessage(`{"url":"http://127.0.0.1:1","metrics_url":"` + url + `"` + extra + `}`)
	mod, err := New(bot.Env{Host: "edge-1", Log: slog.New(slog.DiscardHandler)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return mod.(*Module)
}

func TestQualityWindowAndAlert(t *testing.T) {
	p := &promServer{}
	srv := httptest.NewServer(p)
	defer srv.Close()
	m := newMetricsModule(t, srv.URL, "")
	if len(m.Jobs()) != 1 || len(m.Checks()) != 1 {
		t.Fatal("metrics job or loss check missing")
	}
	t0 := time.Unix(1_800_000_000, 0)
	scrape := func(at time.Duration, body string) {
		p.set(body)
		if err := m.metrics.scrape(context.Background(), t0.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	check := func() []bot.Finding { return m.Checks()[0].Run(context.Background()) }

	scrape(0, exposition(0, 0, 0, 0, 0, 0, 0, 0))
	if _, ok := m.metrics.quality(); ok {
		t.Error("quality from a single scrape")
	}
	// An old, bad period that must fall out of the 1-minute window.
	scrape(30*time.Second, exposition(400, 10, 10, 0, 0, 50000, 300, 10))
	// 45 s later: +20 uplink observations, 2 % mean, 2 above 5 %;
	// +10 downlink, 1 % mean; jitter 8 ms, RTT 176 ms.
	scrape(75*time.Second, exposition(400, 10, 10, 0, 0, 50000, 300, 10))
	scrape(135*time.Second, exposition(440, 30, 12, 10, 10, (50000*10+8000*10)/20.0, (300*10+176*10)/20.0, 20))

	q, ok := m.metrics.quality()
	if !ok {
		t.Fatal("no quality")
	}
	if q.Window != time.Minute || q.Up.Samples != 20 || q.Up.LossPct != 2 || q.Up.BadPct != 10 ||
		q.Down.LossPct != 1 || q.Up.JitterMs != 8 || q.Up.RTTMs != 176 {
		t.Fatalf("quality %+v", q)
	}
	if want := "上行丢包 2.0%（10% 时段 >5%） · 下行丢包 1.0% · 抖动 8/8ms · 往返 176ms（最近 1 分钟，音频）"; q.line() != want {
		t.Errorf("line\n%s\nwant\n%s", q.line(), want)
	}
	if f := check(); len(f) != 0 {
		t.Errorf("alert at 2%%: %v", f)
	}

	// Next minute: uplink loss 8 %, jitter 20 ms, RTT 200 ms -> alert.
	// (exposition takes per-observation means; keep sums cumulative.)
	jit := (580000 + 20000*10) / 30.0
	rtt := (4760 + 200*10) / 30.0
	scrape(195*time.Second, exposition(440+8*20, 50, 12+10, 10, 10, jit, rtt, 30))
	if q, _ := m.metrics.quality(); q.Up.JitterMs != 20 || q.Up.RTTMs != 200 || q.Up.LossPct != 8 {
		t.Errorf("second minute %+v", q.Up)
	}
	f := check()
	if len(f) != 1 || f[0].Key != "call:loss" || !strings.Contains(f[0].Msg, "上行丢包 8.0%") {
		t.Fatalf("findings %v", f)
	}

	// LiveKit restarted: counters back to zero -> no verdict, no alert.
	scrape(255*time.Second, exposition(1, 1, 0, 0, 0, 0, 0, 1))
	if _, ok := m.metrics.quality(); ok {
		t.Error("quality across a counter reset")
	}
	if f := check(); len(f) != 0 {
		t.Errorf("alert across a reset: %v", f)
	}
}

func TestNoAlertWithoutCalls(t *testing.T) {
	p := &promServer{body: exposition(400, 10, 10, 0, 0, 0, 0, 0)}
	srv := httptest.NewServer(p)
	defer srv.Close()
	m := newMetricsModule(t, srv.URL, `,"loss_alert_pct":1`)
	t0 := time.Unix(1_800_000_000, 0)
	m.metrics.scrape(context.Background(), t0)
	m.metrics.scrape(context.Background(), t0.Add(time.Minute))
	if f := m.Checks()[0].Run(context.Background()); len(f) != 0 {
		t.Errorf("alert with nothing measured: %v", f)
	}
	if _, ok := m.metrics.quality(); ok {
		t.Error("quality with nothing measured")
	}
}

func TestMetricsOptional(t *testing.T) {
	mod, err := New(bot.Env{}, json.RawMessage(`{"url":"http://127.0.0.1:1"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	if len(m.Jobs()) != 0 || len(m.Checks()) != 0 || m.qualityLine() != "" {
		t.Error("metrics active without metrics_url")
	}
	if _, err := New(bot.Env{}, json.RawMessage(`{"url":"x","metrics_url":"y","metrics_every":"1m","metrics_window":"1m"}`)); err == nil {
		t.Error("window of one scrape accepted")
	}
	off := newMetricsModule(t, "http://127.0.0.1:1", `,"loss_alert_pct":0`)
	if len(off.Checks()) != 0 {
		t.Error("loss alert active at 0")
	}
}
