// Package probe checks other machines from the outside: an HTTP(S) URL must
// answer with an expected status, a TCP port must accept a connection. Run
// on a second host it is the watcher of the first: when the host that runs
// the main bot goes down, this one still alerts (over Telegram, typically).
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
	// Timeout per probe (default 10s).
	Timeout config.Duration `json:"timeout"`
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

// Result is a target's last probe.
type Result struct {
	At      time.Time
	Err     error
	Latency time.Duration
}

// Module is the probe module.
type Module struct {
	bot.NoJobs
	cfg  Config
	env  bot.Env
	http *http.Client
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	now  func() time.Time

	mu   sync.Mutex
	last map[string]Result
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
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
	seen := map[string]bool{}
	var errs []error
	for i := range c.Targets {
		t := &c.Targets[i]
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("targets[%d]: name is required", i))
		case t.Host == "":
			errs = append(errs, fmt.Errorf("target %q: host (the machine it runs on) is required", t.Name))
		case seen[t.Name]:
			errs = append(errs, fmt.Errorf("targets[%d]: duplicate name %q", i, t.Name))
		case (t.HTTP == "") == (t.TCP == ""):
			errs = append(errs, fmt.Errorf("target %q: set exactly one of http and tcp", t.Name))
		case t.HTTP != "" && !strings.HasPrefix(t.HTTP, "http://") && !strings.HasPrefix(t.HTTP, "https://"):
			errs = append(errs, fmt.Errorf("target %q: http must be an http(s) URL", t.Name))
		case t.TCP != "":
			if _, _, err := net.SplitHostPort(t.TCP); err != nil {
				errs = append(errs, fmt.Errorf("target %q: tcp: %w", t.Name, err))
			}
		}
		seen[t.Name] = true
		if t.HTTP != "" && t.Expect == 0 {
			t.Expect = http.StatusOK
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	d := &net.Dialer{}
	return &Module{
		cfg: c, env: env, dial: d.DialContext, now: time.Now, last: map[string]Result{},
		http: &http.Client{
			// The status of the first answer is the check: a 302 to a login
			// page means something else than a 200.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		},
	}, nil
}

// Checks implements bot.Module: probes every target at once on every
// evaluation; the alert engine's sustain absorbs a single lost probe.
func (m *Module) Checks() []bot.Check {
	return []bot.Check{{Name: "probe", Run: func(ctx context.Context) []bot.Finding {
		m.runAll(ctx)
		var out []bot.Finding
		for _, t := range m.cfg.Targets {
			if r := m.result(t.Name); r.Err != nil {
				out = append(out, bot.Finding{Key: "probe:" + t.Name, Subject: t.Host, What: t.Name,
					Msg: fmt.Sprintf("无法访问（%v）", r.Err)})
			}
		}
		return out
	}}}
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{{Name: "probe", Aliases: []string{"p"}, Help: "外部探测：其他机器是否可达", Run: m.cmdProbe}}
}

func (m *Module) result(name string) Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last[name]
}

// runAll probes every target concurrently and stores the results.
func (m *Module) runAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range m.cfg.Targets {
		wg.Go(func() {
			start := m.now()
			err := m.probe(ctx, t)
			r := Result{At: start, Err: err, Latency: m.now().Sub(start)}
			m.mu.Lock()
			m.last[t.Name] = r
			m.mu.Unlock()
		})
	}
	wg.Wait()
}

func (m *Module) probe(ctx context.Context, t Target) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout.Duration)
	defer cancel()
	if t.TCP != "" {
		c, err := m.dial(ctx, "tcp", t.TCP)
		if err != nil {
			return short(err)
		}
		return c.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.HTTP, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Matrix-Bot-Go probe")
	resp, err := m.http.Do(req)
	if err != nil {
		return short(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != t.Expect {
		return fmt.Errorf("HTTP %d（应为 %d）", resp.StatusCode, t.Expect)
	}
	return nil
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

func (m *Module) counts() (ok, probed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.cfg.Targets {
		if r, done := m.last[t.Name]; done {
			probed++
			if r.Err == nil {
				ok++
			}
		}
	}
	return ok, probed
}

// Summary implements bot.Summarizer.
func (m *Module) Summary(context.Context) ([]string, error) {
	ok, probed := m.counts()
	if probed == 0 {
		return []string{"探测  进行中"}, nil
	}
	l := fmt.Sprintf("探测  %d/%d 可达", ok, len(m.cfg.Targets))
	if ok < len(m.cfg.Targets) {
		var bad []string
		for _, t := range m.cfg.Targets {
			if r := m.result(t.Name); r.Err != nil {
				bad = append(bad, t.Host+" "+t.Name)
			}
		}
		l += "（" + strings.Join(bad, "，") + " 不可达）"
	}
	return []string{l}, nil
}

func (m *Module) cmdProbe(ctx context.Context, _ bot.Request) (message.Message, error) {
	m.runAll(ctx) // a fresh look, not the last evaluation's
	var rows []string
	for _, t := range m.cfg.Targets {
		r := m.result(t.Name)
		state := fmt.Sprintf("✅ %dms", r.Latency.Milliseconds())
		if r.Err != nil {
			state = "❌ " + r.Err.Error()
		}
		what := t.TCP
		if t.HTTP != "" {
			what = t.HTTP
		}
		rows = append(rows, t.Host+" "+t.Name+"  "+state, "  "+what)
	}
	return message.Message{Icon: "📡", Title: m.env.Host + " 外部探测", After: "（从这台机器看别的机器）", Pre: strings.Join(rows, "\n")}, nil
}
