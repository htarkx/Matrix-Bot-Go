// Package config loads and validates the bot configuration file.
//
// One process runs one or more bots. Every bot can speak on Matrix (as the
// appservice's own user or as one of its virtual users) and/or Telegram, and
// runs a list of modules that contribute alert checks, commands and jobs.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Config is the whole configuration file.
type Config struct {
	// Host is the label shown in every message, e.g. "[edge-1]".
	// Empty means the machine's hostname.
	Host   string        `json:"host"`
	Matrix *MatrixConfig `json:"matrix"`
	Bots   []BotConfig   `json:"bots"`
}

// MatrixConfig describes the appservice registration all Matrix bots share.
type MatrixConfig struct {
	// Homeserver is the client-server API base URL, e.g. http://127.0.0.1:8008.
	Homeserver string `json:"homeserver"`
	// ServerName is the part after the colon in user IDs and aliases.
	ServerName string `json:"server_name"`
	// SenderLocalpart is the appservice's own user (sender_localpart in the
	// registration). Bots with another localpart act as virtual users of the
	// appservice and must be inside its user namespace.
	SenderLocalpart string `json:"sender_localpart"`
	// TokenCredential names the file holding the appservice as_token.
	TokenCredential string `json:"token_credential"`
}

// BotConfig is one bot identity.
type BotConfig struct {
	// ID names the bot in logs and messages.
	ID string `json:"id"`
	// User is the bot's Matrix localpart. Empty: the bot does not use Matrix.
	User string `json:"user"`
	// Admin is the only Matrix user whose commands count. The bot invites
	// them to its rooms and treats a message as delivered only once they joined.
	Admin    string          `json:"admin"`
	Rooms    Rooms           `json:"rooms"`
	Telegram *TelegramConfig `json:"telegram"`
	Alerts   AlertConfig     `json:"alerts"`
	// OutOfBand lists alert key prefixes that always go to Telegram as well,
	// because they are about the Matrix delivery path itself.
	OutOfBand []string `json:"out_of_band"`
	// Modules maps a module name to its own configuration. The order in the
	// file is kept: it is the order of checks and of the help text.
	Modules Modules `json:"modules"`
}

// Module is one entry of BotConfig.Modules.
type Module struct {
	Name   string
	Config json.RawMessage
}

// Modules is a JSON object decoded with its key order intact.
type Modules []Module

// UnmarshalJSON walks the object token by token (a map would lose the order).
func (m *Modules) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("modules must be an object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name := tok.(string) // object keys are always strings
		if seen[name] {
			return fmt.Errorf("module %q listed twice", name)
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("module %q: %w", name, err)
		}
		*m = append(*m, Module{Name: name, Config: raw})
	}
	_, err := dec.Token() // closing '}'
	return err
}

// Rooms are found by alias and created when missing.
type Rooms struct {
	// Alerts receives alerts and recoveries. Kept forever, read-only for
	// everyone but the bot, sent as m.text so they push.
	Alerts RoomConfig `json:"alerts"`
	// Query is where the admin types commands. Replies are m.notice.
	Query RoomConfig `json:"query"`
}

// RoomConfig describes one room.
type RoomConfig struct {
	// Alias localpart, e.g. "watch" for #watch:server.
	Alias string `json:"alias"`
	Name  string `json:"name"`
	Topic string `json:"topic"`
	// Retention > 0 purges history older than this (needs a server admin bot).
	Retention Duration `json:"retention"`
	// PurgeEvery is how often the purge runs (default 1h).
	PurgeEvery Duration `json:"purge_every"`
}

// TelegramConfig is the out-of-band channel.
type TelegramConfig struct {
	TokenCredential string `json:"token_credential"`
	ChatID          int64  `json:"chat_id"`
	// Commands makes this process answer /commands. Only one process may do
	// that per token: Telegram allows a single getUpdates consumer.
	Commands bool `json:"commands"`
}

// AlertConfig tunes the alert engine.
type AlertConfig struct {
	// Interval between evaluations (default 30s).
	Interval Duration `json:"interval"`
	// Sustain: a condition must hold this long before it alerts (default 60s).
	Sustain Duration `json:"sustain"`
	// Remind repeats a firing alert this often (default 6h).
	Remind Duration `json:"remind"`
}

