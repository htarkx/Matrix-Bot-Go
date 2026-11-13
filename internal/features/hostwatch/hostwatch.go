// Package hostwatch watches the local host: CPU, swap activity, memory and
// disk alerts, plus the status, top and net commands.
package hostwatch

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// Config is the module's JSON configuration.
type Config struct {
	Proc         string   `json:"proc"`          // default /proc
	Disks        []string `json:"disks"`         // default ["/"]
	NetInterface string   `json:"net_interface"` // vnstat interface; empty = no traffic lines
	// NetRefresh is how often vnstat is read in the background (default 1m).
	// Commands always answer from the last reading: vnstat itself only saves
	// every 5 minutes, so a reading is a few minutes old at worst.
	NetRefresh config.Duration `json:"net_refresh"`
	Thresholds Thresholds      `json:"thresholds"`
}

// Thresholds of the alerts. Zero values take the defaults.
type Thresholds struct {
	CPUBusyPct      float64         `json:"cpu_busy_pct"` // busy incl. steal, averaged over CPUWindow
	CPUWindow       config.Duration `json:"cpu_window"`
	SwapIOPagesPerS float64         `json:"swap_io_pages_per_s"` // pswpin+pswpout averaged over SwapWindow
	SwapWindow      config.Duration `json:"swap_window"`
	SwapUsedPct     float64         `json:"swap_used_pct"`
	MemAvailablePct float64         `json:"mem_available_pct"`
	DiskUsedPct     float64         `json:"disk_used_pct"`
}

func (t *Thresholds) applyDefaults() {
	set := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	set(&t.CPUBusyPct, 85)
	set(&t.SwapIOPagesPerS, 25) // ~100 KiB/s of 4 KiB pages
	set(&t.SwapUsedPct, 50)
	set(&t.MemAvailablePct, 10)
	set(&t.DiskUsedPct, 85)
	if t.CPUWindow.Duration == 0 {
		t.CPUWindow.Duration = 10 * time.Minute
	}
	if t.SwapWindow.Duration == 0 {
		t.SwapWindow.Duration = 5 * time.Minute
	}
}

// Module is the hostwatch module.
type Module struct {
	cfg    Config
	proc   procfs
	env    bot.Env
	vnstat func(ctx context.Context, iface string, now time.Time) *netUsage
	disk   func(path string) (diskUsage, error)

	net atomic.Pointer[netUsage] // last good vnstat reading; nil before the first

	mu   sync.Mutex // the check samples while commands read
	cpu  *window[cpuSample]
	swap *window[uint64]
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	var c Config
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
	}
	if c.Proc == "" {
		c.Proc = "/proc"
	}
	if len(c.Disks) == 0 {
		c.Disks = []string{"/"}
	}
	if c.NetRefresh.Duration == 0 {
		c.NetRefresh.Duration = time.Minute
	}
	c.Thresholds.applyDefaults()
	return &Module{
		cfg: c, proc: procfs(c.Proc), env: env,
		vnstat: readVnstat, disk: statDisk,
		cpu: newWindow[cpuSample](time.Hour + 2*time.Minute), swap: newWindow[uint64](time.Hour + 2*time.Minute),
	}, nil
}

// Jobs implements bot.Module: the background vnstat reader.
func (m *Module) Jobs() []bot.Job {
	if m.cfg.NetInterface == "" {
		return nil
	}
	return []bot.Job{{Name: "vnstat", Every: m.cfg.NetRefresh.Duration, Run: m.refreshNet}}
}

// refreshNet stores a fresh vnstat reading. A failed read keeps the last
// good one: stale traffic numbers beat none.
func (m *Module) refreshNet(ctx context.Context) error {
	n := m.vnstat(ctx, m.cfg.NetInterface, time.Now())
	if n == nil {
		return fmt.Errorf("no vnstat data for %s", m.cfg.NetInterface)
	}
	m.net.Store(n)
	return nil
}

// Checks implements bot.Module.
func (m *Module) Checks() []bot.Check {
	return []bot.Check{{Name: "hostwatch", Run: func(ctx context.Context) []bot.Finding {
		now := time.Now()
		if err := m.sample(now); err != nil {
			m.env.Log.Warn("sampling failed", "err", err)
		}
		return m.evaluate()
	}}}
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{
		{Name: "status", Aliases: []string{"s"}, Help: "负载、CPU、内存、swap、磁盘、网络", Run: m.cmdStatus},
		{Name: "top", Aliases: []string{"t"}, Help: "占用最高的进程", Run: m.cmdTop},
		{Name: "net", Aliases: []string{"n"}, Help: "网络流量：24 小时、30 天、每天", Run: m.cmdNet},
	}
}

