package probe

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

var zurich, _ = time.LoadLocation("Europe/Zurich")

// clock is a settable time source for the module.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func (c *clock) set(t time.Time)     { c.t = t }
func at(s string) time.Time          { t, _ := time.ParseInLocation("2006-01-02 15:04", s, zurich); return t }
func cfgJSON(extra string) json.RawMessage {
	return json.RawMessage(`{"targets":[{"host":"fra","name":"Matrix","tcp":"127.0.0.1:1"},{"host":"fra","name":"通话","http":"https://127.0.0.1:1/"}],"timezone":"Europe/Zurich"` + extra + `}`)
}

func uptimeModule(t *testing.T, dir string, c *clock, posted *[]message.Message, extra string) *Module {
	t.Helper()
	env := bot.Env{Host: "us", Log: slog.New(slog.DiscardHandler), StateDir: dir, Interval: 30 * time.Second,
		Post: func(_ context.Context, msg message.Message) error {
			if posted != nil {
				*posted = append(*posted, msg)
			}
			return nil
		}}
	mod, err := newWithClock(env, cfgJSON(extra), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return mod
}

// feed records n rounds, interval apart, with err for the given target.
func feed(m *Module, c *clock, n int, errFor map[string]error) {
	for range n {
		for _, t := range m.cfg.Targets {
			m.st.record(t.Name, c.t, errFor[t.Name], 300*time.Millisecond, 88*time.Millisecond)
		}
		m.dirty = true
		c.add(m.interval)
	}
}

func TestWindowAndIncidents(t *testing.T) {
	c := &clock{at("2026-11-10 10:00")}
	m := uptimeModule(t, "", c, nil, "")
	feed(m, c, 100, nil)
	feed(m, c, 6, map[string]error{"通话": errors.New("HTTP 502")}) // 3 minutes down
	feed(m, c, 94, nil)
	h := m.st.Targets["通话"]
	w := h.window(at("2026-11-10 00:00"), at("2026-11-11 00:00"))
	if p, _ := w.Availability(); w.OK != 194 || w.Fail != 6 || pct(p, true) != "97.00%" {
		t.Fatalf("window %+v, %s", w, pct(p, true))
	}
	if m.down(w) != 3*time.Minute {
		t.Errorf("down %v", m.down(w))
	}
	if w.MeanMs() != 300 || w.PercentileMs(0.95) != 300 {
		t.Errorf("latency mean %d p95 %d", w.MeanMs(), w.PercentileMs(0.95))
	}
	if len(h.Incidents) != 1 || h.Open != nil {
		t.Fatalf("incidents %+v open %+v", h.Incidents, h.Open)
	}
	in := h.Incidents[0]
	if in.End-in.Start != 180 || in.Reason != "HTTP 502" {
		t.Errorf("incident %+v", in)
	}
	if got := m.incidentLine("fra", "通话", in, c.t); got != "11-10 10:50–10:53 通话 HTTP 502（3 分钟）" {
		t.Errorf("incident line %q", got)
	}
	if p, ok := m.st.Targets["Matrix"].window(at("2026-11-10 00:00"), at("2026-11-11 00:00")).Availability(); !ok || pct(p, ok) != "100%" {
		t.Errorf("Matrix %v", p)
	}
}

func TestPctNeverRoundsUp(t *testing.T) {
	for in, want := range map[float64]string{100: "100%", 99.999: "99.99%", 99.95: "99.95%", 12.345: "12.34%"} {
		if got := pct(in, true); got != want {
			t.Errorf("pct(%v) = %s, want %s", in, got, want)
		}
	}
	if pct(0, false) != "-" {
		t.Error("no data")
	}
}

func TestHourCap(t *testing.T) {
	s := newState()
	start := at("2026-01-01 00:00")
	for h := range hourBuckets + 50 {
		s.record("x", start.Add(time.Duration(h)*time.Hour), nil, time.Millisecond, 0)
	}
	if n := len(s.Targets["x"].Hours); n != hourBuckets {
		t.Errorf("%d hours kept, want %d", n, hourBuckets)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at("2026-11-10 10:00")}
	m := uptimeModule(t, dir, c, nil, "")
	feed(m, c, 10, map[string]error{"Matrix": errors.New("超时")})
	m.Stop(context.Background())

	m2 := uptimeModule(t, dir, c, nil, "")
	h := m2.st.Targets["Matrix"]
	if w := h.window(at("2026-11-10 00:00"), at("2026-11-11 00:00")); w.Fail != 10 || h.Open == nil || h.Open.Reason != "超时" {
		t.Fatalf("restored %+v open %+v", w, h.Open)
	}
	// a target removed from the configuration is forgotten
	m2.st.Targets["gone"] = &targetHist{}
	m2.dirty = true
	m2.Stop(context.Background())
	if m3 := uptimeModule(t, dir, c, nil, ""); m3.st.Targets["gone"] != nil {
		t.Error("removed target kept")
	}

	// a damaged file is set aside, not fatal
	os.WriteFile(filepath.Join(dir, "probe.json"), []byte("{broken"), 0o600)
	m4 := uptimeModule(t, dir, c, nil, "")
	if len(m4.st.Targets) != 0 {
		t.Error("damaged history loaded")
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, "probe.json.broken-*")); len(aside) != 1 {
		t.Errorf("damaged file not kept aside: %v", aside)
	}
}

