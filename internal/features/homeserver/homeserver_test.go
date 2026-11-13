package homeserver

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

var now = time.Unix(1_800_000_000, 0)

func ms(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }

type fakeAPI struct{ pages map[string][]string }

func (f fakeAPI) Get(_ context.Context, path string, q url.Values, out any) error {
	key := path
	if from := q.Get("from"); from != "" && from != "0" {
		key += "?from=" + from
	}
	pages := f.pages[key]
	if len(pages) == 0 {
		return json.Unmarshal([]byte(`{}`), out)
	}
	return json.Unmarshal([]byte(pages[0]), out)
}

func newModule(t *testing.T) *Module {
	t.Helper()
	users1, _ := json.Marshal(map[string]any{"users": []map[string]any{
		{"name": "@alice:ex.org", "admin": true, "last_seen_ts": ms(time.Hour)},
		{"name": "@bob:ex.org", "last_seen_ts": ms(3 * 24 * time.Hour)},
	}, "next_token": "2"})
	users2, _ := json.Marshal(map[string]any{"users": []map[string]any{
		{"name": "@carol:ex.org", "last_seen_ts": 0},
		{"name": "@gone:ex.org", "deactivated": true, "last_seen_ts": ms(time.Minute)},
		{"name": "@watch:ex.org", "last_seen_ts": ms(time.Minute)},
	}, "next_token": nil})
	api := fakeAPI{pages: map[string][]string{
		"/_synapse/admin/v2/users":        {string(users1)},
		"/_synapse/admin/v2/users?from=2": {string(users2)},
		"/_synapse/admin/v1/rooms": {`{"rooms":[
			{"room_id":"!a:ex.org","name":"家庭群","joined_members":3,"encryption":"m.megolm.v1.aes-sha2"},
			{"room_id":"!b:ex.org","name":null,"joined_members":2},
			{"room_id":"!c:ex.org","joined_members":0,"creator":"@watch:ex.org"},
			{"room_id":"!d:ex.org","joined_members":0,"creator":"@watch:ex.org"}],"next_batch":null}`},
		"/_synapse/admin/v1/rooms/%21b:ex.org/members": {`{"members":["@bob:ex.org","@alice:ex.org","@watch:ex.org"]}`},
		"/_synapse/admin/v1/statistics/users/media": {`{"users":[
			{"user_id":"@alice:ex.org","media_count":3,"media_length":3145728},
			{"user_id":"@bob:ex.org","media_count":1,"media_length":1048576}],"next_token":null}`},
	}}
	return &Module{cfg: Config{HideUsers: []string{"watch"}}, env: bot.Env{Host: "edge-1"}, api: api,
		now: func() time.Time { return now }}
}

func TestSummary(t *testing.T) {
	sum, err := newModule(t).Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"聊天  用户 3（24h 活跃 1 · 7天 2）· 房间 4（有人 2 · 空 2）",
		"媒体  4 个文件 4.0M（homeserver 记账）",
	}
	if strings.Join(sum, "\n") != strings.Join(want, "\n") {
		t.Errorf("summary\n%s\nwant\n%s", strings.Join(sum, "\n"), strings.Join(want, "\n"))
	}
}

func TestUsersAndRooms(t *testing.T) {
	m := newModule(t)
	u, err := m.cmdUsers(context.Background(), bot.Request{})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(u.Pre, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "alice*") || !strings.HasPrefix(lines[2], "carol") ||
		!strings.Contains(lines[2], "从未") || !strings.Contains(lines[0], "3.0M") {
		t.Errorf("users:\n%s", u.Pre)
	}
	r, err := m.cmdRooms(context.Background(), bot.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"🔒  3 人  家庭群", "2 人  （alice、bob）"} {
		if !strings.Contains(r.Pre, want) {
			t.Errorf("rooms missing %q:\n%s", want, r.Pre)
		}
	}
	if r.Note != "另有 2 个空房间（按创建者：watch 2）" {
		t.Errorf("note %q", r.Note)
	}
}
