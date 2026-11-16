// Package probe watches other machines from the outside and keeps their
// availability: an HTTP(S) URL must answer with an expected status, a TCP
// port must accept a connection. Every check round is recorded, so the
// module can tell "how many nines" over a day, a week, a month, and send
// weekly and monthly reports against a target (SLO).
//
// Run on a second host it is the watcher of the first: when the host that
// runs the main bot goes down, this one still alerts. Control targets (well
// known outside services) tell this machine losing its own connection apart
// from the targets going down: such rounds are neither recorded nor alerted.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// Config is the module's JSON configuration.
type Config struct {
	Targets []Target `json:"targets"`
	// Controls are well-known outside services (host is not needed). When
	// every control fails as well, this machine is offline, not the targets.
	Controls []Target `json:"controls"`
	// Timeout per probe (default 10s).
	Timeout config.Duration `json:"timeout"`
	// SLOPct is the availability target in percent (default 99.9).
	SLOPct float64 `json:"slo_pct"`
	// Timezone of report periods and times (default UTC), e.g. Europe/Zurich.
	Timezone string `json:"timezone"`
	// WeeklyReport covers Monday to Monday, MonthlyReport the calendar month;
	// both are sent at ReportHour (default 9) local time after the period.
	WeeklyReport  bool `json:"weekly_report"`
	MonthlyReport bool `json:"monthly_report"`
	ReportHour    *int `json:"report_hour"`
}

// Target is one thing to reach. Exactly one of HTTP and TCP is set.
type Target struct {
	// Host is the machine the target runs on: the subject of its alerts
	// ("backbone-fra：通话服务 无法访问"). Name is the service on it.
	Host string `json:"host"`
	Name string `json:"name"`
	// HTTP is a URL fetched with GET (redirects are not followed, TLS is
	// verified); the answer must have status Expect (default 200).
	HTTP   string `json:"http"`
	Expect int    `json:"expect"`
	// TCP is host:port that must accept a connection.
	TCP string `json:"tcp"`
}

// Result is a probe's outcome. Latency is the whole probe; RTT is its TCP
// connect alone (one round trip, about what ping shows). For a TCP target
// both are the same; an HTTPS probe takes about three round trips (TCP,
// TLS, the request) plus the server's own time.
type Result struct {
	At      time.Time
	Err     error
	Latency time.Duration
	RTT     time.Duration // 0 when no connection was made
}

// Module is the probe module.
type Module struct {
	cfg        Config
	env        bot.Env
	loc        *time.Location
	reportHour int
	interval   time.Duration
	path       string // state file, "" = memory only
	http       *http.Client
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
	now        func() time.Time

	mu      sync.Mutex
	last    map[string]Result // targets, last recorded round
	held    []bot.Finding     // findings of the last recorded round
	offline bool              // last round: every control failed
	st      *state
	dirty   bool
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	return newWithClock(env, raw, time.Now)
}

