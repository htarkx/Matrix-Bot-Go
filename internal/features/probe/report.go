package probe

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// weekStart is Monday 00:00 local of the week holding t.
func (m *Module) weekStart(t time.Time) time.Time {
	t = t.In(m.loc)
	d := (int(t.Weekday()) + 6) % 7 // days since Monday
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, m.loc)
}

// monthStart is the 1st 00:00 local of the month holding t.
func (m *Module) monthStart(t time.Time) time.Time {
	t = t.In(m.loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, m.loc)
}

// due is the moment a period ending at end may be reported.
func (m *Module) due(end time.Time) time.Time {
	return time.Date(end.Year(), end.Month(), end.Day(), m.reportHour, 0, 0, 0, m.loc)
}

// initReports marks the periods that ended before there was any history as
// reported: a fresh start sends its first report after a period it saw.
func (m *Module) initReports(st *state, now time.Time) {
	if st.LastWeekly == 0 {
		st.LastWeekly = m.weekStart(now).Unix()
	}
	if st.LastMonthly == 0 {
		st.LastMonthly = m.monthStart(now).Unix()
	}
}

// reportIfDue sends the weekly and the monthly report once their period has
// ended and the report hour has come, including after a downtime of the bot.
func (m *Module) reportIfDue(ctx context.Context) error {
	now := m.now()
	type period struct {
		on         bool
		last       *int64
		start, end time.Time
		title      string
	}
	m.mu.Lock()
	ws, ms := m.weekStart(now), m.monthStart(now)
	ps := []period{
		{m.cfg.WeeklyReport, &m.st.LastWeekly, ws.AddDate(0, 0, -7), ws, "周报"},
		{m.cfg.MonthlyReport, &m.st.LastMonthly, ms.AddDate(0, -1, 0), ms, "月报"},
	}
	m.mu.Unlock()
	var errs []error
	for _, p := range ps {
		m.mu.Lock()
		pending := p.on && *p.last < p.end.Unix() && !now.Before(m.due(p.end))
		m.mu.Unlock()
		if !pending {
			continue
		}
		if err := m.env.Post(ctx, m.report(p.title, p.start, p.end)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.title, err))
			continue // try again next round
		}
		m.mu.Lock()
		*p.last = p.end.Unix()
		m.dirty = true
		m.mu.Unlock()
	}
	if len(errs) > 0 {
		return fmt.Errorf("report: %v", errs)
	}
	return m.saveIfDirty(ctx)
}

// budget is the downtime the SLO allows over d.
func (m *Module) budget(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (100 - m.cfg.SLOPct) / 100)
}

// down is the downtime a window's failed probes stand for.
func (m *Module) down(w Window) time.Duration { return time.Duration(w.Fail) * m.interval }

// pct formats an availability, never rounding up to 100%: "99.98%".
// ASCII only, so the columns of a table line up in any chat font.
func pct(p float64, ok bool) string {
	if !ok {
		return "-"
	}
	if p >= 100 {
		return "100%"
	}
	return fmt.Sprintf("%.2f%%", math.Floor(p*100)/100)
}

func (m *Module) mark(p float64, ok bool) string {
	switch {
	case !ok:
		return "  "
	case p >= m.cfg.SLOPct:
		return "✅"
	default:
		return "❌"
	}
}

func minutes(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Round(time.Minute).Minutes()))
}

// dur is a short ASCII duration for table cells: "0s", "43m", "1h05m".
func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// host is the one machine all targets run on, "" when they are on several.
func (m *Module) host() string {
	h := m.cfg.Targets[0].Host
	for _, t := range m.cfg.Targets {
		if t.Host != h {
			return ""
		}
	}
	return h
}

// label names a target, without its host when all targets share one.
func (m *Module) label(host, name string) string {
	if m.host() != "" {
		return name
	}
	return host + " " + name
}

// seenFrom is "backbone-fra，" when all targets share a host, for titles.
func (m *Module) seenFrom() string {
	if h := m.host(); h != "" {
		return h + "，"
	}
	return ""
}

func (m *Module) clock(unix int64) string { return time.Unix(unix, 0).In(m.loc).Format("01-02 15:04") }

// incidentLine renders "11-14 17:27–17:30 通话服务 HTTP 502（3 分钟）",
// with the host in front when targets are on several machines.
func (m *Module) incidentLine(host, name string, in Incident, now time.Time) string {
	end, span := "进行中", now.Sub(time.Unix(in.Start, 0))
	if in.End != 0 {
		end = time.Unix(in.End, 0).In(m.loc).Format("15:04")
		span = time.Duration(in.End-in.Start) * time.Second
	}
	return fmt.Sprintf("%s–%s %s %s（%s）", m.clock(in.Start), end, m.label(host, name), in.Reason, minutes(span))
}

type tagged struct {
	host, name string
	Incident
}

// incidents of every target in [from, to), newest first.
func (m *Module) incidents(from, to time.Time) []tagged {
	var out []tagged
	for _, t := range m.cfg.Targets {
		for _, in := range m.st.Targets[t.Name].incidentsIn(from, to) {
			out = append(out, tagged{t.Host, t.Name, in})
		}
	}
	slices.SortFunc(out, func(a, b tagged) int { return cmp.Compare(b.Start, a.Start) })
	return out
}

