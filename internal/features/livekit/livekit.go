// Package livekit shows the calls running on a LiveKit SFU used for
// MatrixRTC (Element Call): which Matrix room, who, with what media. It
// calls LiveKit's RoomService (Twirp, JSON) with short-lived admin tokens.
package livekit

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
	"github.com/htarkx/Matrix-Bot-Go/internal/synadmin"
)

// Config is the module's JSON configuration.
type Config struct {
	// URL of LiveKit's HTTP API, e.g. http://127.0.0.1:7880.
	URL string `json:"url"`
	// KeysCredential holds "APIkey: secret" (LiveKit's key file format).
	KeysCredential string `json:"keys_credential"` // default "livekit_keys"
	// MetricsURL is LiveKit's Prometheus endpoint, e.g.
	// http://127.0.0.1:6789/metrics. Empty: no line quality, no loss alert.
	MetricsURL string `json:"metrics_url"`
	// MetricsEvery is the scrape interval (default 15s); MetricsWindow the
	// span reported and alerted on (default 1m).
	MetricsEvery  config.Duration `json:"metrics_every"`
	MetricsWindow config.Duration `json:"metrics_window"`
	// LossAlertPct alerts when mean audio loss in either direction over the
	// window reaches it (default 5; 0 = no alert).
	LossAlertPct *float64 `json:"loss_alert_pct"`
}

// Module is the livekit module.
type Module struct {
	cfg       Config
	env       bot.Env
	http      *http.Client
	rooms     synadmin.Getter // nil: Matrix room names unavailable
	now       func() time.Time
	metrics   *metrics // nil without metrics_url
	lossAlert float64
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.URL == "" {
		return nil, errors.New("url is required")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	if c.KeysCredential == "" {
		c.KeysCredential = "livekit_keys"
	}
	if c.MetricsEvery.Duration == 0 {
		c.MetricsEvery.Duration = 15 * time.Second
	}
	if c.MetricsWindow.Duration == 0 {
		c.MetricsWindow.Duration = time.Minute
	}
	if c.MetricsWindow.Duration < 2*c.MetricsEvery.Duration {
		return nil, errors.New("metrics_window must span at least two scrapes (metrics_every)")
	}
	m := &Module{cfg: c, env: env, http: &http.Client{Timeout: 10 * time.Second}, now: time.Now, lossAlert: 5}
	if c.LossAlertPct != nil {
		m.lossAlert = *c.LossAlertPct
	}
	if env.Matrix != nil {
		m.rooms = env.Matrix
	}
	if c.MetricsURL != "" {
		m.metrics = &metrics{url: c.MetricsURL, window: c.MetricsWindow.Duration, http: m.http}
	}
	return m, nil
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{{Name: "calls", Aliases: []string{"c"}, Help: "正在进行的通话", Run: m.cmdCalls}}
}

// Call is one LiveKit room with people in it.
type Call struct {
	Room         string // Matrix room name, or the LiveKit room name
	Since        time.Time
	Participants []Participant
}

// Participant is one connection.
type Participant struct {
	Who    string // Matrix localpart when known
	Since  time.Time
	Audio  bool
	Video  bool
	Screen bool
}

type lkRoom struct {
	Name            string `json:"name"`
	NumParticipants int    `json:"num_participants"`
	CreationTime    string `json:"creation_time"` // int64 seconds, as protojson writes it
}

type lkParticipant struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	JoinedAt string `json:"joined_at"`
	Tracks   []struct {
		Source string `json:"source"`
		Muted  bool   `json:"muted"`
	} `json:"tracks"`
}

// calls lists the rooms that have participants.
func (m *Module) calls(ctx context.Context) ([]Call, error) {
	var lr struct {
		Rooms []lkRoom `json:"rooms"`
	}
	if err := m.twirp(ctx, "ListRooms", map[string]any{"roomList": true}, struct{}{}, &lr); err != nil {
		return nil, err
	}
	var names map[string]string
	var out []Call
	for _, r := range lr.Rooms {
		if r.NumParticipants == 0 {
			continue
		}
		if names == nil {
			names = m.matrixRooms(ctx)
		}
		c := Call{Room: cmp.Or(names[r.Name], "LiveKit "+r.Name), Since: unix(r.CreationTime)}
		var lp struct {
			Participants []lkParticipant `json:"participants"`
		}
		grant := map[string]any{"roomAdmin": true, "room": r.Name}
		if err := m.twirp(ctx, "ListParticipants", grant, map[string]string{"room": r.Name}, &lp); err != nil {
			return nil, err
		}
		for _, p := range lp.Participants {
			pp := Participant{Who: who(p), Since: unix(p.JoinedAt)}
			for _, t := range p.Tracks {
				if t.Muted {
					continue
				}
				switch t.Source {
				case "MICROPHONE":
					pp.Audio = true
				case "CAMERA":
					pp.Video = true
				case "SCREEN_SHARE", "SCREEN_SHARE_AUDIO":
					pp.Screen = true
				}
			}
			c.Participants = append(c.Participants, pp)
		}
		slices.SortFunc(c.Participants, func(a, b Participant) int { return a.Since.Compare(b.Since) })
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Call) int { return a.Since.Compare(b.Since) })
	return out, nil
}

