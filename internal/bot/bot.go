// Package bot runs one bot: its modules' checks feed an alert engine, alerts
// go to Matrix with Telegram as the out-of-band channel, and commands arrive
// from the Matrix query room and from Telegram.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/alert"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
	"github.com/htarkx/Matrix-Bot-Go/internal/telegram"
)

// Options are a bot's dependencies; nil clients disable that channel.
type Options struct {
	Config    config.BotConfig
	Host      string
	Matrix    *matrix.Client
	MatrixCfg *config.MatrixConfig
	Telegram  *telegram.Client
	Factories map[string]Factory
	// DryRun evaluates checks and logs what would be sent; nothing is sent
	// or received and no room is touched.
	DryRun bool
	Log    *slog.Logger
}

// Bot is one running bot.
type Bot struct {
	cfg    config.BotConfig
	host   string
	log    *slog.Logger
	dryRun bool

	me    *matrix.Identity // nil without Matrix
	rooms *rooms           // nil without Matrix
	tg    *telegram.Client // nil without Telegram

	alerts   *alert.Engine
	checks   []Check
	commands []Command          // in help order
	byName   map[string]Command // names and aliases
	jobs     []Job

	purgeMu  sync.Mutex
	purgeErr error

	// tunables, shortened by tests
	retry       retrier
	errorPause  time.Duration // after a failed poll
	syncTimeout time.Duration
}

const fallbackNote = "（Matrix 未送达，改由 Telegram 发送）"

// New builds the bot and its modules.
func New(o Options) (*Bot, error) {
	c := o.Config
	log := o.Log.With("bot", c.ID)
	b := &Bot{
		cfg:         c,
		host:        o.Host,
		log:         log,
		dryRun:      o.DryRun,
		byName:      map[string]Command{},
		retry:       retrier{attempts: 3, pause: 5 * time.Second, log: log},
		errorPause:  10 * time.Second,
		syncTimeout: 30 * time.Second,
	}
	sustain := int(c.Alerts.Sustain.Duration / c.Alerts.Interval.Duration)
	b.alerts = alert.New(sustain, c.Alerts.Remind.Duration)

	if c.User != "" && o.Matrix != nil {
		userID := o.MatrixCfg.UserID(c.User)
		b.me = o.Matrix.As(userID, c.User != o.MatrixCfg.SenderLocalpart)
		b.rooms = &rooms{
			me: b.me, mx: o.MatrixCfg, cfg: c.Rooms, admin: c.Admin, botID: c.ID, host: o.Host,
			log: log, retry: b.retry, invite: time.Hour,
			ids: map[role]string{}, lastInvite: map[role]time.Time{},
		}
	}
	if c.Telegram != nil {
		b.tg = o.Telegram
	}

	for _, m := range c.Modules {
		f, ok := o.Factories[m.Name]
		if !ok {
			return nil, fmt.Errorf("bot %s: unknown module %q (have %v)", c.ID, m.Name,
				slices.Sorted(maps.Keys(o.Factories)))
		}
		mod, err := f(Env{Host: o.Host, BotID: c.ID, Log: log.With("module", m.Name), Alerts: b.alerts}, m.Config)
		if err != nil {
			return nil, fmt.Errorf("bot %s: module %s: %w", c.ID, m.Name, err)
		}
		b.checks = append(b.checks, mod.Checks()...)
		for _, cmd := range mod.Commands() {
			if err := b.addCommand(cmd); err != nil {
				return nil, fmt.Errorf("bot %s: module %s: %w", c.ID, m.Name, err)
			}
		}
		b.jobs = append(b.jobs, mod.Jobs()...)
	}
	for _, cmd := range b.builtinCommands() {
		if err := b.addCommand(cmd); err != nil {
			return nil, fmt.Errorf("bot %s: %w", c.ID, err)
		}
	}
	b.checks = append(b.checks, Check{Name: "self", Run: b.selfCheck})
	if b.rooms != nil && c.Rooms.Query.Retention.Duration > 0 {
		b.jobs = append(b.jobs, Job{
			Name: "purge-query-room", Every: c.Rooms.Query.PurgeEvery.Duration, Delay: time.Minute,
			Run: b.purge,
		})
	}
	return b, nil
}