func (m *Module) sample(now time.Time) error {
	c, err := m.proc.cpu()
	if err != nil {
		return err
	}
	s, err := m.proc.swapPages()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cpu.add(now, c)
	m.swap.add(now, s)
	return nil
}

// cpuAvg is busy% and steal% over about d; ok false while sampling.
func (m *Module) cpuAvg(d time.Duration) (busy, steal float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, b, ok := m.cpu.span(d)
	if !ok || b.v.total <= a.v.total {
		return 0, 0, false
	}
	dt := float64(b.v.total - a.v.total)
	return 100 * (dt - float64(b.v.idle-a.v.idle)) / dt, 100 * float64(b.v.steal-a.v.steal) / dt, true
}

// swapRate is swap pages in+out per second over about d.
func (m *Module) swapRate(d time.Duration) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, b, ok := m.swap.span(d)
	if !ok {
		return 0, false
	}
	return float64(b.v-a.v) / max(b.at.Sub(a.at).Seconds(), 1), true
}

func (m *Module) evaluate() []bot.Finding {
	th := m.cfg.Thresholds
	var out []bot.Finding
	bad := func(key, format string, a ...any) {
		out = append(out, bot.Finding{Key: key, Msg: fmt.Sprintf(format, a...)})
	}

	if busy, steal, ok := m.cpuAvg(th.CPUWindow.Duration); ok && busy >= th.CPUBusyPct {
		bad("cpu", "CPU 持续高负载：%d 分钟平均 %.0f%%（steal %.0f%%）", int(th.CPUWindow.Minutes()), busy, steal)
	}
	if r, ok := m.swapRate(th.SwapWindow.Duration); ok && r >= th.SwapIOPagesPerS {
		bad("swap_io", "正在频繁使用 swap：%d 分钟平均换入换出 %.0f 页/秒（约 %s/s）",
			int(th.SwapWindow.Minutes()), r, human.Bytes(r*4096))
	}
	if mem, err := m.proc.meminfo(); err != nil {
		m.env.Log.Warn("meminfo", "err", err)
	} else {
		if total := mem["SwapTotal"]; total > 0 {
			used := total - mem["SwapFree"]
			if 100*float64(used)/float64(total) >= th.SwapUsedPct {
				bad("swap_used", "swap 用量 %s / %s", human.Bytes(used), human.Bytes(total))
			}
		}
		if total := mem["MemTotal"]; total > 0 {
			pct := 100 * float64(mem["MemAvailable"]) / float64(total)
			if pct <= th.MemAvailablePct {
				bad("mem", "可用内存只剩 %s（%.0f%%）", human.Bytes(mem["MemAvailable"]), pct)
			}
		}
	}
	for _, p := range m.cfg.Disks {
		du, err := m.disk(p)
		if err != nil {
			bad("disk:"+p, "磁盘 %s 无法读取：%v", p, err)
			continue
		}
		if du.pct() >= th.DiskUsedPct {
			bad("disk:"+p, "磁盘 %s 已用 %.0f%%（剩 %s）", p, du.pct(), human.Bytes(du.free))
		}
	}
	return out
}

func (m *Module) title(icon, what string) message.Message {
	return message.Message{Icon: icon, Title: "[" + m.env.Host + "] " + what}
}

