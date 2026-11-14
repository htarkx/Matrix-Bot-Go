package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

func msg(text string) message.Message { return message.Message{Lines: []string{text}} }

func TestRoomCreationAndDelivery(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	tg, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, nil, nil)
	ctx := context.Background()

	// Room is created on first use; the admin is only invited, so the
	// message is posted but undelivered and goes to Telegram with a note.
	b.notify(ctx, msg("disk full"), "disk:/")
	if len(hs.created) != 1 {
		t.Fatalf("created %d rooms", len(hs.created))
	}
	c := hs.created[0]
	pl := c["power_level_content_override"].(map[string]any)
	if c["room_alias_name"] != "watch-alerts" || c["preset"] != "private_chat" ||
		len(pl["users"].(map[string]any)) != 0 || pl["events_default"] != 100.0 {
		t.Errorf("createRoom body = %v", c)
	}
	room := hs.alias("#watch-alerts:ex.org")
	if got := hs.sentTo(room); len(got) != 1 || got[0].Content["msgtype"] != "m.text" {
		t.Fatalf("alerts room events = %+v", got)
	}
	if got := tg.sent(); len(got) != 1 || !strings.Contains(got[0], "disk full") || !strings.Contains(got[0], fallbackNote) {
		t.Fatalf("telegram = %q", got)
	}

	// Once the admin joined: Matrix only.
	hs.setMember(room, "@admin:ex.org", "join")
	b.notify(ctx, msg("cpu busy"), "cpu")
	if n := len(tg.sent()); n != 1 {
		t.Errorf("telegram got %d messages, want still 1", n)
	}
	// Out-of-band keys go to both, without the fallback note.
	b.notify(ctx, msg("purge broken"), "self:purge")
	got := tg.sent()
	if len(got) != 2 || strings.Contains(got[1], fallbackNote) {
		t.Errorf("out-of-band telegram = %q", got)
	}
	if n := len(hs.sentTo(room)); n != 3 {
		t.Errorf("alerts room has %d events, want 3", n)
	}
}

func TestReinviteWhenAdminLeft(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, nil, nil)
	ctx := context.Background()

	b.notify(ctx, msg("x"), "k")
	room := hs.alias("#watch-alerts:ex.org")
	hs.setMember(room, "@admin:ex.org", "leave")
	b.notify(ctx, msg("y"), "k")
	b.notify(ctx, msg("z"), "k") // within the re-invite interval: no second invite
	if len(hs.invites) != 1 || hs.invites[0] != "@admin:ex.org" {
		t.Errorf("invites = %v", hs.invites)
	}
}

func TestPowerLevelDriftIsReset(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	hs.aliases["#watch-alerts:ex.org"] = "!old:ex.org"
	hs.power["!old:ex.org"] = map[string]any{"users": map[string]any{"@mallory:ex.org": 100.0}, "events_default": 0.0, "historical": 1.0}
	b := testBot(t, hsSrv, tgSrv, nil, nil)

	if _, err := b.rooms.room(context.Background(), alertsRoom); err != nil {
		t.Fatal(err)
	}
	pl := hs.power["!old:ex.org"]
	if hs.putPL != 1 || len(pl["users"].(map[string]any)) != 0 || pl["events_default"] != 100.0 || pl["historical"] != 1.0 {
		t.Fatalf("puts=%d pl=%v", hs.putPL, pl)
	}
	// a fresh bot sees no drift and leaves the state alone
	b2 := testBot(t, hsSrv, tgSrv, nil, nil)
	b2.rooms.room(context.Background(), alertsRoom)
	if hs.putPL != 1 {
		t.Errorf("power levels rewritten without drift")
	}
}

type fakeModule struct {
	NoJobs
	findings atomic.Value // []Finding
}

func (m *fakeModule) Checks() []Check {
	return []Check{{Name: "fake", Run: func(context.Context) []Finding {
		f, _ := m.findings.Load().([]Finding)
		return f
	}}}
}

