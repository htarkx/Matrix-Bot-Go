package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

type role string

const (
	alertsRoom role = "alerts"
	queryRoom  role = "query"
)

// rooms finds or creates the bot's two rooms and posts to them.
//
// Both rooms are unencrypted and invite-only, found by alias:
//
//	alerts — alerts, recoveries, startup notices; kept; m.text (pushes);
//	         read-only for everyone but the bot
//	query  — commands and their replies; purged after a retention;
//	         replies as m.notice (the admin is looking at the room)
type rooms struct {
	me     *matrix.Identity
	mx     *config.MatrixConfig
	cfg    config.Rooms
	admin  string
	botID  string
	host   string
	log    *slog.Logger
	retry  retrier
	invite time.Duration // minimum time between re-invites

	mu         sync.Mutex // the watch loop and the sync loop both resolve rooms
	ids        map[role]string
	lastInvite map[role]time.Time
}

func (r *rooms) roomConfig(ro role) config.RoomConfig {
	if ro == alertsRoom {
		return r.cfg.Alerts
	}
	return r.cfg.Query
}

// readOnly are the alerts room's power levels: sending anything (messages,
// reactions), state, invites and redactions need 100, which only the bot has.
//
// Room v12 gives the creator unlimited power and rejects the creator in
// "users" — and createRoom fails only after the room already exists — so
// the bot never lists itself.
func readOnly() map[string]any {
	return map[string]any{
		"users": map[string]any{}, "users_default": 0.0,
		"events_default": 100.0, "state_default": 100.0,
		"invite": 100.0, "kick": 100.0, "ban": 100.0, "redact": 100.0,
		"notifications": map[string]any{"room": 100.0},
	}
}

func (r *rooms) powerLevels(ro role) map[string]any {
	if ro == alertsRoom {
		return readOnly()
	}
	return map[string]any{"users": map[string]any{r.admin: 100.0}}
}

// room returns the room ID for a role, finding or creating it on first use.
func (r *rooms) room(ctx context.Context, ro role) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.ids[ro]; ok {
		return id, nil
	}
	id, err := r.findOrCreate(ctx, ro)
	if err != nil {
		return "", err
	}
	if ro == alertsRoom {
		if err := r.enforcePowerLevels(ctx, id, ro); err != nil {
			return "", fmt.Errorf("power levels: %w", err)
		}
	}
	r.ids[ro] = id
	return id, nil
}

func (r *rooms) findOrCreate(ctx context.Context, ro role) (string, error) {
	rc := r.roomConfig(ro)
	alias := r.mx.Alias(rc.Alias)
	id, err := r.me.ResolveAlias(ctx, alias)
	if err == nil {
		return id, nil
	}
	if !matrix.IsStatus(err, http.StatusNotFound) {
		return "", fmt.Errorf("resolve %s: %w", alias, err)
	}
	name := rc.Name
	if name == "" {
		name = fmt.Sprintf("%s %s %s", r.host, r.botID, ro)
	}
	topic := rc.Topic
	if topic == "" {
		topic = r.defaultTopic(ro)
	}
	// No m.room.encryption: the bot speaks plaintext.
	id, err = r.me.CreateRoom(ctx, matrix.CreateRoom{
		Preset:      "private_chat",
		AliasName:   rc.Alias,
		Name:        name,
		Topic:       topic,
		Invite:      []string{r.admin},
		PowerLevels: r.powerLevels(ro),
	})
	if err != nil {
		return "", fmt.Errorf("create %s: %w", alias, err)
	}
	r.log.Info("created matrix room", "role", ro, "room", id, "alias", alias)
	return id, nil
}

func (r *rooms) defaultTopic(ro role) string {
	if ro == alertsRoom {
		return r.botID + " 告警（永久保留）"
	}
	t := r.botID + " 查询：发 h 看指令"
	if ret := r.cfg.Query.Retention.Duration; ret > 0 {
		span := human.Duration(ret)
		if ret%time.Hour == 0 {
			span = fmt.Sprintf("%d 小时", ret/time.Hour)
		}
		t += "（" + span + "后自动清除）"
	}
	return t
}

// enforcePowerLevels re-applies the role's power levels if they drifted.
func (r *rooms) enforcePowerLevels(ctx context.Context, roomID string, ro role) error {
	var current map[string]any
	if err := r.me.State(ctx, roomID, "m.room.power_levels", "", &current); err != nil {
		return err
	}
	want := make(map[string]any, len(current))
	for k, v := range current {
		want[k] = v
	}
	for k, v := range r.powerLevels(ro) {
		want[k] = v
	}
	// compare as JSON values: numbers from the server are float64
	if reflect.DeepEqual(normalize(want), normalize(current)) {
		return nil
	}
	if err := r.me.PutState(ctx, roomID, "m.room.power_levels", "", want); err != nil {
		return err
	}
	r.log.Info("reset room power levels", "role", ro, "room", roomID)
	return nil
}

