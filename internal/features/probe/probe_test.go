package probe

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if !strings.Contains(msg.Pre, "h ok  ✅") || !strings.Contains(msg.Pre, "h slow  ❌ 超时") {
		t.Errorf("probe command:\n%s", msg.Pre)
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