func (m *fakeModule) Commands() []Command {
	return []Command{
		{Name: "status", Aliases: []string{"s"}, Help: "状态", Run: func(_ context.Context, r Request) (message.Message, error) {
			if r.Received.IsZero() {
				return message.Message{}, errors.New("Received not set")
			}
			if r.Channel == "matrix" && !r.Sent.Equal(time.UnixMilli(1_700_000_000_123)) {
				return message.Message{}, fmt.Errorf("Sent = %v", r.Sent)
			}
			return msg("status via " + r.Channel + " " + strings.Join(r.Args, ",")), nil
		}},
		{Name: "boom", Help: "出错", Run: func(context.Context, Request) (message.Message, error) {
			return message.Message{}, errors.New("kaputt")
		}},
	}
}

func withFake(m *fakeModule) map[string]Factory {
	return map[string]Factory{"fake": func(Env, json.RawMessage) (Module, error) { return m, nil }}
}

func useFake(c *config.BotConfig) { c.Modules = config.Modules{{Name: "fake"}} }

func TestDispatch(t *testing.T) {
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, useFake, withFake(&fakeModule{}))
	ctx := context.Background()
	cases := []struct {
		text       string
		needPrefix bool
		want       string // "" = not addressed to the bot
	}{
		{"s", false, "status via test "},
		{"  STATUS a b ", false, "status via test a,b"},
		{"!s", false, "status via test "},
		{"/status@SomeBot", true, "status via test "},
		{"status", true, ""},
		{"hello there", false, "可用指令"},
		{"", false, "可用指令"},
		{"?", false, "可用指令"},
		{"/boom", true, "指令出错：kaputt"},
		{"a", false, "没有告警"},
	}
	for _, c := range cases {
		reply, ok := b.dispatch(ctx, Request{Channel: "test"}, c.text, c.needPrefix, false)
		if c.want == "" {
			if ok {
				t.Errorf("%q: answered %q", c.text, reply.Plain())
			}
			continue
		}
		if !ok || !strings.Contains(reply.Plain(), c.want) {
			t.Errorf("%q: got %q (ok=%v), want %q", c.text, reply.Plain(), ok, c.want)
		}
	}
	help, _ := b.dispatch(ctx, Request{}, "h", false, false)
	if p := help.Plain(); !strings.Contains(p, "s — 状态（status）") || !strings.Contains(p, "boom — 出错（boom）") ||
		!strings.Contains(p, "h/? — 指令列表（help）") {
		t.Errorf("help = %q", p)
	}
}

func TestCommandNameClash(t *testing.T) {
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	clash := map[string]Factory{"fake": func(Env, json.RawMessage) (Module, error) {
		return struct {
			NoChecks
			NoJobs
			commandsOf
		}{commandsOf: commandsOf{{Name: "attack", Aliases: []string{"a"}}}}, nil
	}}
	mx := &config.MatrixConfig{Homeserver: hsSrv.URL, ServerName: "ex.org", SenderLocalpart: "watch"}
	_, err := New(Options{
		Config:    config.BotConfig{ID: "x", Modules: config.Modules{{Name: "fake"}}, Alerts: config.AlertConfig{Interval: config.Duration{Duration: time.Second}, Sustain: config.Duration{Duration: time.Second}}},
		MatrixCfg: mx, Factories: clash, Log: testBot(t, hsSrv, tgSrv, nil, nil).log,
	})
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("err = %v", err)
	}
	_, err = New(Options{
		Config:    config.BotConfig{ID: "x", Modules: config.Modules{{Name: "nope"}}, Alerts: config.AlertConfig{Interval: config.Duration{Duration: time.Second}, Sustain: config.Duration{Duration: time.Second}}},
		Factories: clash, Log: testBot(t, hsSrv, tgSrv, nil, nil).log,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("err = %v", err)
	}
}

type commandsOf []Command

func (c commandsOf) Commands() []Command { return c }

