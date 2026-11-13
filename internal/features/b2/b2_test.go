package b2

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
)

func TestListing(t *testing.T) {
	calls := map[string]int{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		if r.URL.Path == "/auth" {
			if id, key, ok := r.BasicAuth(); !ok || id != "kid" || key != "k" {
				w.WriteHeader(401)
				io.WriteString(w, `{"code":"unauthorized","message":"bad key"}`)
				return
			}
			io.WriteString(w, `{"accountId":"acc","authorizationToken":"tok","apiInfo":{"storageApi":{"apiUrl":"`+srv.URL+`"}}}`)
			return
		}
		if r.Header.Get("Authorization") != "tok" {
			t.Errorf("%s without token", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/b2api/v3/b2_list_buckets":
			if body["accountId"] != "acc" {
				t.Errorf("list_buckets body %v", body)
			}
			io.WriteString(w, `{"buckets":[{"bucketId":"b1","bucketName":"media"},{"bucketId":"b2","bucketName":"backup"}]}`)
		case "/b2api/v3/b2_list_file_versions":
			switch {
			case body["bucketId"] == "b1":
				io.WriteString(w, `{"files":[{"fileName":"a","contentLength":100,"action":"upload"}],"nextFileName":null,"nextFileId":null}`)
			case body["startFileName"] == nil:
				// a: current 10 + old 20; b: hidden (hide marker newest) with a 5-byte old version
				io.WriteString(w, `{"files":[
					{"fileName":"a","contentLength":10,"action":"upload"},
					{"fileName":"a","contentLength":20,"action":"upload"},
					{"fileName":"b","contentLength":0,"action":"hide"}],
					"nextFileName":"b","nextFileId":"id-b2"}`)
			default:
				if body["startFileName"] != "b" || body["startFileId"] != "id-b2" {
					t.Errorf("page 2 body %v", body)
				}
				// the listing resumes inside "b": not a new name
				io.WriteString(w, `{"files":[
					{"fileName":"b","contentLength":5,"action":"upload"},
					{"fileName":"c","contentLength":1000,"action":"upload"}],
					"nextFileName":null,"nextFileId":null}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	env := bot.Env{Host: "edge-1", Log: slog.New(slog.DiscardHandler), Secret: func(n string) (string, error) {
		return map[string]string{"b2_key_id": "kid", "b2_key": "k"}[n], nil
	}}
	mod, err := New(env, json.RawMessage(`{"auth_url":"`+srv.URL+`/auth"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := mod.(*Module)
	if sum, _ := m.Summary(context.Background()); sum[0] != "存储  B2 统计中" {
		t.Errorf("before first listing: %q", sum)
	}
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.last.Load().buckets
	want := []Bucket{
		{Name: "backup", Files: 2, Bytes: 1010, Versions: 4, VersionBytes: 1035, Hidden: 1},
		{Name: "media", Files: 1, Bytes: 100, Versions: 1, VersionBytes: 100},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("buckets\n got %+v\nwant %+v", got, want)
	}
	sum, _ := m.Summary(context.Background())
	if !strings.HasPrefix(sum[0], "存储  B2 共 1K（2 个桶，") || !strings.Contains(sum[1], "backup 1010B（旧版本另占 25B）") {
		t.Errorf("summary %q", sum)
	}
	if calls["/b2api/v3/b2_list_file_versions"] != 3 {
		t.Errorf("list calls %d", calls["/b2api/v3/b2_list_file_versions"])
	}
}

// A key restricted to a bucket lists exactly that one, without
// b2_list_buckets (which B2 refuses for such a key unless the bucket is
// named). Response shape as returned by b2api/v3 for such a key.
func TestRestrictedKey(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth":
			io.WriteString(w, `{"accountId":"acc","authorizationToken":"tok","apiInfo":{"storageApi":{"apiUrl":"`+srv.URL+`",
				"bucketId":"b9","bucketName":"only","capabilities":["listBuckets","listFiles"],"namePrefix":null}}}`)
		case "/b2api/v3/b2_list_buckets":
			w.WriteHeader(401)
			io.WriteString(w, `{"code":"unauthorized","message":""}`)
		case "/b2api/v3/b2_list_file_versions":
			io.WriteString(w, `{"files":[{"fileName":"x","contentLength":7,"action":"upload"}],"nextFileName":null,"nextFileId":null}`)
		}
	}))
	defer srv.Close()
	env := bot.Env{Secret: func(string) (string, error) { return "x", nil }}
	mod, _ := New(env, json.RawMessage(`{"auth_url":"`+srv.URL+`/auth"}`))
	m := mod.(*Module)
	if err := m.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.last.Load().buckets; len(got) != 1 || got[0].Name != "only" || got[0].Bytes != 7 {
		t.Fatalf("buckets %+v", got)
	}
}

func TestAuthErrorKeepsLastListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"code":"bad_auth_token","message":"nope"}`)
	}))
	defer srv.Close()
	env := bot.Env{Secret: func(string) (string, error) { return "x", nil }}
	mod, _ := New(env, json.RawMessage(`{"auth_url":"`+srv.URL+`"}`))
	m := mod.(*Module)
	if err := m.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "bad_auth_token") {
		t.Fatalf("err %v", err)
	}
	if sum, _ := m.Summary(context.Background()); !strings.Contains(sum[0], "读取失败") {
		t.Errorf("summary %q", sum)
	}
}

func TestRefreshFloor(t *testing.T) {
	if _, err := New(bot.Env{}, json.RawMessage(`{"refresh":"1m"}`)); err == nil {
		t.Fatal("1m refresh accepted")
	}
}
