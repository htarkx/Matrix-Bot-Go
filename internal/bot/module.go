package bot

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/alert"
	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// Module is a feature: alert checks, commands and periodic jobs.
// Embed NoChecks / NoCommands / NoJobs for the parts a module lacks.
type Module interface {
	Checks() []Check
	Commands() []Command
	Jobs() []Job
}

// Factory builds a module from its JSON configuration (may be "null").
type Factory func(env Env, config json.RawMessage) (Module, error)

// Env is what a module gets from its bot.
type Env struct {
	Host   string // label for message titles, e.g. "edge-1"
	BotID  string
	Log    *slog.Logger
	Alerts AlertView
	// Matrix is the bot's own Matrix identity; nil when the bot has no Matrix.
	Matrix *matrix.Identity
	// Secret reads a credential file by name (systemd LoadCredential). It
	// returns "" without error in dry-run and check-config mode.
	Secret func(name string) (string, error)
}

// Summarizer is optionally implemented by a module that has something to
// say on the dashboard command: a few short lines, each starting with a
// label ("聊天  …"). Summary should answer from cached data where it can;
// the dashboard gives every module a few seconds.
type Summarizer interface {
	Summary(ctx context.Context) ([]string, error)
}

// AlertView is the read side of the bot's alert engine.
type AlertView interface {
	Active() []alert.Active
}

// Finding is a condition that is bad right now. Key identifies it across
// evaluations; a prefix such as "svc:" lets out_of_band route it.
//
// Messages read "<Subject>：<What> <Msg>" while it fires and "<Subject>：
// <What> 恢复正常" once it is gone, so What names the thing ("服务 nginx",
// "磁盘 /") and Msg says what is wrong with it ("停了（inactive）").
type Finding struct {
	Key string
	Msg string
	// What is the affected thing; empty when Msg says it all.
	What string
	// Subject is the machine the problem is on; empty means this host. Set
	// it when a module watches another machine (probes): the message then
	// names that machine and says this host noticed it.
	Subject string
}

// Check evaluates conditions; it runs every alert interval.
type Check struct {
	Name string
	Run  func(ctx context.Context) []Finding
}

// Command answers a chat command.
type Command struct {
	Name    string   // full name: "status", also /status on Telegram
	Aliases []string // short forms for the Matrix room: "s"
	Help    string
	Run     func(ctx context.Context, req Request) (message.Message, error)
}

// Request is one command invocation.
type Request struct {
	Channel string   // "matrix" or "telegram"
	Sender  string   // Matrix user ID or Telegram chat ID
	Args    []string // words after the command name
	// Received is when the bot got the command (its /sync or getUpdates
	// call returned), before parsing it. Commands can report their own
	// processing time against it.
	Received time.Time
	// Sent is when the homeserver received the command (Matrix
	// origin_server_ts, millisecond precision); zero on Telegram. The bot
	// runs next to the homeserver, so Received-Sent is the sync delivery
	// latency on one clock.
	Sent time.Time
}

// Job runs periodically.
type Job struct {
	Name  string
	Every time.Duration
	Delay time.Duration // before the first run
	Run   func(ctx context.Context) error
}

// NoChecks, NoCommands and NoJobs are embeddable empty implementations.
type (
	NoChecks   struct{}
	NoCommands struct{}
	NoJobs     struct{}
)

func (NoChecks) Checks() []Check       { return nil }
func (NoCommands) Commands() []Command { return nil }
func (NoJobs) Jobs() []Job             { return nil }