func TestRunEndToEnd(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	tg, tgSrv := newFakeTG(t)
	mod := &fakeModule{}
	b := testBot(t, hsSrv, tgSrv, useFake, withFake(mod))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	alerts := waitFor(t, func() string { return hs.alias("#watch-alerts:ex.org") })
	hs.setMember(alerts, "@admin:ex.org", "join")
	query := waitFor(t, func() string { return hs.alias("#watch:ex.org") })
	hs.setMember(query, "@admin:ex.org", "join")

	// a finding fires (sustain 1 tick), then resolves
	mod.findings.Store([]Finding{{Key: "disk:/", What: "磁盘 /", Msg: "已用 91%"}})
	fired := waitFor(t, func() string { return lastBody(hs.sentTo(alerts), "磁盘 / 已用 91%") })
	if !strings.HasPrefix(fired, "🔴 edge-1：磁盘 / 已用 91%") {
		t.Errorf("firing message %q", fired)
	}
	mod.findings.Store([]Finding(nil))
	resolved := waitFor(t, func() string { return lastBody(hs.sentTo(alerts), "恢复正常") })
	if !strings.HasPrefix(resolved, "🟢 edge-1：磁盘 / 恢复正常\n异常持续 ") || !strings.Contains(resolved, "（当时：已用 91%）") {
		t.Errorf("resolved message %q", resolved)
	}

	// the admin's message is answered as m.notice; others are ignored
	hs.syncs <- syncBody(query, "@mallory:ex.org", "s")
	hs.syncs <- syncBody(query, "@admin:ex.org", "s x")
	reply := waitFor(t, func() string { return lastBody(hs.sentTo(query), "status via matrix") })
	if n := len(hs.sentTo(query)); n != 1 {
		t.Errorf("query room has %d replies, want 1 (%s)", n, reply)
	}
	if ev := hs.sentTo(query)[0]; ev.Content["msgtype"] != "m.notice" {
		t.Errorf("reply msgtype = %v", ev.Content["msgtype"])
	}
	// a reply needs no membership check: the admin's own command proves it
	hs.mu.Lock()
	gets := hs.memberGets
	hs.mu.Unlock()
	hs.syncs <- syncBody(query, "@admin:ex.org", "s")
	waitFor(t, func() string {
		if len(hs.sentTo(query)) == 2 {
			return "two"
		}
		return ""
	})
	hs.mu.Lock()
	after := hs.memberGets
	hs.mu.Unlock()
	if after != gets {
		t.Errorf("reply did %d membership lookups, want 0", after-gets)
	}

	// the command menu is scoped to the configured chat; the default one is removed
	tg.mu.Lock()
	menu := strings.Join(tg.menu, " | ")
	tg.mu.Unlock()
	if menu != `setMyCommands {"chat_id":42,"type":"chat"} | deleteMyCommands ` {
		t.Errorf("menu calls = %q", menu)
	}

	// Telegram commands from the configured chat only
	now := time.Now().Unix()
	tg.updates <- fmt.Sprintf(`{"ok":true,"result":[
		{"update_id":1,"message":{"date":%d,"text":"/status","chat":{"id":7}}},
		{"update_id":2,"message":{"date":%d,"text":"/status","chat":{"id":42}}},
		{"update_id":3,"message":{"date":%d,"text":"/status","chat":{"id":42}}}]}`, now, now-3600, now)
	waitFor(t, func() string {
		var n int
		for _, s := range tg.sent() {
			if strings.Contains(s, "status via telegram") {
				n++
			}
		}
		if n == 1 {
			return "one"
		}
		return ""
	})
}

func lastBody(evs []sentEvent, sub string) string {
	for i := len(evs) - 1; i >= 0; i-- {
		if body, _ := evs[i].Content["body"].(string); strings.Contains(body, sub) {
			return body
		}
	}
	return ""
}

func syncBody(room, sender, text string) string {
	return fmt.Sprintf(`{"next_batch":"s%d","rooms":{"join":{%q:{"timeline":{"events":[
		{"type":"m.room.message","sender":%q,"event_id":"$x","origin_server_ts":1700000000123,"content":{"msgtype":"m.text","body":%q}}]}}}}}`,
		time.Now().UnixNano(), room, sender, text)
}

// waitFor polls until f returns non-empty or two seconds pass.
func waitFor(t *testing.T, f func() string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := f(); v != "" {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
	return ""
}

func TestDryRunSendsNothing(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	tg, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, nil, nil)
	b.dryRun = true
	b.notify(context.Background(), msg("x"), "k")
	if len(hs.created) != 0 || len(tg.sent()) != 0 {
		t.Fatal("dry-run touched a channel")
	}
}
