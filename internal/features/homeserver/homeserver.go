// Package homeserver reports on the Matrix homeserver itself, as its admin
// sees it: accounts and their activity, rooms, uploaded media. It reads the
// Synapse admin API (served by Tuwunel too), so the bot must be a server
// admin.
package homeserver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
	"github.com/htarkx/Matrix-Bot-Go/internal/synadmin"
)

// Config is the module's JSON configuration.
type Config struct {
	// HideUsers are localparts left out of user counts and lists (bots).
	HideUsers []string `json:"hide_users"`
}

// Module is the homeserver module.
type Module struct {
	bot.NoChecks
	bot.NoJobs
	cfg Config
	env bot.Env
	api synadmin.Getter
	now func() time.Time
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	var c Config
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
	}
	if env.Matrix == nil {
		return nil, errors.New("needs Matrix: the bot reads the homeserver's admin API")
	}
	return &Module{cfg: c, env: env, api: env.Matrix, now: time.Now}, nil
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{
		{Name: "users", Aliases: []string{"u"}, Help: "用户：最后在线、上传媒体", Run: m.cmdUsers},
		{Name: "rooms", Aliases: []string{"r"}, Help: "房间：人数、加密", Run: m.cmdRooms},
	}
}

func (m *Module) hidden(userID string) bool {
	local, _, _ := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	return slices.Contains(m.cfg.HideUsers, local)
}

func (m *Module) users(ctx context.Context) ([]synadmin.User, error) {
	all, err := synadmin.Users(ctx, m.api)
	if err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	return slices.DeleteFunc(all, func(u synadmin.User) bool { return u.Deactivated || m.hidden(u.Name) }), nil
}

// Summary implements bot.Summarizer.
func (m *Module) Summary(ctx context.Context) ([]string, error) {
	users, err := m.users(ctx)
	if err != nil {
		return nil, err
	}
	rooms, err := synadmin.Rooms(ctx, m.api)
	if err != nil {
		return nil, fmt.Errorf("rooms: %w", err)
	}
	media, err := synadmin.MediaStats(ctx, m.api)
	if err != nil {
		return nil, fmt.Errorf("media: %w", err)
	}
	now := m.now()
	active := func(d time.Duration) (n int) {
		for _, u := range users {
			if u.LastSeenTS > 0 && now.Sub(time.UnixMilli(u.LastSeenTS)) <= d {
				n++
			}
		}
		return n
	}
	occupied := 0
	for _, r := range rooms {
		if r.JoinedMembers > 0 {
			occupied++
		}
	}
	var files int
	var size int64
	for _, u := range media {
		files += u.Count
		size += u.Bytes
	}
	return []string{
		fmt.Sprintf("聊天  用户 %d（24h 活跃 %d · 7天 %d）· 房间 %d（有人 %d · 空 %d）",
			len(users), active(24*time.Hour), active(7*24*time.Hour), len(rooms), occupied, len(rooms)-occupied),
		fmt.Sprintf("媒体  %d 个文件 %s（homeserver 记账）", files, human.Bytes(size)),
	}, nil
}

func (m *Module) cmdUsers(ctx context.Context, _ bot.Request) (message.Message, error) {
	users, err := m.users(ctx)
	if err != nil {
		return message.Message{}, err
	}
	media, err := synadmin.MediaStats(ctx, m.api)
	if err != nil {
		return message.Message{}, fmt.Errorf("media: %w", err)
	}
	byUser := map[string]synadmin.UserMedia{}
	for _, u := range media {
		byUser[u.UserID] = u
	}
	slices.SortFunc(users, func(a, b synadmin.User) int { return cmp.Compare(b.LastSeenTS, a.LastSeenTS) })
	now := m.now()
	var rows []string
	for _, u := range users {
		local, _, _ := strings.Cut(strings.TrimPrefix(u.Name, "@"), ":")
		if u.Admin {
			local += "*"
		}
		seen := "从未"
		if u.LastSeenTS > 0 {
			seen = human.Duration(now.Sub(time.UnixMilli(u.LastSeenTS))) + "前"
		}
		md := byUser[u.Name]
		rows = append(rows, human.Pad(local, 12)+" "+human.Pad(seen, 16)+fmt.Sprintf("%3d 个 %7s", md.Count, human.Bytes(md.Bytes)))
	}
	return message.Message{
		Icon: "👥", Title: m.env.Host + " 用户", After: fmt.Sprintf("（%d 个）", len(users)),
		Pre:  strings.Join(rows, "\n"),
		Note: "* 服务器管理员 · 最后在线 · 上传媒体（homeserver 记账）",
	}, nil
}

func (m *Module) cmdRooms(ctx context.Context, _ bot.Request) (message.Message, error) {
	rooms, err := synadmin.Rooms(ctx, m.api)
	if err != nil {
		return message.Message{}, fmt.Errorf("rooms: %w", err)
	}
	var occupied []synadmin.Room
	emptyBy := map[string]int{}
	for _, r := range rooms {
		if r.JoinedMembers > 0 {
			occupied = append(occupied, r)
		} else {
			emptyBy[localpart(r.Creator)]++
		}
	}
	slices.SortStableFunc(occupied, func(a, b synadmin.Room) int { return cmp.Compare(b.JoinedMembers, a.JoinedMembers) })
	var rows []string
	for _, r := range occupied {
		name := r.DisplayName()
		if name == "" {
			name = m.unnamed(ctx, r.ID)
		}
		lock := "  "
		if r.Encryption != nil && *r.Encryption != "" {
			lock = "🔒"
		}
		rows = append(rows, fmt.Sprintf("%s %2d 人  %s", lock, r.JoinedMembers, name))
	}
	msg := message.Message{
		Icon: "🏠", Title: m.env.Host + " 房间",
		After: fmt.Sprintf("（%d 个，有人 %d）", len(rooms), len(occupied)),
		Pre:   strings.Join(rows, "\n"),
	}
	if n := len(rooms) - len(occupied); n > 0 {
		var by []string
		for _, c := range slices.Sorted(maps.Keys(emptyBy)) {
			by = append(by, fmt.Sprintf("%s %d", c, emptyBy[c]))
		}
		msg.Note = fmt.Sprintf("另有 %d 个空房间（按创建者：%s）", n, strings.Join(by, "，"))
	}
	return msg, nil
}

// unnamed describes a room without name or alias by its members, as
// clients do for direct chats.
func (m *Module) unnamed(ctx context.Context, roomID string) string {
	members, err := synadmin.Members(ctx, m.api, roomID)
	if err != nil || len(members) == 0 {
		return roomID
	}
	var names []string
	for _, u := range members {
		if !m.hidden(u) {
			names = append(names, localpart(u))
		}
	}
	slices.Sort(names)
	return "（" + strings.Join(names, "、") + "）"
}

func localpart(userID string) string {
	l, _, _ := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	if l == "" {
		return "?"
	}
	return l
}
