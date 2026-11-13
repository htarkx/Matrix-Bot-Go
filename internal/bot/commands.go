package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// builtinCommands are present in every bot, after the modules' commands.
func (b *Bot) builtinCommands() []Command {
	var cmds []Command
	if len(b.summarizers) > 0 {
		cmds = append(cmds, Command{Name: "dashboard", Aliases: []string{"d"}, Help: "总览", Run: b.cmdDashboard})
	}
	return append(cmds,
		Command{Name: "alerts", Aliases: []string{"a"}, Help: "当前告警", Run: b.cmdAlerts},
		Command{Name: "help", Aliases: []string{"h", "?"}, Help: "指令列表", Run: b.cmdHelp},
	)
}

// namedSummarizer is a module's dashboard section.
type namedSummarizer struct {
	name string
	s    Summarizer
}

// summaryTimeout bounds each module's dashboard section.
const summaryTimeout = 8 * time.Second

// cmdDashboard asks every summarizing module at once and keeps module order.
func (b *Bot) cmdDashboard(ctx context.Context, _ Request) (message.Message, error) {
	parts := make([][]string, len(b.summarizers))
	var wg sync.WaitGroup
	for i, ns := range b.summarizers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
			defer cancel()
			lines, err := safeSummary(ns.s, ctx)
			if err != nil {
				b.log.Warn("dashboard section failed", "module", ns.name, "err", err)
				lines = []string{ns.name + "  读取失败：" + err.Error()}
			}
			parts[i] = lines
		})
	}
	wg.Wait()
	var lines []string
	for _, p := range parts {
		lines = append(lines, p...)
	}
	m := message.Message{Icon: "📊", Title: "[" + b.host + "] 总览", Pre: strings.Join(lines, "\n")}
	if n := len(b.alerts.Active()); n > 0 {
		m.After = fmt.Sprintf("，%d 条告警中（发 a 查看）", n)
	}
	return m, nil
}

func safeSummary(s Summarizer, ctx context.Context) (lines []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("内部错误（%v）", r)
		}
	}()
	return s.Summary(ctx)
}

// dispatch answers one chat line. Commands start with "!" or "/"; in the
// bot's own Matrix room (needPrefix false) the bare word works too: "s",
// "status". Telegram's "/status@SomeBot" form is accepted. Unknown commands
// get the help text, except in strict mode (a shared room), where they are
// left alone. ok is false when the line is not addressed to the bot.
func (b *Bot) dispatch(ctx context.Context, req Request, text string, needPrefix, strict bool) (reply message.Message, ok bool) {
	if req.Received.IsZero() {
		req.Received = time.Now()
	}
	text = strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, "!"), strings.HasPrefix(text, "/"):
		text = text[1:]
	case needPrefix:
		return message.Message{}, false
	}
	words := strings.Fields(text)
	name := ""
	if len(words) > 0 {
		name, _, _ = strings.Cut(strings.ToLower(words[0]), "@")
		req.Args = words[1:]
	}
	cmd, found := b.byName[name]
	if !found {
		if strict {
			return message.Message{}, false
		}
		cmd = b.byName["help"]
	}
	reply, err := b.safeRun(ctx, cmd, req)
	if err != nil {
		return message.Message{Lines: []string{"指令出错：" + err.Error()}}, true
	}
	return reply, true
}

func (b *Bot) safeRun(ctx context.Context, cmd Command, req Request) (reply message.Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("command panicked", "command", cmd.Name, "panic", r)
			err = fmt.Errorf("内部错误（%v）", r)
		}
	}()
	return cmd.Run(ctx, req)
}

func (b *Bot) cmdAlerts(context.Context, Request) (message.Message, error) {
	active := b.alerts.Active()
	if len(active) == 0 {
		return message.Message{Icon: "✅", After: "[" + b.host + "] 没有告警"}, nil
	}
	m := message.Message{Icon: "🔴", Title: "[" + b.host + "] 告警中"}
	for _, a := range active {
		m.Lines = append(m.Lines, fmt.Sprintf("• %s（%s）", a.Msg, human.Duration(time.Since(a.Since))))
	}
	return m, nil
}

func (b *Bot) cmdHelp(context.Context, Request) (message.Message, error) {
	m := message.Message{Title: fmt.Sprintf("可用指令（%s · %s）", b.host, b.cfg.ID)}
	for _, c := range b.commands {
		short := strings.Join(c.Aliases, "/")
		if short == "" {
			short = c.Name
		}
		m.Lines = append(m.Lines, fmt.Sprintf("%s — %s（%s）", short, c.Help, c.Name))
	}
	m.Note = "Matrix 里直接发字母（如 s），Telegram 里用 /status"
	return m, nil
}
