package livekit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

// Expected value computed independently (Python: base64 of sha256 over the
// compact JSON array); the formula was checked against lk-jwt-service 0.10.0.
func TestRoomAlias(t *testing.T) {
	got := RoomAlias("!Q2xvc2VUaGVEb29y:example.org")
	if want := "mTd4Fv2Ce+tR+K+38warWYKlwweOO6QDw27zMnOx470"; got != want {
		t.Fatalf("RoomAlias = %s, want %s", got, want)
	}
}

type fakeAdmin struct{}

func (fakeAdmin) Get(_ context.Context, path string, _ url.Values, out any) error {
	var body string
	switch {
	case path == "/_synapse/admin/v1/rooms":
		body = `{"rooms":[
			{"room_id":"!named:ex.org","name":"家庭群","joined_members":3},
			{"room_id":"!dm:ex.org","name":null,"joined_members":2},
			{"room_id":"!empty:ex.org","name":null,"joined_members":0}],"next_batch":null}`
	case strings.HasSuffix(path, "/members"):
		body = `{"members":["@bob:ex.org","@alice:ex.org"]}`
	default:
		body = `{}`
	}
	return json.Unmarshal([]byte(body), out)
}

func verify(t *testing.T, tok, secret string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		t.Fatal("bad signature")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(raw, &claims)
	return claims
}

func TestCalls(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	named, dm := RoomAlias("!named:ex.org"), RoomAlias("!dm:ex.org")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := verify(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "s3cret")
		if claims["iss"] != "APIkey" {
			t.Errorf("iss %v", claims["iss"])
		}
		video := claims["video"].(map[string]any)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/twirp/livekit.RoomService/ListRooms":
			if video["roomList"] != true {
				t.Error("ListRooms without roomList grant")
			}
			io.WriteString(w, `{"rooms":[
				{"name":"`+named+`","num_participants":2,"creation_time":"1799999400"},
				{"name":"`+dm+`","num_participants":0,"creation_time":"1799999000"},
				{"name":"stranger","num_participants":1,"creation_time":"1799999900"}]}`)
		case "/twirp/livekit.RoomService/ListParticipants":
			var req struct{ Room string }
			json.Unmarshal(body, &req)
			if video["roomAdmin"] != true || video["room"] != req.Room {
				t.Errorf("ListParticipants grant %v for %s", video, req.Room)
			}
			if req.Room == named {
				io.WriteString(w, `{"participants":[
					{"identity":"@bob:ex.org:DEV2","joined_at":"1799999700","tracks":[{"source":"MICROPHONE"},{"source":"CAMERA","muted":true}]},
					{"identity":"@alice:ex.org:DEV1","joined_at":"1799999400","tracks":[{"source":"MICROPHONE"},{"source":"SCREEN_SHARE"}]}]}`)
			} else {
				io.WriteString(w, `{"participants":[{"identity":"opaquehash","name":"","joined_at":"1799999900"}]}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	env := bot.Env{Host: "edge-1", BotID: "watch", Log: slog.New(slog.DiscardHandler),
		Secret: func(string) (string, error) { return "APIkey: s3cret\n", nil }}
	mod, err := New(env, json.RawMessage(`{"url":"`+srv.URL+`/"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	m.rooms = fakeAdmin{}
	m.now = func() time.Time { return now }

	sum, err := m.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "通话  家庭群 2 人 · LiveKit stranger 1 人"; sum[0] != want {
		t.Errorf("summary %q, want %q", sum[0], want)
	}
	msg, err := m.cmdCalls(context.Background(), bot.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"家庭群（10 分钟）", "alice        🎤🖥", "bob          🎤 ", "（未知）"} {
		if !strings.Contains(msg.Pre, want) {
			t.Errorf("calls missing %q:\n%s", want, msg.Pre)
		}
	}
	if strings.Index(msg.Pre, "alice") > strings.Index(msg.Pre, "bob") {
		t.Error("participants not in join order")
	}
}

func TestBadKeyFile(t *testing.T) {
	env := bot.Env{Secret: func(string) (string, error) { return "no-colon", nil }}
	mod, _ := New(env, json.RawMessage(`{"url":"http://127.0.0.1:1"}`))
	if _, err := mod.(*Module).Summary(context.Background()); err == nil || !strings.Contains(err.Error(), "key: secret") {
		t.Fatalf("err = %v", err)
	}
}