func (b *Bot) addCommand(cmd Command) error {
	for _, n := range append([]string{cmd.Name}, cmd.Aliases...) {
		if other, ok := b.byName[n]; ok {
			return fmt.Errorf("command name %q of %s already used by %s", n, cmd.Name, other.Name)
		}
		b.byName[n] = cmd
	}
	b.commands = append(b.commands, cmd)
	return nil
}

// Run blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	// Virtual users must exist before they can act; the sender user always does.
	if b.rooms != nil && !b.dryRun && b.cfg.User != b.rooms.mx.SenderLocalpart {
		err := b.retry.do(ctx, "matrix register", func() error { return b.me.Register(ctx) })
		if err != nil {
			b.log.Error("matrix: cannot register the bot user; Matrix will keep failing", "err", err)
		}
	}
	var wg sync.WaitGroup
	if !b.dryRun {
		if b.tg != nil && b.cfg.Telegram.Commands {
			wg.Go(func() { b.pollTelegram(ctx) })
		}
		if b.rooms != nil {
			wg.Go(func() { b.pollMatrix(ctx) })
		}
	}
	for _, j := range b.jobs {
		if b.dryRun && j.Name == "purge-query-room" {
			continue
		}
		wg.Go(func() { b.runJob(ctx, j) })
	}
	b.notify(ctx, message.Message{
		Icon:  "ℹ️",
		After: fmt.Sprintf("[%s] %s 已启动（查询房间发 h 看指令，Telegram 发 /help）", b.host, b.cfg.ID),
	}, "")
	b.watch(ctx)
	wg.Wait()
	return nil
}