// Duration is a time.Duration written as "90s", "10m", "6h" in JSON.
type Duration struct{ time.Duration }

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %q is negative", s)
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes the duration back as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Load reads, decodes (rejecting unknown fields), defaults and validates a file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse is Load without the file.
func Parse(raw []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Host == "" {
		c.Host, _ = os.Hostname()
	}
	if c.Matrix != nil && c.Matrix.TokenCredential == "" {
		c.Matrix.TokenCredential = "matrix_token"
	}
	for i := range c.Bots {
		b := &c.Bots[i]
		def := func(d *Duration, v time.Duration) {
			if d.Duration == 0 {
				d.Duration = v
			}
		}
		def(&b.Alerts.Interval, 30*time.Second)
		def(&b.Alerts.Sustain, 60*time.Second)
		def(&b.Alerts.Remind, 6*time.Hour)
		def(&b.Rooms.Query.PurgeEvery, time.Hour)
		if b.Telegram != nil && b.Telegram.TokenCredential == "" {
			b.Telegram.TokenCredential = "telegram_token"
		}
	}
}

var (
	localpartRe = regexp.MustCompile(`^[a-z0-9._=/+-]+$`)
	userIDRe    = regexp.MustCompile(`^@[a-z0-9._=/+-]+:[A-Za-z0-9.:\[\]-]+$`)
)

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Bots) == 0 {
		add("no bots configured")
	}
	if m := c.Matrix; m != nil {
		if m.Homeserver == "" {
			add("matrix.homeserver is required")
		}
		if m.ServerName == "" {
			add("matrix.server_name is required")
		}
		if !localpartRe.MatchString(m.SenderLocalpart) {
			add("matrix.sender_localpart %q is not a valid localpart", m.SenderLocalpart)
		}
	}

	ids := map[string]bool{}
	users := map[string]bool{}
	pollers := map[string]string{}
	for i, b := range c.Bots {
		where := fmt.Sprintf("bots[%d] (%s)", i, b.ID)
		if b.ID == "" {
			add("bots[%d]: id is required", i)
		} else if ids[b.ID] {
			add("%s: duplicate id", where)
		}
		ids[b.ID] = true

		if b.User == "" && b.Telegram == nil {
			add("%s: needs matrix (user) or telegram", where)
		}
		if b.User != "" {
			if c.Matrix == nil {
				add("%s: user is set but there is no matrix section", where)
			}
			if !localpartRe.MatchString(b.User) {
				add("%s: user %q is not a valid localpart", where, b.User)
			} else if users[b.User] {
				add("%s: user %q is used by another bot", where, b.User)
			}
			users[b.User] = true
			if !userIDRe.MatchString(b.Admin) {
				add("%s: admin %q is not a user ID", where, b.Admin)
			}
			if b.Rooms.Alerts.Alias == "" || b.Rooms.Query.Alias == "" {
				add("%s: rooms.alerts.alias and rooms.query.alias are required", where)
			}
			if b.Rooms.Alerts.Alias == b.Rooms.Query.Alias {
				add("%s: alerts and query rooms must differ", where)
			}
		}
		if t := b.Telegram; t != nil {
			if t.ChatID == 0 {
				add("%s: telegram.chat_id is required", where)
			}
			if t.Commands {
				if other, ok := pollers[t.TokenCredential]; ok {
					add("%s: telegram token %q already answers commands for bot %s (one getUpdates consumer per token)",
						where, t.TokenCredential, other)
				}
				pollers[t.TokenCredential] = b.ID
			}
		}
		if b.Alerts.Sustain.Duration < b.Alerts.Interval.Duration {
			add("%s: alerts.sustain is shorter than alerts.interval", where)
		}
	}
	return errors.Join(errs...)
}

// UserID builds @localpart:server_name.
func (m *MatrixConfig) UserID(localpart string) string {
	return "@" + localpart + ":" + m.ServerName
}

// Alias builds #localpart:server_name.
func (m *MatrixConfig) Alias(localpart string) string {
	return "#" + localpart + ":" + m.ServerName
}

// ReadCredential reads a secret file from dir (systemd's $CREDENTIALS_DIRECTORY).
func ReadCredential(dir, name string) (string, error) {
	if dir == "" {
		return "", errors.New("no credentials directory (set $CREDENTIALS_DIRECTORY or -credentials)")
	}
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("bad credential name %q", name)
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("credential %q is empty", name)
	}
	return s, nil
}
