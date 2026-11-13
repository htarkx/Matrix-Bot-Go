// Package matrix is a small Matrix client-server API client for appservice bots.
//
// One Client holds the appservice's as_token. Identities derived from it act
// as the appservice's own user or, through ?user_id= (identity assertion), as
// one of its virtual users.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// RequestTimeout bounds every request whose context has no deadline.
const RequestTimeout = 20 * time.Second

// Client talks to one homeserver with one appservice token.
type Client struct {
	base  string
	token string
	http  *http.Client

	// Transaction IDs must never repeat for a token, or the server silently
	// drops the event as a retry. Several identities share the token, so the
	// counter is per client, prefixed with the start time to survive restarts.
	txnPrefix string
	txn       atomic.Uint64
}

// New returns a client for the homeserver base URL (e.g. http://127.0.0.1:8008).
func New(homeserver, asToken string) *Client {
	return &Client{
		base:      strings.TrimRight(homeserver, "/"),
		token:     asToken,
		http:      &http.Client{},
		txnPrefix: "mbg" + strconv.FormatInt(time.Now().UnixNano(), 36),
	}
}

func (c *Client) nextTxn() string {
	return c.txnPrefix + "." + strconv.FormatUint(c.txn.Add(1), 10)
}

// Identity is the client acting as one user.
type Identity struct {
	c      *Client
	UserID string
	// assert adds ?user_id= to every request. False for the appservice's own
	// user (sender_localpart), which the token already identifies.
	assert bool
}

// As returns the identity for userID. Set virtual for any user other than the
// appservice's sender_localpart.
func (c *Client) As(userID string, virtual bool) *Identity {
	return &Identity{c: c, UserID: userID, assert: virtual}
}

// Error is a non-2xx answer. Matrix puts errcode/error in the body.
type Error struct {
	Status  int
	ErrCode string
	Message string
	Body    string
}

