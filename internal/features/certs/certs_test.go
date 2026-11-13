package certs

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

// tlsServer is an httptest TLS server that does not log the handshakes the
// checker cuts short.
func tlsServer() *httptest.Server {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	return srv
}

func newModule(t *testing.T, srv *httptest.Server, warnDays int) *Module {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"hosts":     []string{"example.com"}, // a SAN of httptest's certificate
		"connect":   srv.Listener.Addr().String(),
		"warn_days": warnDays,
	})
	mod, err := New(bot.Env{Host: "edge-1", Log: slog.New(slog.DiscardHandler)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	m.roots = x509.NewCertPool()
	m.roots.AddCert(srv.Certificate())
	return m
}

func TestValidCertificate(t *testing.T) {
	srv := tlsServer()
	defer srv.Close()
	m := newModule(t, srv, 14)
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := m.Checks()[0].Run(context.Background()); len(f) != 0 {
		t.Errorf("findings %v", f)
	}
	sum, _ := m.Summary(context.Background())
	if !strings.HasPrefix(sum[0], "证书  1 个正常，最早到期 example.com 还剩 ") {
		t.Errorf("summary %q", sum)
	}
}

func TestExpiringSoon(t *testing.T) {
	srv := tlsServer()
	defer srv.Close()
	m := newModule(t, srv, 14)
	// Pretend we are a week before the end of the certificate's validity.
	end := srv.Certificate().NotAfter
	m.now = func() time.Time { return end.Add(-7 * 24 * time.Hour) }
	m.refresh(context.Background())
	f := m.Checks()[0].Run(context.Background())
	if len(f) != 1 || f[0].Key != "cert:example.com" || !strings.Contains(f[0].Msg, "还剩 7 天") {
		t.Fatalf("findings %v", f)
	}
}

func TestUntrusted(t *testing.T) {
	srv := tlsServer()
	defer srv.Close()
	m := newModule(t, srv, 14)
	m.roots = x509.NewCertPool() // trusts nothing
	m.refresh(context.Background())
	f := m.Checks()[0].Run(context.Background())
	if len(f) != 1 || !strings.Contains(f[0].Msg, "校验失败") {
		t.Fatalf("findings %v", f)
	}
	msg, _ := m.cmdCerts(context.Background(), bot.Request{})
	if !strings.Contains(msg.Pre, "⚠ 校验失败") {
		t.Errorf("certs:\n%s", msg.Pre)
	}
}

func TestUnreachable(t *testing.T) {
	raw := json.RawMessage(`{"hosts":["example.com"],"connect":"127.0.0.1:1"}`)
	mod, _ := New(bot.Env{}, raw)
	m := mod.(*Module)
	if err := m.refresh(context.Background()); err == nil {
		t.Fatal("no error")
	}
	f := m.Checks()[0].Run(context.Background())
	if len(f) != 1 || !strings.Contains(f[0].Msg, "读取失败") {
		t.Fatalf("findings %v", f)
	}
}
