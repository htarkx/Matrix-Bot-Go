package bot

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
	"github.com/htarkx/Matrix-Bot-Go/internal/telegram"
)

// fakeHS is a tiny stateful homeserver: aliases, rooms with membership and
// power levels, sent events and a scripted /sync.
type fakeHS struct {
	t  *testing.T
	mu sync.Mutex

	aliases map[string]string // #alias:server -> room
	members map[string]string // room|user -> membership
	power   map[string]map[string]any
	created []map[string]any
	sent    []sentEvent
	invites []string
	putPL   int
	syncs   chan string // bodies for incremental /sync answers
	nextID  int
}

type sentEvent struct {
	Room, User string
	Content    map[string]any
}

func newFakeHS(t *testing.T) (*fakeHS, *httptest.Server) {
	f := &fakeHS{
		t: t, aliases: map[string]string{}, members: map[string]string{},
		power: map[string]map[string]any{}, syncs: make(chan string, 10),
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeHS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	locked := true
	defer func() {
		if locked {
			f.mu.Unlock()
		}
	}()
	user := r.URL.Query().Get("user_id")
	if user == "" {
		user = "@watch:ex.org"
	}
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	seg := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/_matrix/client/v3/"), "/")
	for i := range seg {
		seg[i], _ = url.PathUnescape(seg[i])
	}
	ok := func(v any) { json.NewEncoder(w).Encode(v) }
	notFound := func() {
		w.WriteHeader(404)
		io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"not found"}`)
	}
	switch {
	case r.Method == "GET" && seg[0] == "directory":
		if id, found := f.aliases[seg[2]]; found {
			ok(map[string]string{"room_id": id})
		} else {
			notFound()
		}
	case r.Method == "POST" && seg[0] == "createRoom":
		f.nextID++
		id := fmt.Sprintf("!r%d:ex.org", f.nextID)
		f.aliases["#"+body["room_alias_name"].(string)+":ex.org"] = id
		f.created = append(f.created, body)
		f.members[id+"|"+user] = "join"
		for _, inv := range body["invite"].([]any) {
			f.members[id+"|"+inv.(string)] = "invite"
		}
		pl, _ := body["power_level_content_override"].(map[string]any)
		f.power[id] = pl
		ok(map[string]string{"room_id": id})
	case r.Method == "GET" && seg[0] == "rooms" && seg[2] == "state" && seg[3] == "m.room.member":
		if m, found := f.members[seg[1]+"|"+seg[4]]; found {
			ok(map[string]string{"membership": m})
		} else {
			notFound()
		}
	case seg[0] == "rooms" && seg[2] == "state" && seg[3] == "m.room.power_levels":
		if r.Method == "PUT" {
			f.power[seg[1]] = body
			f.putPL++
			ok(map[string]string{"event_id": "$pl"})
		} else {
			ok(f.power[seg[1]])
		}
	case r.Method == "POST" && seg[0] == "rooms" && seg[2] == "invite":
		f.invites = append(f.invites, body["user_id"].(string))
		ok(map[string]string{})
	case r.Method == "PUT" && seg[0] == "rooms" && seg[2] == "send":
		f.sent = append(f.sent, sentEvent{seg[1], user, body})
		ok(map[string]string{"event_id": fmt.Sprintf("$e%d", len(f.sent))})
	case r.Method == "GET" && seg[0] == "sync":
		if !r.URL.Query().Has("since") {
			ok(map[string]any{"next_batch": "s0"})
			return
		}
		f.mu.Unlock()
		locked = false
		select {
		case b := <-f.syncs:
			io.WriteString(w, b)
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
			io.WriteString(w, `{"next_batch":"s-idle"}`)
		}
	default:
		f.t.Errorf("fake hs: unexpected %s %s", r.Method, r.URL.Path)
		notFound()
	}
}

func (f *fakeHS) sentTo(room string) []sentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentEvent
	for _, e := range f.sent {
		if e.Room == room {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeHS) alias(a string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aliases[a]
}

func (f *fakeHS) setMember(room, user, m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[room+"|"+user] = m
}

// fakeTG records sendMessage texts and serves scripted getUpdates batches.
type fakeTG struct {
	mu      sync.Mutex
	texts   []string
	menu    []string // "method scope" of setMyCommands / deleteMyCommands
	updates chan string
}

func newFakeTG(t *testing.T) (*fakeTG, *httptest.Server) {
	f := &fakeTG{updates: make(chan string, 10)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.mu.Lock()
			f.texts = append(f.texts, r.PostForm.Get("text"))
			f.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":{}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			select {
			case b := <-f.updates:
				io.WriteString(w, b)
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
				io.WriteString(w, `{"ok":true,"result":[]}`)
			}
		default:
			f.mu.Lock()
			f.menu = append(f.menu, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]+" "+r.PostForm.Get("scope"))
			f.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeTG) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// testBot builds a bot against the fakes with quick retries.
func testBot(t *testing.T, hs, tg *httptest.Server, mutate func(*config.BotConfig), factories map[string]Factory) *Bot {
	t.Helper()
	mx := &config.MatrixConfig{Homeserver: hs.URL, ServerName: "ex.org", SenderLocalpart: "watch"}
	cfg := config.BotConfig{
		ID: "watch", User: "watch", Admin: "@admin:ex.org",
		Rooms: config.Rooms{
			Alerts: config.RoomConfig{Alias: "watch-alerts"},
			Query:  config.RoomConfig{Alias: "watch"},
		},
		Telegram:  &config.TelegramConfig{ChatID: 42, Commands: true},
		OutOfBand: []string{"self:"},
		Alerts: config.AlertConfig{
			Interval: config.Duration{Duration: time.Second},
			Sustain:  config.Duration{Duration: time.Second},
			Remind:   config.Duration{Duration: time.Hour},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(Options{
		Config: cfg, Host: "edge-1", MatrixCfg: mx,
		Matrix:    matrix.New(hs.URL, "as-token"),
		Telegram:  telegram.New(tg.URL, "1:x"),
		Factories: factories,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	b.retry.pause = time.Millisecond
	b.rooms.retry.pause = time.Millisecond
	b.errorPause = 10 * time.Millisecond
	b.syncTimeout = 100 * time.Millisecond
	return b
}