// watch evaluates the checks every interval until ctx ends.
func (b *Bot) watch(ctx context.Context) {
	t := time.NewTicker(b.cfg.Alerts.Interval.Duration)
	defer t.Stop()
	for {
		b.evaluate(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// evaluate runs all checks concurrently and reports the alert events.
func (b *Bot) evaluate(ctx context.Context) {
	var (
		mu  sync.Mutex
		bad = map[string]string{}
		wg  sync.WaitGroup
	)
	for _, c := range b.checks {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("check panicked", "check", c.Name, "panic", r)
				}
			}()
			found := c.Run(ctx)
			mu.Lock()
			defer mu.Unlock()
			for _, f := range found {
				bad[f.Key] = f.Msg
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // shutting down: results may be partial, don't "resolve" anything
	}
	for _, ev := range b.alerts.Update(bad) {
		b.notify(ctx, b.alertMessage(ev), ev.Key)
	}
}

func (b *Bot) alertMessage(ev alert.Event) message.Message {
	tag := "[" + b.host + "] "
	switch ev.Kind {
	case alert.Firing:
		return message.Message{Icon: "🔴", Title: tag + "告警", Lines: []string{ev.Msg}}
	case alert.Reminder:
		return message.Message{Icon: "🔴", Title: tag + "仍未恢复", After: "（已 " + human.Duration(ev.Age) + "）", Lines: []string{ev.Msg}}
	default:
		return message.Message{Icon: "🟢", Title: tag + "已恢复", After: "（持续 " + human.Duration(ev.Age) + "）", Lines: []string{ev.Msg}}
	}
}

// selfCheck raises problems of the bot itself.
func (b *Bot) selfCheck(context.Context) []Finding {
	b.purgeMu.Lock()
	defer b.purgeMu.Unlock()
	if b.purgeErr != nil {
		return []Finding{{"self:purge", "查询房间清理失败（历史会一直累积）：" + b.purgeErr.Error()}}
	}
	return nil
}

func (b *Bot) purge(ctx context.Context) error {
	err := b.rooms.purge(ctx)
	b.purgeMu.Lock()
	b.purgeErr = err
	b.purgeMu.Unlock()
	return err
}

// notify sends an alert or notice to the Matrix alerts room, and to Telegram
// as well when Matrix did not deliver it or the key is out-of-band (about
// the Matrix path itself).
func (b *Bot) notify(ctx context.Context, msg message.Message, key string) {
	if b.dryRun {
		b.log.Info("dry-run: would notify", "key", key, "text", msg.Plain())
		return
	}
	delivered := false
	if b.rooms != nil {
		delivered = b.rooms.send(ctx, alertsRoom, msg, "m.text")
	}
	if delivered && !hasAnyPrefix(key, b.cfg.OutOfBand) {
		return
	}
	if !delivered && b.rooms != nil {
		msg = msg.WithNote(fallbackNote)
	}
	if !b.sendTelegram(ctx, msg) && !delivered {
		b.log.Error("message lost: neither Matrix nor Telegram delivered it", "key", key, "text", msg.Plain())
	}
}

func (b *Bot) sendTelegram(ctx context.Context, msg message.Message) bool {
	if b.tg == nil {
		return false
	}
	err := b.retry.do(ctx, "telegram send", func() error {
		return b.tg.SendHTML(ctx, b.cfg.Telegram.ChatID, msg.TelegramHTML())
	})
	return err == nil
}

func (b *Bot) runJob(ctx context.Context, j Job) {
	next := j.Delay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
		if err := j.Run(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("job failed", "job", j.Name, "err", err)
		}
		next = j.Every
	}
}

// pollMatrix answers the admin's messages in the query room.
func (b *Bot) pollMatrix(ctx context.Context) {
	since := ""
	for ctx.Err() == nil {
		room, err := b.rooms.room(ctx, queryRoom)
		if err == nil {
			var res *matrix.SyncResponse
			res, err = b.me.Sync(ctx, since, syncFilter(room), b.syncTimeout)
			if err == nil {
				first := since == ""
				since = res.NextBatch
				if !first { // skip whatever was said before we started
					b.handleMatrix(ctx, res.Rooms.Join[room].Timeline.Events)
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("matrix sync failed", "err", err)
		sleep(ctx, b.errorPause)
	}
}

func (b *Bot) handleMatrix(ctx context.Context, events []matrix.Event) {
	for _, ev := range events {
		if ev.Sender != b.cfg.Admin {
			continue
		}
		var content struct {
			MsgType string `json:"msgtype"`
			Body    string `json:"body"`
		}
		if json.Unmarshal(ev.Content, &content) != nil || content.MsgType != "m.text" {
			continue
		}
		reply, ok := b.dispatch(ctx, Request{Channel: "matrix", Sender: ev.Sender}, content.Body, false)
		if !ok {
			continue
		}
		if !b.rooms.send(ctx, queryRoom, reply, "m.notice") {
			b.sendTelegram(ctx, reply.WithNote(fallbackNote))
		}
	}
}

// pollTelegram answers /commands from the configured chat. It is the only
// getUpdates consumer for this token: never set a webhook on it.
func (b *Bot) pollTelegram(ctx context.Context) {
	var cmds []telegram.Command
	for _, c := range b.commands {
		cmds = append(cmds, telegram.Command{Command: c.Name, Description: c.Help})
	}
	// The menu is shown only in the configured chat: anyone can open a bot,
	// and the default-scope menu would tell them what this one does.
	if err := b.tg.SetMyCommands(ctx, cmds, telegram.ChatScope(b.cfg.Telegram.ChatID)); err != nil {
		b.log.Warn("telegram setMyCommands failed", "err", err)
	}
	if err := b.tg.DeleteMyCommands(ctx, nil); err != nil {
		b.log.Warn("telegram deleteMyCommands (default scope) failed", "err", err)
	}
	started := time.Now()
	var offset int64
	for ctx.Err() == nil {
		ups, err := b.tg.GetUpdates(ctx, offset, 50*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Warn("telegram getUpdates failed", "err", err)
			sleep(ctx, b.errorPause)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			m := u.Message
			// ignore other chats and the backlog from before we started
			if m == nil || m.Chat.ID != b.cfg.Telegram.ChatID || m.Date < started.Unix()-60 {
				continue
			}
			req := Request{Channel: "telegram", Sender: strconv.FormatInt(m.Chat.ID, 10)}
			if reply, ok := b.dispatch(ctx, req, m.Text, true); ok {
				b.sendTelegram(ctx, reply)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
