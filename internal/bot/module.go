package bot

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/alert"
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
}

// AlertView is the read side of the bot's alert engine.
type AlertView interface {
	Active() []alert.Active
}

// Finding is a condition that is bad right now. Key identifies it across
// evaluations; a prefix such as "svc:" lets out_of_band route it.
type Finding struct {
	Key string
	Msg string
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
