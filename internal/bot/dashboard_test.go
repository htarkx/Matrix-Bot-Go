package bot

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/htarkx/Matrix-Bot-Go/internal/alert"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/config"
)

// In a shared room only known commands are answered.
func TestStrictDispatch(t *testing.T) {
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, useFake, withFake(&fakeModule{}))
	ctx := context.Background()
	for text, want := range map[string]string{
		"s":                      "status via test",
		"!s":                     "status via test",
		"h":                      "可用指令",
		"!admin users list":      "",
		`\!admin users list`:     "",
		"@conduit:ex.org: help":  "",
		"thanks, looks good":     "",
		"":                       "",
		"server uptime":          "",
		"!admin server uptime 1": "",
	} {
		reply, ok := b.dispatch(ctx, Request{Channel: "test"}, text, false, true)
		switch {
		case want == "" && ok:
			t.Errorf("%q answered in a shared room: %q", text, reply.Plain())
		case want != "" && (!ok || !strings.Contains(reply.Plain(), want)):
			t.Errorf("%q: got %q (ok=%v), want %q", text, reply.Plain(), ok, want)
		}
	}
}

type summaryModule struct {
	NoChecks
	NoCommands
	NoJobs
	lines []string
	err   error
	slow  bool
}

func (m summaryModule) Summary(ctx context.Context) ([]string, error) {
	if m.slow {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.lines, m.err
}

func TestDashboard(t *testing.T) {
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	factories := map[string]Factory{
		"host":  func(Env, json.RawMessage) (Module, error) { return summaryModule{lines: []string{"主机  ok"}}, nil },
		"store": func(Env, json.RawMessage) (Module, error) { return summaryModule{err: errors.New("no key")}, nil },
		"chat": func(Env, json.RawMessage) (Module, error) {
			return summaryModule{lines: []string{"聊天  a", "媒体  b"}}, nil
		},
	}
	b := testBot(t, hsSrv, tgSrv, func(c *config.BotConfig) {
		c.Modules = config.Modules{{Name: "host"}, {Name: "store"}, {Name: "chat"}}
	}, factories)
	reply, ok := b.dispatch(context.Background(), Request{}, "d", false, false)
	if !ok {
		t.Fatal("d not answered")
	}
	if want := "主机  ok\nstore  读取失败：no key\n聊天  a\n媒体  b"; reply.Pre != want {
		t.Errorf("dashboard\n%s\nwant\n%s", reply.Pre, want)
	}
	help, _ := b.dispatch(context.Background(), Request{}, "h", false, false)
	if !strings.Contains(help.Plain(), "d — 总览（dashboard）") {
		t.Errorf("help lacks the dashboard: %s", help.Plain())
	}

	// without summarizing modules there is no dashboard command
	plain := testBot(t, hsSrv, tgSrv, useFake, withFake(&fakeModule{}))
	if _, ok := plain.byName["d"]; ok {
		t.Error("dashboard offered with nothing to show")
	}
}

func TestDashboardSlowModule(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the section timeout")
	}
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	factories := map[string]Factory{
		"slow": func(Env, json.RawMessage) (Module, error) { return summaryModule{slow: true}, nil },
		"fast": func(Env, json.RawMessage) (Module, error) { return summaryModule{lines: []string{"x  y"}}, nil },
	}
	b := testBot(t, hsSrv, tgSrv, func(c *config.BotConfig) {
		c.Modules = config.Modules{{Name: "slow"}, {Name: "fast"}}
	}, factories)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	reply, _ := b.dispatch(ctx, Request{}, "d", false, false)
	if !strings.HasPrefix(reply.Pre, "slow  读取失败：") || !strings.HasSuffix(reply.Pre, "x  y") {
		t.Errorf("dashboard %q", reply.Pre)
	}
}