func (m *Module) cmdStatus(ctx context.Context, req bot.Request) (message.Message, error) {
	mem, err := m.proc.meminfo()
	if err != nil {
		return message.Message{}, err
	}
	load, err := m.proc.loadavg()
	if err != nil {
		return message.Message{}, err
	}
	up, err := m.proc.uptime()
	if err != nil {
		return message.Message{}, err
	}
	pct := func(v float64, ok bool) string {
		if !ok {
			return "采样中"
		}
		return fmt.Sprintf("%.0f%%", v)
	}
	c1, _, ok1 := m.cpuAvg(time.Minute)
	c10, _, ok10 := m.cpuAvg(10 * time.Minute)
	swapIO := "采样中"
	if r, ok := m.swapRate(5 * time.Minute); ok {
		swapIO = fmt.Sprintf("%.1f 页/秒", r)
	}
	pc, pm := m.proc.psi("cpu"), m.proc.psi("memory")
	lines := []string{
		fmt.Sprintf("运行 %s，%d 核", human.Duration(time.Duration(up*float64(time.Second))), runtime.NumCPU()),
		fmt.Sprintf("负载  %.2f / %.2f / %.2f", load[0], load[1], load[2]),
		fmt.Sprintf("CPU   1分钟 %s，10分钟 %s", pct(c1, ok1), pct(c10, ok10)),
		fmt.Sprintf("内存  已用 %s / %s，可用 %s", human.Bytes(mem["MemTotal"]-mem["MemAvailable"]),
			human.Bytes(mem["MemTotal"]), human.Bytes(mem["MemAvailable"])),
		fmt.Sprintf("swap  已用 %s / %s，换入换出 %s", human.Bytes(mem["SwapTotal"]-mem["SwapFree"]),
			human.Bytes(mem["SwapTotal"]), swapIO),
		fmt.Sprintf("PSI   CPU %.1f%%，内存 %.1f%%（avg60）", pc["avg60"], pm["avg60"]),
	}
	for _, p := range m.cfg.Disks {
		if du, err := m.disk(p); err == nil {
			lines = append(lines, fmt.Sprintf("磁盘  %s %.0f%%（剩 %s）", p, du.pct(), human.Bytes(du.free)))
		}
	}
	if m.cfg.NetInterface != "" {
		if n := m.net.Load(); n != nil {
			lines = append(lines,
				"网络  24h "+n.h24.String(),
				"      30天 "+n.d30.String()+coverage(n, 30))
		}
	}
	msg := m.title("📊", "状态")
	if n := len(m.env.Alerts.Active()); n > 0 {
		msg.After = fmt.Sprintf("，%d 条告警中", n)
	}
	if !req.Sent.IsZero() && !req.Received.IsZero() {
		// origin_server_ts has millisecond precision, so tiny values can
		// come out negative.
		d := max(req.Received.Sub(req.Sent), 0)
		lines = append(lines, "投递  "+latency(d)+"（Tuwunel 收到指令→bot 拿到，ms 精度）")
	}
	// Last, so it covers everything above: parsing, /proc, formatting.
	if !req.Received.IsZero() {
		lines = append(lines, "处理  "+latency(time.Since(req.Received))+"（Go：收到指令→生成回复）")
	}
	msg.Pre = strings.Join(lines, "\n")
	return msg, nil
}

// latency formats a short duration: 830µs, 12.4ms, 1.25s.
func latency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func (t traffic) String() string { return "↓" + human.Bytes(t.rx) + " ↑" + human.Bytes(t.tx) }

// coverage notes when vnstat has less history than the window shown.
func coverage(n *netUsage, days int) string {
	if time.Since(n.since) < time.Duration(days)*24*time.Hour {
		return "（统计自 " + n.since.Local().Format("01-02 15:04") + "）"
	}
	return ""
}

func (m *Module) cmdTop(ctx context.Context, _ bot.Request) (message.Message, error) {
	a := m.proc.procs()
	select {
	case <-ctx.Done():
		return message.Message{}, ctx.Err()
	case <-time.After(time.Second):
	}
	b := m.proc.procs()
	type row struct {
		comm string
		cpu  float64
		rss  uint64
	}
	rows := make([]row, 0, len(b))
	for pid, p := range b {
		r := row{comm: p.comm, rss: p.rss}
		if old, ok := a[pid]; ok && p.ticks >= old.ticks {
			r.cpu = 100 * float64(p.ticks-old.ticks) / clockTicks
		}
		rows = append(rows, r)
	}
	table := func(less func(x, y row) int) string {
		sorted := slices.Clone(rows)
		slices.SortStableFunc(sorted, less)
		var lines []string
		for _, r := range sorted[:min(8, len(sorted))] {
			comm := []rune(r.comm)
			lines = append(lines, fmt.Sprintf("%-15s %5.1f%% %7s", string(comm[:min(15, len(comm))]), r.cpu, human.Bytes(r.rss)))
		}
		return strings.Join(lines, "\n")
	}
	byCPU := table(func(x, y row) int { return cmp.Compare(y.cpu, x.cpu) })
	byMem := table(func(x, y row) int { return cmp.Compare(y.rss, x.rss) })
	msg := m.title("🔝", "进程")
	msg.After = "（CPU 为最近 1 秒）"
	msg.Pre = "按 CPU\n" + byCPU + "\n\n按内存\n" + byMem
	return msg, nil
}

func (m *Module) cmdNet(_ context.Context, _ bot.Request) (message.Message, error) {
	if m.cfg.NetInterface == "" {
		return message.Message{Lines: []string{"[" + m.env.Host + "] 没有配置 net_interface"}}, nil
	}
	n := m.net.Load()
	if n == nil {
		return message.Message{Lines: []string{fmt.Sprintf("[%s] 没有流量数据（vnstat 未安装或尚未记录 %s）",
			m.env.Host, m.cfg.NetInterface)}}, nil
	}
	lines := []string{
		"过去 24 小时  " + n.h24.String(),
		"过去 30 天    " + n.d30.String(),
		"本月          " + n.month.String(),
		"",
		"最近每天：",
	}
	for _, d := range n.days {
		lines = append(lines, d.day.Format("01-02")+"  "+d.traffic.String())
	}
	msg := m.title("🌐", "网络 "+n.iface)
	msg.After = coverage(n, 30)
	msg.Pre = strings.Join(lines, "\n")
	return msg, nil
}
