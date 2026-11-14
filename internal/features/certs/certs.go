// Package certs watches the TLS certificates a host serves: days left, and
// whether the chain still verifies for the name. It connects like a client
// would, so it sees exactly what the web server hands out after a renewal.
package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// Config is the module's JSON configuration.
type Config struct {
	Hosts []string `json:"hosts"` // names to check (SNI and verification)
	Port  int      `json:"port"`  // default 443
	// Connect, when set, is the address dialled for every host (e.g.
	// "127.0.0.1:443" to ask the local web server without DNS).
	Connect  string          `json:"connect"`
	WarnDays int             `json:"warn_days"` // default 14
	Refresh  config.Duration `json:"refresh"`   // default 6h
}

// Result is one host's certificate.
type Result struct {
	Host     string
	NotAfter time.Time
	Issuer   string
	Err      error // dial or verification error
	At       time.Time
}

// Module is the certs module.
type Module struct {
	cfg   Config
	env   bot.Env
	roots *x509.CertPool // nil = system roots
	now   func() time.Time

	mu      sync.Mutex
	results map[string]Result
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if len(c.Hosts) == 0 {
		return nil, errors.New("hosts is required")
	}
	if c.Port == 0 {
		c.Port = 443
	}
	if c.WarnDays == 0 {
		c.WarnDays = 14
	}
	if c.Refresh.Duration == 0 {
		c.Refresh.Duration = 6 * time.Hour
	}
	return &Module{cfg: c, env: env, now: time.Now, results: map[string]Result{}}, nil
}

// Jobs implements bot.Module.
func (m *Module) Jobs() []bot.Job {
	return []bot.Job{{Name: "certs", Every: m.cfg.Refresh.Duration, Delay: 5 * time.Second, Run: m.refresh}}
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{{Name: "certs", Aliases: []string{"tls"}, Help: "TLS 证书到期", Run: m.cmdCerts}}
}

// Checks implements bot.Module: alerts from the last refresh.
func (m *Module) Checks() []bot.Check {
	return []bot.Check{{Name: "certs", Run: func(context.Context) []bot.Finding {
		var out []bot.Finding
		for _, r := range m.sorted() {
			if msg := m.problem(r); msg != "" {
				out = append(out, bot.Finding{Key: "cert:" + r.Host, What: "证书 " + r.Host, Msg: msg})
			}
		}
		return out
	}}}
}

func (m *Module) refresh(ctx context.Context) error {
	var errs []error
	for _, h := range m.cfg.Hosts {
		r := m.check(ctx, h)
		m.mu.Lock()
		m.results[h] = r
		m.mu.Unlock()
		if r.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, r.Err))
		}
	}
	return errors.Join(errs...)
}

// check connects to host and verifies what it serves. The handshake skips
// Go's own verification so that an expired certificate still shows its date.
func (m *Module) check(ctx context.Context, host string) Result {
	r := Result{Host: host, At: m.now()}
	addr := m.cfg.Connect
	if addr == "" {
		addr = net.JoinHostPort(host, fmt.Sprint(m.cfg.Port))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		r.Err = err
		return r
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		r.Err = errors.New("no certificate")
		return r
	}
	leaf := certs[0]
	r.NotAfter, r.Issuer = leaf.NotAfter, leaf.Issuer.CommonName
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, r.Err = leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, Roots: m.roots, CurrentTime: r.At})
	return r
}

func (m *Module) sorted() []Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Result, 0, len(m.results))
	for _, r := range m.results {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Result) int { return a.NotAfter.Compare(b.NotAfter) })
	return out
}

func (m *Module) daysLeft(r Result) int { return int(r.NotAfter.Sub(m.now()).Hours() / 24) }

// problem is what is wrong with a result, "" when it is fine.
func (m *Module) problem(r Result) string {
	switch {
	case r.NotAfter.IsZero():
		return fmt.Sprintf("读取失败：%v", r.Err)
	case r.Err != nil:
		return fmt.Sprintf("校验失败：%v", r.Err)
	case m.daysLeft(r) < m.cfg.WarnDays:
		return fmt.Sprintf("还剩 %d 天到期（续期没有生效？）", m.daysLeft(r))
	}
	return ""
}

// Summary implements bot.Summarizer.
func (m *Module) Summary(context.Context) ([]string, error) {
	rs := m.sorted()
	if len(rs) == 0 {
		return []string{"证书  检查中"}, nil
	}
	var bad []string
	for _, r := range rs {
		if p := m.problem(r); p != "" {
			bad = append(bad, r.Host+" "+p)
		}
	}
	if len(bad) > 0 {
		return []string{"证书  ⚠ " + strings.Join(bad, "；")}, nil
	}
	first := rs[0]
	return []string{fmt.Sprintf("证书  %d 个正常，最早到期 %s 还剩 %d 天", len(rs), first.Host, m.daysLeft(first))}, nil
}

func (m *Module) cmdCerts(context.Context, bot.Request) (message.Message, error) {
	msg := message.Message{Icon: "🔐", Title: m.env.Host + " 证书"}
	rs := m.sorted()
	if len(rs) == 0 {
		msg.After = "：检查中"
		return msg, nil
	}
	var rows []string
	for _, r := range rs {
		state := fmt.Sprintf("%4d 天", m.daysLeft(r))
		switch {
		case r.NotAfter.IsZero():
			state = "读取失败"
		case r.Err != nil:
			state += " ⚠ 校验失败"
		}
		rows = append(rows, fmt.Sprintf("%-20s %s  %s", r.Host, state, r.Issuer))
	}
	msg.Pre = strings.Join(rows, "\n")
	msg.Note = fmt.Sprintf("每 %s检查一次，少于 %d 天告警", human.Duration(m.cfg.Refresh.Duration), m.cfg.WarnDays)
	return msg, nil
}