// Commands in a command room (the homeserver's admin room) are answered
// there; the homeserver's own commands and chatter are left alone.
func TestCommandRoomEndToEnd(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	const admins = "!admins:ex.org"
	hs.mu.Lock()
	hs.aliases["#admins:ex.org"] = admins
	hs.members[admins+"|@watch:ex.org"] = "join"
	hs.mu.Unlock()
	b := testBot(t, hsSrv, tgSrv, func(c *config.BotConfig) {
		useFake(c)
		c.Rooms.Commands = []string{"admins", "missing"}
	}, withFake(&fakeModule{}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	query := waitFor(t, func() string { return hs.alias("#watch:ex.org") })
	// wait until the sync loop has picked up the admin room
	waitFor(t, func() string {
		if len(b.rooms.commandRooms(ctx)) == 1 {
			return "ok"
		}
		return ""
	})
	time.Sleep(50 * time.Millisecond) // let the loop restart with the new filter

	hs.syncs <- syncBody(admins, "@admin:ex.org", "!admin users list")
	hs.syncs <- syncBody(admins, "@admin:ex.org", "nice")
	hs.syncs <- syncBody(admins, "@conduit:ex.org", "s")
	hs.syncs <- syncBody(admins, "@admin:ex.org", "s")
	waitFor(t, func() string { return lastBody(hs.sentTo(admins), "status via matrix") })
	time.Sleep(50 * time.Millisecond)
	if got := hs.sentTo(admins); len(got) != 1 || got[0].Content["msgtype"] != "m.notice" {
		t.Errorf("admin room got %d messages: %+v", len(got), got)
	}
	if n := len(hs.sentTo(query)); n != 0 {
		t.Errorf("query room got %d replies", n)
	}
}

// Alerts lead with the machine that has the problem; a problem seen on
// another machine says who noticed it.
func TestAlertWording(t *testing.T) {
	_, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	b := testBot(t, hsSrv, tgSrv, nil, nil)
	b.seen["probe:rtc"] = Finding{Key: "probe:rtc", Subject: "fra", What: "通话服务", Msg: "无法访问（HTTP 502）"}
	b.seen["svc:nginx"] = Finding{Key: "svc:nginx", What: "服务 nginx", Msg: "停了（inactive）"}
	b.seen["legacy"] = Finding{Key: "legacy", Msg: "something odd"}
	cases := []struct {
		ev   alert.Event
		want string
	}{
		{alert.Event{Kind: alert.Firing, Key: "probe:rtc", Msg: "无法访问（HTTP 502）"},
			"🔴 fra：通话服务 无法访问（HTTP 502）\n由 edge-1 从外部发现"},
		{alert.Event{Kind: alert.Resolved, Key: "probe:rtc", Msg: "无法访问（HTTP 502）", Age: 2 * time.Minute},
			"🟢 fra：通话服务 恢复正常\n异常持续 2 分钟（当时：无法访问（HTTP 502））\n由 edge-1 从外部发现"},
		{alert.Event{Kind: alert.Firing, Key: "svc:nginx", Msg: "停了（inactive）"},
			"🔴 edge-1：服务 nginx 停了（inactive）"},
		{alert.Event{Kind: alert.Reminder, Key: "svc:nginx", Msg: "停了（inactive）", Age: 6 * time.Hour},
			"🔴 edge-1：服务 nginx 停了（inactive）\n仍未恢复，已持续 6 小时 0 分"},
		{alert.Event{Kind: alert.Resolved, Key: "legacy", Msg: "something odd", Age: time.Minute},
			"🟢 edge-1：恢复正常\n异常持续 1 分钟（当时：something odd）"},
	}
	for _, c := range cases {
		if got := b.alertMessage(c.ev).Plain(); got != c.want {
			t.Errorf("%s %s:\n%s\nwant\n%s", c.ev.Kind, c.ev.Key, got, c.want)
		}
	}
	if hint := b.commandHint(); hint != "（查询房间发 h 看指令，Telegram 发 /help）" {
		t.Errorf("hint %q", hint)
	}
}

type postingModule struct {
	NoChecks
	NoCommands
	env     *Env
	stopped chan struct{}
}

func (p *postingModule) Jobs() []Job {
	return []Job{{Name: "report", Every: time.Hour, Run: func(ctx context.Context) error {
		return p.env.Post(ctx, message.Message{Icon: "📈", Title: "周报"})
	}}}
}
func (p *postingModule) Stop(context.Context) { close(p.stopped) }

// A module's Post lands in the alerts room without a push (m.notice); its
// Stop runs once the bot shuts down.
func TestPostAndStop(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	mod := &postingModule{stopped: make(chan struct{})}
	factories := map[string]Factory{"rep": func(env Env, _ json.RawMessage) (Module, error) {
		mod.env = &env
		if env.Interval != time.Second {
			t.Errorf("Env.Interval = %v", env.Interval)
		}
		return mod, nil
	}}
	b := testBot(t, hsSrv, tgSrv, func(c *config.BotConfig) { c.Modules = config.Modules{{Name: "rep"}} }, factories)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	alerts := waitFor(t, func() string { return hs.alias("#watch-alerts:ex.org") })
	hs.setMember(alerts, "@admin:ex.org", "join")
	waitFor(t, func() string { return lastBody(hs.sentTo(alerts), "周报") })
	for _, ev := range hs.sentTo(alerts) {
		if body, _ := ev.Content["body"].(string); strings.Contains(body, "周报") && ev.Content["msgtype"] != "m.notice" {
			t.Errorf("report msgtype %v", ev.Content["msgtype"])
		}
	}
	cancel()
	<-done
	select {
	case <-mod.stopped:
	default:
		t.Error("Stop not called")
	}
}

// -exec runs cache-filling jobs but never side-effecting ones, and posts
// nothing.
func TestExecSkipsSideEffects(t *testing.T) {
	hs, hsSrv := newFakeHS(t)
	_, tgSrv := newFakeTG(t)
	var ran []string
	factories := map[string]Factory{"m": func(env Env, _ json.RawMessage) (Module, error) {
		return struct {
			NoChecks
			NoCommands
			jobsOf
		}{jobsOf: jobsOf{
			{Name: "fill", Every: time.Hour, Run: func(context.Context) error { ran = append(ran, "fill"); return nil }},
			{Name: "save", Every: time.Hour, SideEffects: true, Run: func(context.Context) error { ran = append(ran, "save"); return nil }},
			{Name: "post", Every: time.Hour, Run: func(ctx context.Context) error {
				ran = append(ran, "post")
				return env.Post(ctx, message.Message{Title: "x"})
			}},
		}}, nil
	}}
	b := testBot(t, hsSrv, tgSrv, func(c *config.BotConfig) { c.Modules = config.Modules{{Name: "m"}} }, factories)
	if _, err := b.Exec(context.Background(), "h"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ran, ",") != "fill,post" {
		t.Errorf("ran %v", ran)
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if len(hs.sent) != 0 || len(hs.created) != 0 {
		t.Errorf("exec touched Matrix: %d sent, %d rooms", len(hs.sent), len(hs.created))
	}
}

type jobsOf []Job

func (j jobsOf) Jobs() []Job { return j }