// who turns a LiveKit identity into a Matrix localpart. lk-jwt-service's
// legacy endpoint uses "@user:server:DEVICE"; its MSC4195 endpoints use an
// opaque hash, where only the display name (if any) is left.
func who(p lkParticipant) string {
	if strings.HasPrefix(p.Identity, "@") {
		l, _, _ := strings.Cut(p.Identity[1:], ":")
		return l
	}
	return cmp.Or(p.Name, "（未知）")
}

// RoomAlias is the LiveKit room lk-jwt-service uses for a Matrix room's
// room-wide call: unpadded standard base64 of SHA-256 over the JSON array
// [roomID, "m.call#room"] (MSC4195).
func RoomAlias(matrixRoomID string) string {
	b, _ := json.Marshal([]string{matrixRoomID, "m.call#room"})
	sum := sha256.Sum256(b)
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// matrixRooms maps LiveKit room aliases to Matrix room names; empty when
// the homeserver cannot be asked.
func (m *Module) matrixRooms(ctx context.Context) map[string]string {
	out := map[string]string{}
	if m.rooms == nil {
		return out
	}
	rooms, err := synadmin.Rooms(ctx, m.rooms)
	if err != nil {
		m.env.Log.Warn("cannot map calls to Matrix rooms", "err", err)
		return out
	}
	for _, r := range rooms {
		if r.JoinedMembers == 0 {
			continue // nobody can be calling there
		}
		name := r.DisplayName()
		if name == "" {
			if members, err := synadmin.Members(ctx, m.rooms, r.ID); err == nil && len(members) > 0 {
				var ls []string
				for _, u := range members {
					l, _, _ := strings.Cut(strings.TrimPrefix(u, "@"), ":")
					ls = append(ls, l)
				}
				slices.Sort(ls)
				name = "（" + strings.Join(ls, "、") + "）"
			} else {
				name = r.ID
			}
		}
		out[RoomAlias(r.ID)] = name
	}
	return out
}

func unix(s string) time.Time {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// twirp calls a RoomService method with a token carrying the given grant.
func (m *Module) twirp(ctx context.Context, method string, grant map[string]any, body, out any) error {
	tok, err := m.token(grant)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.URL+"/twirp/livekit.RoomService/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %.200s", method, resp.StatusCode, raw)
	}
	return json.Unmarshal(raw, out)
}

// token signs a one-minute HS256 access token.
func (m *Module) token(grant map[string]any) (string, error) {
	keys, err := m.env.Secret(m.cfg.KeysCredential)
	if err != nil {
		return "", err
	}
	key, secret, ok := strings.Cut(strings.TrimSpace(keys), ":")
	key, secret = strings.TrimSpace(key), strings.TrimSpace(secret)
	if !ok || key == "" || secret == "" {
		return "", fmt.Errorf("credential %s is not \"key: secret\"", m.cfg.KeysCredential)
	}
	now := m.now().Unix()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": key, "sub": m.env.BotID, "nbf": now - 5, "exp": now + 60, "video": grant})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Summary implements bot.Summarizer.
func (m *Module) Summary(ctx context.Context) ([]string, error) {
	calls, err := m.calls(ctx)
	if err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return []string{"通话  没有进行中的通话"}, nil
	}
	var parts []string
	for _, c := range calls {
		parts = append(parts, fmt.Sprintf("%s %d 人", c.Room, len(c.Participants)))
	}
	lines := []string{"通话  " + strings.Join(parts, " · ")}
	if l := m.qualityLine(); l != "" {
		lines = append(lines, "      "+l)
	}
	return lines, nil
}

// qualityLine is the recent audio line quality, "" when unknown.
func (m *Module) qualityLine() string {
	if m.metrics == nil {
		return ""
	}
	if q, ok := m.metrics.quality(); ok {
		return q.line()
	}
	return "线路统计中"
}

func (m *Module) cmdCalls(ctx context.Context, _ bot.Request) (message.Message, error) {
	calls, err := m.calls(ctx)
	if err != nil {
		return message.Message{}, err
	}
	msg := message.Message{Icon: "📞", Title: "[" + m.env.Host + "] 通话"}
	if len(calls) == 0 {
		msg.After = "：没有进行中的通话"
		return msg, nil
	}
	now := m.now()
	var rows []string
	for _, c := range calls {
		head := c.Room
		if !c.Since.IsZero() {
			head += "（" + human.Duration(now.Sub(c.Since)) + "）"
		}
		rows = append(rows, head)
		for _, p := range c.Participants {
			media := ""
			for _, x := range []struct {
				on   bool
				icon string
			}{{p.Audio, "🎤"}, {p.Video, "📷"}, {p.Screen, "🖥"}} {
				if x.on {
					media += x.icon
				}
			}
			if media == "" {
				media = "静音"
			}
			since := ""
			if !p.Since.IsZero() {
				since = human.Duration(now.Sub(p.Since))
			}
			rows = append(rows, "  "+human.Pad(p.Who, 12)+" "+human.Pad(media, 6)+" "+since)
		}
	}
	if l := m.qualityLine(); l != "" {
		rows = append(rows, "", "线路  "+l)
	}
	msg.Pre = strings.Join(rows, "\n")
	if m.metrics != nil {
		msg.Note = "上行 = 各人到服务器，下行 = 服务器到各人；所有人合计，不分人"
	}
	return msg, nil
}
