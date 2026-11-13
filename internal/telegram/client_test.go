package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	var got []url.Values
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = append(got, r.PostForm)
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			io.WriteString(w, `{"ok":true,"result":[{"update_id":7,"message":{"date":100,"text":"/status","chat":{"id":42}}}]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "123:abc")
	ctx := context.Background()

	ups, err := c.GetUpdates(ctx, 5, 50*time.Second)
	if err != nil || len(ups) != 1 || ups[0].UpdateID != 7 || ups[0].Message.Chat.ID != 42 || ups[0].Message.Text != "/status" {
		t.Fatalf("updates = %+v, %v", ups, err)
	}
	if paths[0] != "/bot123:abc/getUpdates" || got[0].Get("offset") != "5" || got[0].Get("timeout") != "50" {
		t.Errorf("getUpdates request %s %v", paths[0], got[0])
	}

	err = c.SendHTML(ctx, 42, "<b>x</b>")
	var te *Error
	if !errors.As(err, &te) || te.Code != 400 {
		t.Errorf("send error = %v", err)
	}
	if f := got[1]; f.Get("chat_id") != "42" || f.Get("parse_mode") != "HTML" || f.Get("text") != "<b>x</b>" {
		t.Errorf("sendMessage form %v", f)
	}

	if err := c.SetMyCommands(ctx, []Command{{"status", "状态"}}, ChatScope(42)); err != nil {
		t.Fatal(err)
	}
	if f := got[2]; f.Get("commands") != `[{"command":"status","description":"状态"}]` ||
		f.Get("scope") != `{"chat_id":42,"type":"chat"}` {
		t.Errorf("setMyCommands form = %v", f)
	}
	if err := c.DeleteMyCommands(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if paths[3] != "/bot123:abc/deleteMyCommands" || got[3].Has("scope") {
		t.Errorf("deleteMyCommands %s %v", paths[3], got[3])
	}
}

func TestTransportErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused
	err := New(srv.URL, "123:SECRET").SendHTML(context.Background(), 1, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
}
