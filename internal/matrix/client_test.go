package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake homeserver: it answers from a handler map keyed by
// "METHOD path-prefix" and remembers every request.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func newServer(t *testing.T, routes map[string]http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r)
		rec.body = append(rec.body, string(b))
		rec.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer as-token" {
			http.Error(w, `{"errcode":"M_UNKNOWN_TOKEN"}`, http.StatusUnauthorized)
			return
		}
		best := ""
		for k := range routes {
			m, p, _ := strings.Cut(k, " ")
			if m == r.Method && strings.HasPrefix(r.URL.EscapedPath(), p) && len(k) > len(best) {
				best = k
			}
		}
		if best == "" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_UNRECOGNIZED","error":"no route"}`)
			return
		}
		routes[best](w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "as-token"), rec
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestIdentityAssertion(t *testing.T) {
	c, rec := newServer(t, map[string]http.HandlerFunc{
		"PUT /_matrix/client/v3/rooms/": reply(200, `{"event_id":"$e"}`),
	})
	ctx := context.Background()
	if _, err := c.As("@bot:ex.org", false).Send(ctx, "!r:ex.org", "m.room.message", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.As("@bot-2:ex.org", true).Send(ctx, "!r:ex.org", "m.room.message", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if q := rec.reqs[0].URL.Query(); q.Has("user_id") {
		t.Errorf("own user asserted a user_id: %v", q)
	}
	if got := rec.reqs[1].URL.Query().Get("user_id"); got != "@bot-2:ex.org" {
		t.Errorf("virtual user_id = %q", got)
	}
	// room IDs contain '!' and ':' — they must arrive as one escaped segment
	if p := rec.reqs[0].URL.EscapedPath(); !strings.HasPrefix(p, "/_matrix/client/v3/rooms/%21r:ex.org/send/m.room.message/") {
		t.Errorf("path = %s", p)
	}
}

func TestTxnIDsNeverRepeat(t *testing.T) {
	c, rec := newServer(t, map[string]http.HandlerFunc{
		"PUT /_matrix/client/v3/rooms/": reply(200, `{"event_id":"$e"}`),
	})
	var wg sync.WaitGroup
	for n := range 20 {
		wg.Go(func() {
			// two identities share the token, so they share the txn space
			id := c.As("@a:ex.org", n%2 == 0)
			if _, err := id.Send(context.Background(), "!r:ex.org", "m.room.message", map[string]string{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, r := range rec.reqs {
		txn := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if seen[txn] {
			t.Fatalf("txn %s reused", txn)
		}
		seen[txn] = true
	}
	if len(seen) != 20 {
		t.Fatalf("%d txns", len(seen))
	}
}

func TestErrors(t *testing.T) {
	c, _ := newServer(t, map[string]http.HandlerFunc{
		"GET /_matrix/client/v3/directory/room/":                        reply(404, `{"errcode":"M_NOT_FOUND","error":"Room alias not found"}`),
		"GET /_matrix/client/v3/rooms/%21r:ex.org/state/m.room.member/": reply(404, `{"errcode":"M_NOT_FOUND"}`),
		"POST /_matrix/client/v3/register":                              reply(400, `{"errcode":"M_USER_IN_USE","error":"taken"}`),
	})
	ctx := context.Background()
	me := c.As("@bot:ex.org", false)

	_, err := me.ResolveAlias(ctx, "#x:ex.org")
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 || e.ErrCode != "M_NOT_FOUND" || !IsStatus(err, 404) {
		t.Errorf("resolve: %v", err)
	}
	if m, err := me.Membership(ctx, "!r:ex.org", "@admin:ex.org"); err != nil || m != "none" {
		t.Errorf("membership = %q, %v", m, err)
	}
	if err := c.As("@bot-2:ex.org", true).Register(ctx); err != nil {
		t.Errorf("register of an existing user: %v", err)
	}
}

func TestRegisterBody(t *testing.T) {
	c, rec := newServer(t, map[string]http.HandlerFunc{
		"POST /_matrix/client/v3/register": reply(200, `{"user_id":"@bot-2:ex.org"}`),
	})
	if err := c.As("@bot-2:ex.org", true).Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal([]byte(rec.body[0]), &body)
	if body["type"] != "m.login.application_service" || body["username"] != "bot-2" || body["inhibit_login"] != true {
		t.Errorf("body = %v", body)
	}
	if rec.reqs[0].URL.Query().Has("user_id") {
		t.Error("register must not assert the not-yet-existing user")
	}
}

func TestSync(t *testing.T) {
	c, rec := newServer(t, map[string]http.HandlerFunc{
		"GET /_matrix/client/v3/sync": reply(200, `{"next_batch":"s2","rooms":{"join":{"!q:ex.org":{"timeline":{"events":[
			{"type":"m.room.message","sender":"@admin:ex.org","event_id":"$1","content":{"msgtype":"m.text","body":"s"}}]}}}}}`),
	})
	me := c.As("@bot:ex.org", false)
	if _, err := me.Sync(context.Background(), "", "{}", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	res, err := me.Sync(context.Background(), "s1", "{}", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if q := rec.reqs[0].URL.Query(); q.Get("timeout") != "0" || q.Has("since") {
		t.Errorf("initial sync query = %v", q)
	}
	if q := rec.reqs[1].URL.Query(); q.Get("timeout") != "30000" || q.Get("since") != "s1" {
		t.Errorf("incremental sync query = %v", q)
	}
	evs := res.Rooms.Join["!q:ex.org"].Timeline.Events
	if res.NextBatch != "s2" || len(evs) != 1 || evs[0].Sender != "@admin:ex.org" {
		t.Errorf("sync = %+v", res)
	}
}

func TestPurge(t *testing.T) {
	polls := 0
	c, rec := newServer(t, map[string]http.HandlerFunc{
		"POST /_synapse/admin/v1/purge_history/": reply(200, `{"purge_id":"p1"}`),
		"GET /_synapse/admin/v1/purge_history_status/p1": func(w http.ResponseWriter, _ *http.Request) {
			polls++
			io.WriteString(w, `{"status":"complete"}`)
		},
	})
	upTo := time.UnixMilli(1_700_000_000_000)
	if err := c.As("@bot:ex.org", false).PurgeHistory(context.Background(), "!q:ex.org", upTo); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || !strings.Contains(rec.body[0], `"purge_up_to_ts":1700000000000`) ||
		!strings.Contains(rec.body[0], `"delete_local_events":true`) {
		t.Errorf("polls=%d body=%s", polls, rec.body[0])
	}

	c2, _ := newServer(t, map[string]http.HandlerFunc{
		"POST /_synapse/admin/v1/purge_history/": reply(404,
			`{"errcode":"M_NOT_FOUND","error":"There is no event to be purged before the given timestamp"}`),
	})
	if err := c2.As("@bot:ex.org", false).PurgeHistory(context.Background(), "!q:ex.org", upTo); !errors.Is(err, ErrNothingToPurge) {
		t.Errorf("empty purge: %v", err)
	}
}
