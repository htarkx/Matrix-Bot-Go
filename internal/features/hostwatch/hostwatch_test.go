package hostwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/alert"
	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

type noAlerts struct{}

func (noAlerts) Active() []alert.Active { return nil }

// copyProc copies testdata/proc to a temp dir the test may rewrite.
func copyProc(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := os.CopyFS(dst, os.DirFS("testdata/proc"))
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func newModule(t *testing.T, proc string, cfg string) *Module {
	t.Helper()
	raw := json.RawMessage(fmt.Sprintf(`{"proc": %q %s}`, proc, cfg))
	mod, err := New(bot.Env{Host: "edge-1", Log: slog.New(slog.DiscardHandler), Alerts: noAlerts{}}, raw)
	if err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	m.disk = func(string) (diskUsage, error) {
		return diskUsage{total: 100 << 30, used: 40 << 30, free: 60 << 30}, nil
	}
	m.vnstat = func(context.Context, string, time.Time) *netUsage { return nil }
	return m
}

func writeCPU(t *testing.T, dir string, user, idle uint64) {
	t.Helper()
	line := fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\n", user, idle)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keys(fs []bot.Finding) string {
	var k []string
	for _, f := range fs {
		k = append(k, f.Key)
	}
	return strings.Join(k, ",")
}

func TestParsers(t *testing.T) {
	p := procfs("testdata/proc")
	c, err := p.cpu()
	if err != nil || c != (cpuSample{total: 10000, idle: 8000, steal: 0}) {
		t.Errorf("cpu = %+v, %v", c, err)
	}
	if s, err := p.swapPages(); err != nil || s != 1000 {
		t.Errorf("swap pages = %d, %v", s, err)
	}
	mem, err := p.meminfo()
	if err != nil || mem["MemAvailable"] != 500000*1024 || mem["HugePages_Total"] != 0 {
		t.Errorf("meminfo = %v, %v", mem, err)
	}
	if got := p.psi("memory")["avg60"]; got != 2.5 {
		t.Errorf("psi avg60 = %v", got)
	}
	if p.psi("io") != nil {
		t.Error("missing PSI file must give nil")
	}
	if l, err := p.loadavg(); err != nil || l != [3]float64{0.2, 0.14, 0.1} {
		t.Errorf("loadavg = %v, %v", l, err)
	}
	procs := p.procs()
	page := uint64(os.Getpagesize())
	if got := procs[100]; got != (proc{"tuwunel", 600, 13000 * page}) {
		t.Errorf("proc 100 = %+v", got)
	}
	if got := procs[200].comm; got != "my (odd) proc" {
		t.Errorf("comm with parentheses = %q", got)
	}
}

func TestWindowSpan(t *testing.T) {
	w := newWindow[int](time.Hour)
	t0 := time.Unix(0, 0)
	for i := range 11 { // every 30 s for 5 min
		w.add(t0.Add(time.Duration(i)*30*time.Second), i)
	}
	if _, _, ok := w.span(6 * time.Minute); ok {
		t.Error("6 min span from 5 min of history")
	}
	// 90% tolerance: 4m30s of history is enough for "5 minutes", so the
	// newest sample that old (30 s) starts the span
	first, last, ok := w.span(5 * time.Minute)
	if !ok || first.v != 1 || last.v != 10 {
		t.Errorf("5m span = %v %v %v", first, last, ok)
	}
	// rounds up to the newest sample that is old enough
	if first, _, _ := w.span(time.Minute); first.v != 8 {
		t.Errorf("1m span starts at %d, want 8", first.v)
	}
	w.add(t0.Add(2*time.Hour), 99)
	if len(w.samples) != 1 {
		t.Errorf("old samples kept: %d", len(w.samples))
	}
}

func TestCPUAndSwapAlerts(t *testing.T) {
	dir := copyProc(t)
	m := newModule(t, dir, `, "thresholds": {"cpu_window": "1m", "swap_window": "1m"}`)
	t0 := time.Now()

	writeCPU(t, dir, 0, 0)
	m.sample(t0)
	if got := keys(m.evaluate()); got != "" {
		t.Fatalf("one sample: %s", got)
	}
	// 90% busy over the next minute, swap +6000 pages = 100 pages/s
	writeCPU(t, dir, 9000, 1000)
	os.WriteFile(filepath.Join(dir, "vmstat"), []byte("pswpin 3500\npswpout 3500\n"), 0o644)
	m.sample(t0.Add(time.Minute))
	fs := m.evaluate()
	if got := keys(fs); got != "cpu,swap_io" {
		t.Fatalf("findings = %s", got)
	}
	if fs[0].Msg != "CPU 持续高负载：1 分钟平均 90%（steal 0%）" ||
		fs[1].Msg != "正在频繁使用 swap：1 分钟平均换入换出 100 页/秒（约 400K/s）" {
		t.Errorf("messages = %q / %q", fs[0].Msg, fs[1].Msg)
	}
}

func TestMemoryAndDiskAlerts(t *testing.T) {
	dir := copyProc(t)
	os.WriteFile(filepath.Join(dir, "meminfo"), []byte(
		"MemTotal: 1000000 kB\nMemAvailable: 50000 kB\nSwapTotal: 1000 kB\nSwapFree: 100 kB\n"), 0o644)
	m := newModule(t, dir, `, "disks": ["/", "/data"]`)
	m.disk = func(p string) (diskUsage, error) {
		if p == "/data" {
			return diskUsage{total: 100, used: 90, free: 10}, nil
		}
		return diskUsage{}, os.ErrPermission
	}
	fs := m.evaluate()
	want := map[string]string{
		"swap_used":  "swap 用量 900K / 1000K",
		"mem":        "可用内存只剩 48.8M（5%）",
		"disk:/":     "磁盘 / 无法读取",
		"disk:/data": "磁盘 /data 已用 90%（剩 10B）",
	}
	if len(fs) != len(want) {
		t.Fatalf("findings = %+v", fs)
	}
	for _, f := range fs {
		if !strings.HasPrefix(f.Msg, want[f.Key]) {
			t.Errorf("%s: %q, want prefix %q", f.Key, f.Msg, want[f.Key])
		}
	}
}

func TestStatusAndTop(t *testing.T) {
	m := newModule(t, "testdata/proc", `, "net_interface": "eth0"`)
	m.vnstat = func(_ context.Context, iface string, now time.Time) *netUsage {
		return &netUsage{iface: iface, since: now.Add(-5 * 24 * time.Hour), h24: traffic{1 << 20, 2 << 20}, d30: traffic{3 << 30, 4 << 30}}
	}
	msg, err := m.cmdStatus(context.Background(), bot.Request{Received: time.Now().Add(-2 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"运行 1 天 2 小时", "负载  0.20 / 0.14 / 0.10", "CPU   1分钟 采样中", "可用 488.3M",
		"PSI   CPU 1.2%，内存 2.5%", "磁盘  / 40%（剩 60.0G）", "24h ↓1.0M ↑2.0M", "30天 ↓3.0G ↑4.0G（统计自",
		"ms（Go：收到指令→生成回复）"} {
		if !strings.Contains(msg.Pre, want) {
			t.Errorf("status lacks %q:\n%s", want, msg.Pre)
		}
	}
	top, err := m.cmdTop(context.Background(), bot.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(top.Pre, "按内存\ntuwunel") || !strings.Contains(top.Pre, "my (odd) proc") {
		t.Errorf("top:\n%s", top.Pre)
	}
}

func TestLatency(t *testing.T) {
	for d, want := range map[time.Duration]string{
		830 * time.Microsecond:   "830µs",
		12400 * time.Microsecond: "12.4ms",
		1250 * time.Millisecond:  "1.25s",
	} {
		if got := latency(d); got != want {
			t.Errorf("latency(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestVnstat(t *testing.T) {
	f, err := os.Open("testdata/vnstat.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(f)
	// "now" is 2026-11-13 12:00 UTC; fixture entries are UTC timestamps
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	u := parseVnstat(raw, "eth0", now)
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.h24 != (traffic{300, 30}) { // hours within 24 h: 200 + 100 (the 25 h old one is out)
		t.Errorf("h24 = %+v", u.h24)
	}
	if u.d30 != (traffic{3000, 300}) || u.month != (traffic{5000, 500}) {
		t.Errorf("d30 = %+v month = %+v", u.d30, u.month)
	}
	if len(u.days) != 3 || u.days[2].day.Format("01-02") != "11-13" {
		t.Errorf("days = %+v", u.days)
	}
	if parseVnstat(raw, "wg0", now) != nil || parseVnstat([]byte("not json"), "eth0", now) != nil {
		t.Error("unknown interface or junk must give nil")
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	_, err := New(bot.Env{Log: slog.New(slog.DiscardHandler)}, json.RawMessage(`{"thresholds": {"cpu": 1}}`))
	if err == nil {
		t.Fatal("unknown threshold accepted")
	}
	if _, err := New(bot.Env{Log: slog.New(slog.DiscardHandler)}, nil); err != nil {
		t.Fatalf("empty config: %v", err)
	}
}