func newWithClock(env bot.Env, raw json.RawMessage, now func() time.Time) (*Module, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if len(c.Targets) == 0 {
		return nil, errors.New("targets is required")
	}
	if c.Timeout.Duration == 0 {
		c.Timeout.Duration = 10 * time.Second
	}
	if c.SLOPct == 0 {
		c.SLOPct = 99.9
	}
	if c.SLOPct <= 0 || c.SLOPct >= 100 {
		return nil, fmt.Errorf("slo_pct %v must be between 0 and 100", c.SLOPct)
	}
	loc := time.UTC
	if c.Timezone != "" {
		l, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone: %w", err)
		}
		loc = l
	}
	hour := 9
	if c.ReportHour != nil {
		hour = *c.ReportHour
	}
	if hour < 0 || hour > 23 {
		return nil, fmt.Errorf("report_hour %d is not an hour", hour)
	}
	seen := map[string]bool{}
	var errs []error
	check := func(what string, i int, t *Target, needHost bool) {
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("%s[%d]: name is required", what, i))
		case needHost && t.Host == "":
			errs = append(errs, fmt.Errorf("target %q: host (the machine it runs on) is required", t.Name))
		case seen[t.Name]:
			errs = append(errs, fmt.Errorf("%s[%d]: duplicate name %q", what, i, t.Name))
		case (t.HTTP == "") == (t.TCP == ""):
			errs = append(errs, fmt.Errorf("%s %q: set exactly one of http and tcp", what, t.Name))
		case t.HTTP != "" && !strings.HasPrefix(t.HTTP, "http://") && !strings.HasPrefix(t.HTTP, "https://"):
			errs = append(errs, fmt.Errorf("%s %q: http must be an http(s) URL", what, t.Name))
		case t.TCP != "":
			if _, _, err := net.SplitHostPort(t.TCP); err != nil {
				errs = append(errs, fmt.Errorf("%s %q: tcp: %w", what, t.Name, err))
			}
		}
		seen[t.Name] = true
		if t.HTTP != "" && t.Expect == 0 {
			t.Expect = http.StatusOK
		}
	}
	for i := range c.Targets {
		check("targets", i, &c.Targets[i], true)
	}
	for i := range c.Controls {
		check("controls", i, &c.Controls[i], false)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	m := &Module{
		cfg: c, env: env, loc: loc, reportHour: hour, interval: env.Interval,
		now: now, last: map[string]Result{},
		http: &http.Client{
			// The status of the first answer is the check: a 302 to a login
			// page means something else than a 200.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		},
	}
	if m.interval <= 0 {
		m.interval = 30 * time.Second
	}
	m.dial = (&net.Dialer{}).DialContext
	if env.StateDir != "" {
		m.path = filepath.Join(env.StateDir, "probe.json")
	}
	m.st = m.loadState()
	return m, nil
}

// loadState reads the history, setting a damaged file aside rather than
// refusing to start: monitoring matters more than its past.
func (m *Module) loadState() *state {
	st := newState()
	if m.path != "" {
		s, err := load(m.path)
		if err == nil {
			st = s
		} else {
			aside := m.path + ".broken-" + m.now().UTC().Format("20060102T150405Z")
			m.logWarn("probe history unreadable, starting a new one", "err", err, "kept", aside)
			os.Rename(m.path, aside)
		}
	}
	for name := range st.Targets {
		if !slices.ContainsFunc(m.cfg.Targets, func(t Target) bool { return t.Name == name }) {
			delete(st.Targets, name) // target removed from the configuration
		}
	}
	m.initReports(st, m.now())
	return st
}

func (m *Module) logWarn(msg string, args ...any) {
	if m.env.Log != nil {
		m.env.Log.Warn(msg, args...)
	}
}

// Checks implements bot.Module: probes every target (and the controls) at
// once on every evaluation; the alert engine's sustain absorbs a single
// lost probe.
func (m *Module) Checks() []bot.Check {
	return []bot.Check{{Name: "probe", Run: m.check}}
}

func (m *Module) check(ctx context.Context) []bot.Finding {
	all := append(slices.Clone(m.cfg.Targets), m.cfg.Controls...)
	res := m.probeAll(ctx, all)
	if ctx.Err() != nil {
		return nil // shutting down: these failures are ours
	}
	offline := len(m.cfg.Controls) > 0
	for _, c := range m.cfg.Controls {
		if res[c.Name].Err == nil {
			offline = false
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.offline = offline
	if offline {
		// This machine lost its own connection: what it sees of the targets
		// says nothing about them. Record nothing, keep the alerts as they
		// were (no false "down", no false "recovered").
		return append(slices.Clone(m.held), bot.Finding{
			Key: "self:probe-offline", What: "外网连接",
			Msg: "不通（对照目标全部失败），探测结果暂不计入",
		})
	}
	var out []bot.Finding
	for _, t := range m.cfg.Targets {
		r := res[t.Name]
		m.last[t.Name] = r
		m.st.record(t.Name, r.At, r.Err, r.Latency, r.RTT)
		if r.Err != nil {
			out = append(out, bot.Finding{Key: "probe:" + t.Name, Subject: t.Host, What: t.Name,
				Msg: fmt.Sprintf("无法访问（%v）", r.Err)})
		}
	}
	m.dirty = true
	m.held = out
	return out
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{
		{Name: "probe", Aliases: []string{"p"}, Help: "外部探测：现在能不能访问", Run: m.cmdProbe},
		{Name: "uptime", Aliases: []string{"up"}, Help: "可用性：24 小时 / 7 天 / 30 天，本月预算，最近中断", Run: m.cmdUptime},
	}
}

// Jobs implements bot.Module: save the history now and then, and send the
// reports when due.
func (m *Module) Jobs() []bot.Job {
	var jobs []bot.Job
	if m.path != "" {
		jobs = append(jobs, bot.Job{Name: "probe-save", Every: 5 * time.Minute, Delay: 5 * time.Minute, Run: m.saveIfDirty, SideEffects: true})
	}
	if m.cfg.WeeklyReport || m.cfg.MonthlyReport {
		jobs = append(jobs, bot.Job{Name: "probe-report", Every: 10 * time.Minute, Delay: time.Minute, Run: m.reportIfDue, SideEffects: true})
	}
	return jobs
}

// Stop implements bot.Stopper: keep what was recorded since the last save.
func (m *Module) Stop(ctx context.Context) {
	if err := m.saveIfDirty(ctx); err != nil {
		m.logWarn("probe history not saved", "err", err)
	}
}

func (m *Module) saveIfDirty(context.Context) error {
	if m.path == "" {
		return nil
	}
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(m.st) // snapshot under the lock, write outside it
	m.dirty = false
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFile(m.path, raw)
}

// probeAll probes targets concurrently.
func (m *Module) probeAll(ctx context.Context, ts []Target) map[string]Result {
	out := make(map[string]Result, len(ts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Go(func() {
			start := m.now()
			rtt, err := m.probe(ctx, t)
			r := Result{At: start, Err: err, Latency: m.now().Sub(start), RTT: rtt}
			mu.Lock()
			out[t.Name] = r
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// probe reaches t once and returns how long its TCP connect took.
func (m *Module) probe(ctx context.Context, t Target) (rtt time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout.Duration)
	defer cancel()
	if t.TCP != "" {
		start := time.Now()
		c, err := m.dial(ctx, "tcp", t.TCP)
		if err != nil {
			return 0, short(err)
		}
		rtt = time.Since(start)
		return rtt, c.Close()
	}
	// Keep-alives are off, so every probe connects anew; with several
	// addresses (IPv4 and IPv6) the connection that is used counts.
	var mu sync.Mutex
	started := map[string]time.Time{}
	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, addr string) {
			mu.Lock()
			started[addr] = time.Now()
			mu.Unlock()
		},
		ConnectDone: func(_, addr string, err error) {
			mu.Lock()
			if err == nil && rtt == 0 {
				rtt = time.Since(started[addr])
			}
			mu.Unlock()
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, t.HTTP, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Matrix-Bot-Go probe")
	resp, err := m.http.Do(req)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		return rtt, short(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != t.Expect {
		return rtt, fmt.Errorf("HTTP %d（应为 %d）", resp.StatusCode, t.Expect)
	}
	return rtt, nil
}

// short trims Go's nested network errors to their gist for a chat line.
func short(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return errors.New("超时")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("超时")
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err
	}
	return err
}

func (m *Module) cmdProbe(ctx context.Context, _ bot.Request) (message.Message, error) {
	// A fresh look, not recorded: history only counts the regular rounds.
	res := m.probeAll(ctx, append(slices.Clone(m.cfg.Targets), m.cfg.Controls...))
	var rows []string
	line := func(label string, t Target) {
		r := res[t.Name]
		state := "✅ " + latencyText(t, r.RTT, r.Latency)
		if r.Err != nil {
			state = "❌ " + r.Err.Error()
		}
		what := t.TCP
		if t.HTTP != "" {
			what = t.HTTP
		}
		rows = append(rows, label+"  "+state, "  "+what)
	}
	for _, t := range m.cfg.Targets {
		line(t.Host+" "+t.Name, t)
	}
	for _, c := range m.cfg.Controls {
		line("对照 "+c.Name, c)
	}
	return message.Message{Icon: "📡", Title: m.env.Host + " 外部探测", After: "（从这台机器看别的机器）",
		Pre: strings.Join(rows, "\n"), Note: latencyNote}, nil
}

// latencyNote explains why a request takes several times the round trip.
const latencyNote = "往返 = TCP 建连耗时，约等于 ping 的 RTT，两个方向一样；" +
	"请求 = 一次完整的 HTTPS 请求（建连 + TLS 握手 + HTTP，约 3 个往返，再加服务端处理）"

// latencyText is "往返 88ms，请求 325ms" (HTTP) or "往返 88ms" (TCP).
func latencyText(t Target, rtt, total time.Duration) string {
	if t.TCP != "" {
		return fmt.Sprintf("往返 %dms", total.Milliseconds())
	}
	if rtt == 0 {
		return fmt.Sprintf("请求 %dms", total.Milliseconds())
	}
	return fmt.Sprintf("往返 %dms，请求 %dms", rtt.Milliseconds(), total.Milliseconds())
}