func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	json.Unmarshal(b, &out)
	return out
}

// adminJoined reports whether the admin is in the room. Otherwise it
// re-invites them (at most every r.invite) and returns false: a message
// nobody reads must not count as delivered.
func (r *rooms) adminJoined(ctx context.Context, ro role) (bool, error) {
	id, err := r.room(ctx, ro)
	if err != nil {
		return false, err
	}
	m, err := r.me.Membership(ctx, id, r.admin)
	if err != nil {
		return false, err
	}
	if m == "join" {
		return true, nil
	}
	r.mu.Lock()
	due := m != "invite" && time.Since(r.lastInvite[ro]) > r.invite
	if due {
		r.lastInvite[ro] = time.Now()
	}
	r.mu.Unlock()
	if due {
		if err := r.me.Invite(ctx, id, r.admin, r.botID+" "+string(ro)+" 房间"); err != nil {
			return false, fmt.Errorf("re-invite: %w", err)
		}
		r.log.Info("re-invited admin", "role", ro, "membership", m)
	}
	return false, nil
}

// send posts msg and reports whether it was delivered: posted to a room the
// admin has joined. Without them the message is still posted (history for
// when they join) but reported undelivered.
func (r *rooms) send(ctx context.Context, ro role, msg message.Message, msgtype string) bool {
	joined, err := r.adminJoined(ctx, ro)
	if err != nil {
		r.log.Warn("matrix membership check failed", "role", ro, "err", err)
		return false
	}
	return r.post(ctx, ro, msg, msgtype) && joined
}

// reply posts the answer to a command the admin just sent in that room.
// Only joined members can send, so the command itself proves the admin is
// there: no membership round trip before the reply.
func (r *rooms) reply(ctx context.Context, ro role, msg message.Message) bool {
	return r.post(ctx, ro, msg, "m.notice")
}

// post sends msg, retrying network blips; true when the server took it.
func (r *rooms) post(ctx context.Context, ro role, msg message.Message, msgtype string) bool {
	id, err := r.room(ctx, ro)
	if err != nil {
		return false
	}
	err = r.retry.do(ctx, "matrix send", func() error {
		_, err := r.me.Send(ctx, id, "m.room.message", map[string]string{
			"msgtype":        msgtype,
			"body":           msg.Plain(),
			"format":         "org.matrix.custom.html",
			"formatted_body": msg.MatrixHTML(),
		})
		return err
	})
	return err == nil
}

// purge deletes query-room history older than the retention.
func (r *rooms) purge(ctx context.Context) error {
	id, err := r.room(ctx, queryRoom)
	if err != nil {
		return err
	}
	err = r.me.PurgeHistory(ctx, id, time.Now().Add(-r.cfg.Query.Retention.Duration))
	if errors.Is(err, matrix.ErrNothingToPurge) {
		return nil
	}
	return err
}

// syncFilter limits /sync to message events in the query room.
func syncFilter(roomID string) string {
	b, _ := json.Marshal(map[string]any{
		"room": map[string]any{
			"rooms":         []string{roomID},
			"timeline":      map[string]any{"limit": 20, "types": []string{"m.room.message"}},
			"state":         map[string]any{"lazy_load_members": true},
			"ephemeral":     map[string]any{"not_types": []string{"*"}},
			"account_data":  map[string]any{"not_types": []string{"*"}},
			"include_leave": false,
		},
		"presence":     map[string]any{"not_types": []string{"*"}},
		"account_data": map[string]any{"not_types": []string{"*"}},
	})
	return string(b)
}

// retrier retries an action a few times with growing pauses: network blips
// must not lose an alert.
type retrier struct {
	attempts int
	pause    time.Duration // multiplied by the attempt number
	log      *slog.Logger
}

func (rt retrier) do(ctx context.Context, what string, fn func() error) error {
	var err error
	for n := 1; n <= rt.attempts; n++ {
		if err = fn(); err == nil {
			return nil
		}
		rt.log.Warn(what+" failed", "attempt", n, "err", err)
		if n == rt.attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rt.pause * time.Duration(n)):
		}
	}
	return err
}

// hasAnyPrefix reports whether key starts with one of prefixes.
func hasAnyPrefix(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