func TestControlsHoldWhenOffline(t *testing.T) {
	open, _ := net.Listen("tcp", "127.0.0.1:0")
	defer open.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().String()
	closed.Close()

	raw := func(target, control string) json.RawMessage {
		return json.RawMessage(`{"targets":[{"host":"fra","name":"Matrix","tcp":"` + target + `"}],
			"controls":[{"name":"cf","tcp":"` + control + `"}],"timeout":"300ms"}`)
	}
	env := bot.Env{Host: "us", Log: slog.New(slog.DiscardHandler), Interval: 30 * time.Second}
	m, err := newWithClock(env, raw(dead, open.Addr().String()), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	// online, target down: recorded and alerted
	f := m.check(context.Background())
	if len(f) != 1 || f[0].Key != "probe:Matrix" || m.st.Targets["Matrix"].window(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)).Fail != 1 {
		t.Fatalf("online round: %v", f)
	}
	// now the control fails too: this machine is offline
	m.cfg.Controls[0].TCP = dead
	f = m.check(context.Background())
	if len(f) != 2 || f[0].Key != "probe:Matrix" || f[1].Key != "self:probe-offline" {
		t.Fatalf("offline round: %v", f)
	}
	if w := m.st.Targets["Matrix"].window(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); w.Fail != 1 {
		t.Errorf("offline round recorded: %+v", w)
	}
	if s, _ := m.Summary(context.Background()); !strings.Contains(s[0], "本机外网不通") {
		t.Errorf("summary %q", s)
	}
}

func TestReportSchedule(t *testing.T) {
	dir := t.TempDir()
	var posted []message.Message
	// Start on Wednesday: the week and month in progress will be reported.
	c := &clock{at("2026-11-11 12:00")}
	m := uptimeModule(t, dir, c, &posted, `,"weekly_report":true,"monthly_report":true`)
	if len(m.Jobs()) != 2 {
		t.Fatalf("jobs %v", m.Jobs())
	}
	run := func(when string) int {
		c.set(at(when))
		posted = nil
		if err := m.reportIfDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		return len(posted)
	}
	if n := run("2026-11-15 23:00"); n != 0 {
		t.Fatalf("report before the week ended: %d", n)
	}
	if n := run("2026-11-16 08:50"); n != 0 {
		t.Fatalf("report before 09:00: %d", n)
	}
	if n := run("2026-11-16 09:00"); n != 1 || !strings.HasPrefix(posted[0].Title, "可用性周报 11-09 至 11-15") {
		t.Fatalf("weekly: %d %+v", n, posted)
	}
	if n := run("2026-11-16 09:10"); n != 0 {
		t.Fatalf("weekly sent twice: %d", n)
	}
	// Before 09:00 on the 1st: only the week of 11-23 (due 11-30 09:00).
	if n := run("2026-12-01 08:59"); n != 1 || !strings.HasPrefix(posted[0].Title, "可用性周报 11-23 至 11-29") {
		t.Fatalf("12-01 08:59: %d %+v", n, posted)
	}
	// The bot was down over 09:00 on the 1st: the monthly report follows on
	// the next start, once.
	m.Stop(context.Background())
	m = uptimeModule(t, dir, c, &posted, `,"weekly_report":true,"monthly_report":true`)
	if n := run("2026-12-02 14:00"); n != 1 || !strings.HasPrefix(posted[0].Title, "可用性月报 11-01 至 11-30") {
		t.Fatalf("catch-up after downtime: %d %+v", n, posted)
	}
	if n := run("2026-12-02 14:10"); n != 0 {
		t.Fatalf("monthly sent twice: %d", n)
	}
}

