package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

// fakeSD answers Show from a table; a nil table means systemd is unreachable.
type fakeSD map[string]map[string]string

func (f fakeSD) Show(_ context.Context, unit string, _ ...string) (map[string]string, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: Transport endpoint is not connected", ErrUnreachable)
	}
	return f[unit], nil
}

func mod(t *testing.T, sd Shower) *Module {
	t.Helper()
	m, err := newWith(bot.Env{Host: "edge-1", BotID: "watch", Log: slog.New(slog.DiscardHandler)}, json.RawMessage(`{
		"units": ["nginx", "app"],
		"backups": {"db": "2h", "media": "26h", "never": "1h"}
	}`), sd)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ago(d time.Duration) string { return fmt.Sprintf("@%d", time.Now().Add(-d).Unix()) }

func TestCheck(t *testing.T) {
	sd := fakeSD{
		"nginx":                      {"ActiveState": "active"},
		"app":                        {"ActiveState": "failed"},
		"edge-backup@db.service":     {"Result": "success", "ExecMainExitTimestamp": ago(3 * time.Hour)},
		"edge-backup@media.service":  {"Result": "exit-code", "ExecMainExitTimestamp": ago(time.Hour)},
		"edge-backup@never.service":  {"Result": "success", "ExecMainExitTimestamp": ""},
		"edge-backup@unused.service": {},
	}
	got := map[string]string{}
	for _, f := range mod(t, sd).check(context.Background()) {
		got[f.Key] = f.Msg
	}
	want := map[string]string{
		"svc:app":   "服务 app 状态 failed",
		"bak:db":    "备份 db 已 3 小时 0 分 没有运行",
		"bak:media": "备份 media 上次运行失败（exit-code）",
		"bak:never": "备份 never 还没有成功运行过",
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestUnreachableIsOneSelfAlert(t *testing.T) {
	fs := mod(t, fakeSD(nil)).check(context.Background())
	if len(fs) != 1 || fs[0].Key != "self:systemd" || !strings.Contains(fs[0].Msg, "Transport endpoint") {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestServicesCommand(t *testing.T) {
	sd := fakeSD{
		"nginx":                     {"ActiveState": "active", "MemoryCurrent": "13504512"},
		"app":                       {"ActiveState": "inactive", "MemoryCurrent": "[not set]"},
		"edge-backup@db.service":    {"Result": "success", "ExecMainExitTimestamp": ago(10 * time.Minute)},
		"edge-backup@media.service": {"Result": "success", "ExecMainExitTimestamp": ago(5 * time.Hour)},
		"edge-backup@never.service": {"Result": "success"},
	}
	msg, err := mod(t, sd).cmdServices(context.Background(), bot.Request{})
	if err != nil {
		t.Fatal(err)
	}
	want := "nginx        active   12.9M\n" +
		"app          inactive\n" +
		"备份 db       success  10 分钟前\n" +
		"备份 media    success  5 小时 0 分前\n" +
		"备份 never    success  从未运行"
	if msg.Pre != want {
		t.Errorf("services:\n%s\nwant:\n%s", msg.Pre, want)
	}
}

func TestConfigValidation(t *testing.T) {
	env := bot.Env{Log: slog.New(slog.DiscardHandler)}
	for raw, want := range map[string]string{
		`{}`: "nothing to watch",
		`{"units": ["a"], "backup_unit": "x.service"}`: "exactly one %s",
		`{"units": ["a"], "unit": "x"}`:                "unknown field",
		`{"backups": {"db": "soon"}}`:                  "invalid duration",
	} {
		if _, err := newWith(env, json.RawMessage(raw), fakeSD{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", raw, err, want)
		}
	}
}