func (e *Error) Error() string {
	if e.ErrCode != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.ErrCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// IsStatus reports whether err is a Matrix error with that HTTP status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// do sends a JSON request and decodes a JSON answer into out (when non-nil).
func (i *Identity) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	if query == nil {
		query = url.Values{}
	}
	if i.assert {
		query.Set("user_id", i.UserID)
	}
	u := i.c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := i.c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode, Body: truncate(string(data), 300)}
		var mx struct {
			ErrCode string `json:"errcode"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(data, &mx) == nil {
			e.ErrCode, e.Message = mx.ErrCode, mx.Error
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func esc(s string) string { return url.PathEscape(s) }

// Register creates the virtual user if it does not exist yet. The
// appservice's own user exists implicitly and must not be registered.
func (i *Identity) Register(ctx context.Context) error {
	localpart := strings.TrimPrefix(strings.SplitN(i.UserID, ":", 2)[0], "@")
	// Registration runs as the appservice itself: no user_id assertion for a
	// user that does not exist yet.
	self := &Identity{c: i.c, UserID: i.UserID}
	err := self.do(ctx, http.MethodPost, "/_matrix/client/v3/register", nil, map[string]any{
		"type":          "m.login.application_service",
		"username":      localpart,
		"inhibit_login": true,
	}, nil)
	var e *Error
	if errors.As(err, &e) && e.ErrCode == "M_USER_IN_USE" {
		return nil
	}
	return err
}

// ResolveAlias returns the room ID for #alias:server, or an *Error with
// status 404 when the alias does not exist.
func (i *Identity) ResolveAlias(ctx context.Context, alias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := i.do(ctx, http.MethodGet, "/_matrix/client/v3/directory/room/"+esc(alias), nil, nil, &out)
	return out.RoomID, err
}

// CreateRoom is the subset of the createRoom body the bots use.
type CreateRoom struct {
	Preset      string         `json:"preset,omitempty"`
	AliasName   string         `json:"room_alias_name,omitempty"`
	Name        string         `json:"name,omitempty"`
	Topic       string         `json:"topic,omitempty"`
	Invite      []string       `json:"invite,omitempty"`
	PowerLevels map[string]any `json:"power_level_content_override,omitempty"`
}

// CreateRoom creates a room and returns its ID.
func (i *Identity) CreateRoom(ctx context.Context, req CreateRoom) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := i.do(ctx, http.MethodPost, "/_matrix/client/v3/createRoom", nil, req, &out)
	return out.RoomID, err
}

// State reads one state event's content.
func (i *Identity) State(ctx context.Context, roomID, evType, stateKey string, out any) error {
	return i.do(ctx, http.MethodGet,
		"/_matrix/client/v3/rooms/"+esc(roomID)+"/state/"+esc(evType)+"/"+esc(stateKey), nil, nil, out)
}

// PutState replaces one state event.
func (i *Identity) PutState(ctx context.Context, roomID, evType, stateKey string, content any) error {
	return i.do(ctx, http.MethodPut,
		"/_matrix/client/v3/rooms/"+esc(roomID)+"/state/"+esc(evType)+"/"+esc(stateKey), nil, content, nil)
}

// Membership returns userID's membership in the room ("join", "invite",
// "leave", "ban"), or "none" when they never had one.
func (i *Identity) Membership(ctx context.Context, roomID, userID string) (string, error) {
	var out struct {
		Membership string `json:"membership"`
	}
	err := i.State(ctx, roomID, "m.room.member", userID, &out)
	if IsStatus(err, http.StatusNotFound) {
		return "none", nil
	}
	return out.Membership, err
}

// Invite invites userID.
func (i *Identity) Invite(ctx context.Context, roomID, userID, reason string) error {
	return i.do(ctx, http.MethodPost, "/_matrix/client/v3/rooms/"+esc(roomID)+"/invite", nil,
		map[string]string{"user_id": userID, "reason": reason}, nil)
}

// Send sends a message event and returns its event ID.
func (i *Identity) Send(ctx context.Context, roomID, evType string, content any) (string, error) {
	var out struct {
		EventID string `json:"event_id"`
	}
	err := i.do(ctx, http.MethodPut,
		"/_matrix/client/v3/rooms/"+esc(roomID)+"/send/"+esc(evType)+"/"+esc(i.c.nextTxn()), nil, content, &out)
	return out.EventID, err
}

// SyncResponse is the part of /sync the bots read.
type SyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []Event `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

// Event is a timeline event.
type Event struct {
	Type    string          `json:"type"`
	Sender  string          `json:"sender"`
	EventID string          `json:"event_id"`
	Content json.RawMessage `json:"content"`
}

// Sync long-polls /sync. since == "" is the initial sync (returns at once).
func (i *Identity) Sync(ctx context.Context, since, filter string, timeout time.Duration) (*SyncResponse, error) {
	q := url.Values{"filter": {filter}, "timeout": {"0"}}
	if since != "" {
		q.Set("since", since)
		q.Set("timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	var out SyncResponse
	if err := i.do(ctx, http.MethodGet, "/_matrix/client/v3/sync", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrNothingToPurge means the room has no events older than the cut-off.
var ErrNothingToPurge = errors.New("nothing to purge")

// PurgeHistory deletes room history older than upTo from the database
// (Synapse admin API, also served by Tuwunel; needs a server admin) and
// waits up to two minutes for the purge to finish.
func (i *Identity) PurgeHistory(ctx context.Context, roomID string, upTo time.Time) error {
	var start struct {
		PurgeID string `json:"purge_id"`
	}
	err := i.do(ctx, http.MethodPost, "/_synapse/admin/v1/purge_history/"+esc(roomID), nil, map[string]any{
		"purge_up_to_ts":      upTo.UnixMilli(),
		"delete_local_events": true, // every sender in a bot room is local
	}, &start)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound && strings.Contains(e.Body, "before the given timestamp") {
		return ErrNothingToPurge
	}
	if err != nil {
		return fmt.Errorf("purge_history: %w", err)
	}
	for range 60 {
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		err := i.do(ctx, http.MethodGet, "/_synapse/admin/v1/purge_history_status/"+esc(start.PurgeID), nil, nil, &st)
		if err != nil {
			return fmt.Errorf("purge status: %w", err)
		}
		switch st.Status {
		case "complete":
			return nil
		case "failed":
			return fmt.Errorf("purge failed: %s", st.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("purge %s still running after 2 min", start.PurgeID)
}