func TestReportContent(t *testing.T) {
	c := &clock{at("2026-11-09 00:00")}
	m := uptimeModule(t, "", c, nil, "")
	week := int(7 * 24 * time.Hour / m.interval)
	feed(m, c, week/2, nil)
	feed(m, c, 20, map[string]error{"通话": errors.New("HTTP 502")}) // 10 minutes
	feed(m, c, week/2-20, nil)
	r := m.report("周报", at("2026-11-09 00:00"), at("2026-11-16 00:00"))
	text := r.Plain()
	// one host: names without it, the host in the title; each target is a
	// name line followed by an ASCII-aligned figures line
	for _, want := range []string{
		"✅ Matrix\n   100%     停机 0s     往返 300ms\n",
		// 10 minutes in a week is 0.099 %: 99.90 %, just within three
		// nines (a week's budget is 10.08 minutes)
		"✅ 通话\n   99.90%   停机 10m    往返 88ms   请求 300ms（P95 ≤300ms）\n",
		"📈 可用性周报 11-09 至 11-15（fra，目标 99.9%，us 观测）",
		"中断 1 次：",
		"11-12 12:00–12:10 通话 HTTP 502（10 分钟）",
		"数据覆盖 100.0%",
		"往返 = TCP 建连耗时",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

func TestUptimeCommand(t *testing.T) {
	c := &clock{at("2026-11-01 00:00")}
	m := uptimeModule(t, "", c, nil, "")
	feed(m, c, 2*24*120, nil)
	feed(m, c, 100, map[string]error{"Matrix": errors.New("超时")}) // 50 minutes: over the 43-minute budget
	feed(m, c, 10, nil)
	msg, _ := m.cmdUptime(context.Background(), bot.Request{})
	text := msg.Plain()
	for _, want := range []string{
		"（fra，目标 99.9%，本月停机预算 43 分钟）",
		"24h      7d       30d      本月停机\n",
		"96.51%   98.29%   98.29%   50m     ❌ Matrix\n",
		"100%     100%     100%     0s      ✅ 通话\n",
		"最近中断：", "Matrix 超时（50 分钟）",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("uptime lacks %q:\n%s", want, text)
		}
	}
	// targets on several machines carry their host
	m.cfg.Targets[1].Host = "us2"
	msg, _ = m.cmdUptime(context.Background(), bot.Request{})
	if text := msg.Plain(); !strings.Contains(text, "❌ fra Matrix") || !strings.Contains(text, "✅ us2 通话") ||
		!strings.Contains(text, "（目标 99.9%，") {
		t.Errorf("several hosts:\n%s", text)
	}
}

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 59 * time.Second: "59s", 10 * time.Minute: "10m", 43*time.Minute + 12*time.Second: "43m",
		65 * time.Minute: "1h05m", 26 * time.Hour: "26h00m",
	} {
		if got := dur(d); got != want {
			t.Errorf("dur(%v) = %q, want %q", d, got, want)
		}
	}
}
