package probe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

func newModule(t *testing.T, cfg string) *Module {
	t.Helper()
	mod, err := New(bot.Env{Host: "edge-2", Log: slog.New(slog.DiscardHandler)}, json.RawMessage(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return mod.(*Module)
}

func TestProbes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "{}") })
	mux.HandleFunc("/auth", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", 302) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tls := httptest.NewUnstartedServer(mux)
	tls.Config.ErrorLog = log.New(io.Discard, "", 0)
	tls.StartTLS() // self-signed: must fail verification
	defer tls.Close()

	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()

	cfg, _ := json.Marshal(map[string]any{
		"timeout": "300ms",
		"targets": []map[string]any{
			{"host": "h", "name": "ok", "http": srv.URL + "/ok"},
			{"host": "h", "name": "login", "http": srv.URL + "/auth", "expect": 401},
			{"host": "h", "name": "wrong", "http": srv.URL + "/auth"},
			{"host": "h", "name": "moved", "http": srv.URL + "/moved"},
			{"host": "h", "name": "slow", "http": srv.URL + "/slow"},
			{"host": "h", "name": "untrusted", "http": tls.URL + "/ok"},
			{"host": "h", "name": "port", "tcp": open.Addr().String()},
			{"host": "h", "name": "noport", "tcp": closedAddr},
		},
	})
	m := newModule(t, string(cfg))
	got := map[string]string{}
	for _, f := range m.Checks()[0].Run(context.Background()) {
		got[f.Key] = f.Msg
		if f.Subject != "h" || f.What != strings.TrimPrefix(f.Key, "probe:") {
			t.Errorf("%s: subject %q what %q", f.Key, f.Subject, f.What)
		}
	}
	want := map[string]string{
		"probe:wrong":     "无法访问（HTTP 401（应为 200））",
		"probe:moved":     "无法访问（HTTP 302（应为 200））",
		"probe:slow":      "无法访问（超时）",
		"probe:untrusted": "无法访问（",
		"probe:noport":    "无法访问（",
	}
	if len(got) != len(want) {
		t.Fatalf("findings:\n%v", got)
	}
	for k, prefix := range want {
		if !strings.HasPrefix(got[k], prefix) {
			t.Errorf("%s = %q, want prefix %q", k, got[k], prefix)
		}
	}
	if !strings.Contains(got["probe:untrusted"], "certificate") {
		t.Errorf("untrusted: %q", got["probe:untrusted"])
	}
	if !strings.Contains(got["probe:noport"], "refused") {
		t.Errorf("noport: %q", got["probe:noport"])
	}

	sum, _ := m.Summary(context.Background())
	if want := "探测  3/8 可达（h wrong，h moved，h slow，h untrusted，h noport 不可达）"; sum[0] != want {
		t.Errorf("summary %q, want %q", sum[0], want)
	}
	msg, _ := m.cmdProbe(context.Background(), bot.Request{})
	if !strings.Contains(msg.Pre, "h ok  ✅ 往返 ") || !strings.Contains(msg.Pre, "ms，请求 ") ||
		!strings.Contains(msg.Pre, "h port  ✅ 往返 ") || !strings.Contains(msg.Pre, "h slow  ❌ 超时") {
		t.Errorf("probe command:\n%s", msg.Pre)
	}
	// the TCP connect of an HTTP probe is kept as its round trip
	if w := m.st.Targets["ok"].window(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); w.RTTN != 1 || w.OK != 1 {
		t.Errorf("ok window %+v", w)
	}
}

// HTTP probes send HEAD unless told otherwise, and resume TLS sessions, but
// do a full handshake again after fullHandshakeEvery.
func TestMethodAndResumption(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s %s resumed=%v", r.Method, r.URL.Path, r.TLS.DidResume))
		mu.Unlock()
	}))
	defer srv.Close()

	clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cfg, _ := json.Marshal(map[string]any{"targets": []map[string]any{
		{"host": "h", "name": "head", "http": srv.URL + "/head"},
		{"host": "h", "name": "get", "http": srv.URL + "/get", "method": "GET"},
	}})
	m, err := newWithClock(bot.Env{Log: slog.New(slog.DiscardHandler)}, cfg, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	m.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots

	round := func() []string {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		for _, tg := range m.cfg.Targets { // one at a time: a ticket only exists after a handshake
			if _, err := m.probe(context.Background(), tg); err != nil {
				t.Fatalf("%s: %v", tg.Name, err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
	full := []string{"HEAD /head resumed=false", "GET /get resumed=true"}
	resumed := []string{"HEAD /head resumed=true", "GET /get resumed=true"}
	for i, want := range [][]string{full, resumed, resumed} {
		if got := round(); !slices.Equal(got, want) {
			t.Errorf("round %d: %q, want %q", i, got, want)
		}
		clock = clock.Add(30 * time.Second)
	}
	clock = clock.Add(fullHandshakeEvery)
	if got := round(); !slices.Equal(got, full) {
		t.Errorf("after %v: %q, want a full handshake first", fullHandshakeEvery, got)
	}
}

func TestSummaryBeforeFirstRound(t *testing.T) {
	m := newModule(t, `{"targets":[{"host":"h","name":"x","tcp":"127.0.0.1:1"}]}`)
	if s, _ := m.Summary(context.Background()); s[0] != "探测  进行中" {
		t.Errorf("summary %q", s)
	}
}

func TestConfig(t *testing.T) {
	for cfg, want := range map[string]string{
		`{}`:                                     "targets is required",
		`{"targets":[{"http":"https://x"}]}`:     "name is required",
		`{"targets":[{"name":"a","tcp":"x:1"}]}`: "host (the machine",
		`{"targets":[{"host":"h","name":"a","http":"https://x","tcp":"x:1"}]}`:                                "exactly one",
		`{"targets":[{"host":"h","name":"a"}]}`:                                                               "exactly one",
		`{"targets":[{"host":"h","name":"a","http":"ftp://x"}]}`:                                              "http(s) URL",
		`{"targets":[{"host":"h","name":"a","tcp":"no-port"}]}`:                                               "missing port",
		`{"targets":[{"host":"h","name":"a","tcp":"x:1"},{"host":"h","name":"a","tcp":"y:1"}]}`:               "duplicate",
		`{"targets":[{"host":"h","name":"a","tcp":"x:1"}],"bogus":1}`:                                         "unknown field",
		`{"targets":[{"host":"h","name":"a","http":"https://x","method":"POST"}]}`:                            "HEAD or GET",
		`{"targets":[{"host":"h","name":"a","tcp":"x:1","method":"GET"}]}`:                                    "for http targets",
		`{"targets":[{"host":"h","name":"a","http":"https://x"},{"host":"h","name":"b","http":"https://y"}]}`: "",
	} {
		_, err := New(bot.Env{}, json.RawMessage(cfg))
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", cfg, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err %v, want %q", cfg, err, want)
		}
	}
}