// report renders a period: availability per target against the SLO,
// downtime, latency, incidents and how much of the period was observed.
// Each target takes two lines, its name and then the figures, so the
// figures line up whatever the names' widths in a chat font.
func (m *Module) report(title string, from, to time.Time) message.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	expected := float64(to.Sub(from) / m.interval)
	var rows []string
	coverage := 1.0
	for _, t := range m.cfg.Targets {
		w := m.st.Targets[t.Name].window(from, to)
		p, ok := w.Availability()
		coverage = min(coverage, float64(w.OK+w.Fail)/max(expected, 1))
		figures := fmt.Sprintf("   %s 停机 %s", human.Pad(pct(p, ok), 8), human.Pad(dur(m.down(w)), 6))
		switch {
		case w.OK == 0:
		case t.TCP != "":
			figures += fmt.Sprintf(" 往返 %dms", w.MeanMs())
		default:
			if rtt := w.MeanRTTMs(); rtt > 0 {
				figures += " 往返 " + human.Pad(fmt.Sprintf("%dms", rtt), 6)
			}
			figures += fmt.Sprintf(" 请求 %dms", w.MeanMs())
			if p95 := w.PercentileMs(0.95); p95 > 0 {
				figures += fmt.Sprintf("（P95 ≤%dms）", p95)
			}
		}
		rows = append(rows, m.mark(p, ok)+" "+m.label(t.Host, t.Name), figures)
	}
	ins := m.incidents(from, to)
	if len(ins) > 0 {
		rows = append(rows, "", fmt.Sprintf("中断 %d 次：", len(ins)))
		for i, in := range ins {
			if i == 10 {
				rows = append(rows, fmt.Sprintf("……另有 %d 次", len(ins)-10))
				break
			}
			rows = append(rows, m.incidentLine(in.host, in.name, in.Incident, to))
		}
	}
	last := to.Add(-time.Second).In(m.loc)
	return message.Message{
		Icon:  "📈",
		Title: fmt.Sprintf("可用性%s %s 至 %s", title, from.In(m.loc).Format("01-02"), last.Format("01-02")),
		After: fmt.Sprintf("（%s目标 %.1f%%，%s 观测）", m.seenFrom(), m.cfg.SLOPct, m.env.Host),
		Pre:   strings.Join(rows, "\n"),
		Note: fmt.Sprintf("数据覆盖 %.1f%%（%s 自己断网或重启的时段不计）· 时间为 %s\n%s · 均值，P95 为上界",
			100*min(coverage, 1), m.env.Host, m.loc, latencyNote),
	}
}

// cmdUptime is a table with only ASCII before the last column, so it lines
// up in any chat font; the names (often CJK) come last.
func (m *Module) cmdUptime(context.Context, bot.Request) (message.Message, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.monthStart(now)
	monthLen := ms.AddDate(0, 1, 0).Sub(ms)
	allowed := m.budget(monthLen)
	rows := []string{human.Pad("24h", 9) + human.Pad("7d", 9) + human.Pad("30d", 9) + "本月停机"}
	for _, t := range m.cfg.Targets {
		h := m.st.Targets[t.Name]
		cell := func(d time.Duration) string {
			p, ok := h.window(now.Add(-d), now.Add(time.Hour)).Availability()
			return human.Pad(pct(p, ok), 9)
		}
		used := m.down(h.window(ms, now.Add(time.Hour)))
		state := "✅"
		if used > allowed {
			state = "❌"
		}
		rows = append(rows, cell(24*time.Hour)+cell(7*24*time.Hour)+cell(30*24*time.Hour)+
			human.Pad(dur(used), 8)+state+" "+m.label(t.Host, t.Name))
	}
	ins := m.incidents(now.Add(-30*24*time.Hour), now.Add(time.Hour))
	if len(ins) > 0 {
		rows = append(rows, "", "最近中断：")
		for i, in := range ins {
			if i == 5 {
				break
			}
			rows = append(rows, m.incidentLine(in.host, in.name, in.Incident, now))
		}
	}
	msg := message.Message{Icon: "📈", Title: m.env.Host + " 观测到的可用性",
		After: fmt.Sprintf("（%s目标 %.1f%%，本月停机预算 %s）", m.seenFrom(), m.cfg.SLOPct, minutes(allowed)),
		Pre:   strings.Join(rows, "\n"),
		Note:  "预算 = 本月按目标允许的停机时间；停机按失败的探测轮次计"}
	if m.offline {
		msg.Note += "\n⚠ 最近一轮" + m.env.Host + "自己的外网不通，探测结果暂不计入"
	}
	return msg, nil
}

// Summary implements bot.Summarizer: reachability now and 30-day availability.
func (m *Module) Summary(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.last) == 0 {
		return []string{"探测  进行中"}, nil
	}
	now := m.now()
	var bad []string
	lo, hi, seen := 100.0, 0.0, false
	for _, t := range m.cfg.Targets {
		if r, ok := m.last[t.Name]; ok && r.Err != nil {
			bad = append(bad, t.Host+" "+t.Name)
		}
		if p, ok := m.st.Targets[t.Name].window(now.Add(-30*24*time.Hour), now.Add(time.Hour)).Availability(); ok {
			lo, hi, seen = min(lo, p), max(hi, p), true
		}
	}
	l := fmt.Sprintf("探测  %d/%d 可达", len(m.cfg.Targets)-len(bad), len(m.cfg.Targets))
	if len(bad) > 0 {
		l += "（" + strings.Join(bad, "，") + " 不可达）"
	}
	if m.offline {
		l += "（本机外网不通，暂停计入）"
	}
	lines := []string{l}
	if seen {
		span := pct(lo, true)
		if pct(hi, true) != span {
			span += "–" + pct(hi, true)
		}
		lines = append(lines, fmt.Sprintf("      30 天可用性 %s（目标 %.1f%% %s）", span, m.cfg.SLOPct, m.mark(lo, true)))
	}
	return lines, nil
}
