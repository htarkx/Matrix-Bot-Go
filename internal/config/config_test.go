package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `{
  "host": "edge-1",
  "matrix": {"homeserver": "http://127.0.0.1:8008", "server_name": "example.org", "sender_localpart": "watch"},
  "bots": [{
    "id": "watch",
    "user": "watch",
    "admin": "@admin:example.org",
    "rooms": {"alerts": {"alias": "watch-alerts"}, "query": {"alias": "watch", "retention": "12h"}},
    "telegram": {"chat_id": 42, "commands": true},
    "alerts": {"interval": "15s"},
    "out_of_band": ["self:"],
    "modules": {"zeta": {"a": 1}, "alpha": {}, "mid": null}
  }]
}`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Bots[0]
	if got := b.Alerts.Interval.Duration; got != 15*time.Second {
		t.Errorf("interval = %v", got)
	}
	// defaults
	if b.Alerts.Sustain.Duration != time.Minute || b.Alerts.Remind.Duration != 6*time.Hour {
		t.Errorf("alert defaults not applied: %+v", b.Alerts)
	}
	if b.Rooms.Query.Retention.Duration != 12*time.Hour || b.Rooms.Query.PurgeEvery.Duration != time.Hour {
		t.Errorf("room durations: %+v", b.Rooms.Query)
	}
	if c.Matrix.TokenCredential != "matrix_token" || b.Telegram.TokenCredential != "telegram_token" {
		t.Errorf("credential defaults not applied")
	}
	// module order is the file's order, not sorted
	var names []string
	for _, m := range b.Modules {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "zeta,alpha,mid" {
		t.Errorf("module order = %v", names)
	}
	if got := c.Matrix.UserID("watch"); got != "@watch:example.org" {
		t.Errorf("UserID = %s", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"unknown field":   {`"host": "edge-1"`, `"hostname": "edge-1"`, "unknown field"},
		"bad duration":    {`"15s"`, `"soon"`, "invalid duration"},
		"bad admin":       {`"@admin:example.org"`, `"admin"`, "not a user ID"},
		"same rooms":      {`"watch-alerts"`, `"watch"`, "must differ"},
		"no chat id":      {`"chat_id": 42`, `"chat_id": 0`, "chat_id is required"},
		"sustain < tick":  {`"interval": "15s"`, `"interval": "5m"`, "sustain is shorter"},
		"duplicate mod":   {`"alpha": {}`, `"zeta": {}`, "listed twice"},
		"bad localpart":   {`"user": "watch"`, `"user": "Watch Bot"`, "not a valid localpart"},
		"modules not obj": {`{"zeta": {"a": 1}, "alpha": {}, "mid": null}`, `[]`, "must be an object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(valid, tc.from, tc.to, 1)
			if raw == valid {
				t.Fatalf("replacement %q not found", tc.from)
			}
			_, err := Parse([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOnePollerPerToken(t *testing.T) {
	raw := `{"bots": [
	  {"id": "a", "telegram": {"chat_id": 1, "commands": true}},
	  {"id": "b", "telegram": {"chat_id": 1, "commands": true}}]}`
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "one getUpdates consumer") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadCredential(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tok"), []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadCredential(dir, "tok"); err != nil || v != "s3cret" {
		t.Fatalf("got %q, %v", v, err)
	}
	for _, bad := range []string{"", "../tok", "a/b", ".."} {
		if _, err := ReadCredential(dir, bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := ReadCredential("", "tok"); err == nil {
		t.Error("empty dir accepted")
	}
}
