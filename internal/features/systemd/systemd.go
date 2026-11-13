// Package systemd alerts when units are not active and when periodic backup
// jobs (oneshot service instances) failed or went stale, and lists both with
// the services command.
package systemd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// Config is the module's JSON configuration.
type Config struct {
	// Units must be active.
	Units []string `json:"units"`
	// Backups maps a job name to the longest acceptable time since its last
	// run; the unit is BackupUnit with %s replaced by the job name.
	Backups    map[string]config.Duration `json:"backups"`
	BackupUnit string                     `json:"backup_unit"` // default "edge-backup@%s.service"
}

// Shower reads unit properties (systemctl show).
type Shower interface {
	Show(ctx context.Context, unit string, props ...string) (map[string]string, error)
}

// ErrUnreachable means systemd could not be asked at all. It must never be
// read as "the unit is down".
var ErrUnreachable = errors.New("systemd unreachable")

// Systemctl runs `systemctl show`. It talks D-Bus, so the service user must
// be one dbus-daemon can resolve (not a DynamicUser).
type Systemctl struct{}

// Show implements Shower.
func (Systemctl) Show(ctx context.Context, unit string, props ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", "show", unit, "--timestamp=unix", "-p", strings.Join(props, ","))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String() + stdout.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, msg)
	}
	out := map[string]string{}
	for line := range strings.Lines(stdout.String()) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// Module is the systemd module.
type Module struct {
	bot.NoJobs
	cfg  Config
	env  bot.Env
	sd   Shower
	jobs []string // backup job names, sorted
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	return newWith(env, raw, Systemctl{})
}

func newWith(env bot.Env, raw json.RawMessage, sd Shower) (*Module, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.BackupUnit == "" {
		c.BackupUnit = "edge-backup@%s.service"
	}
	if strings.Count(c.BackupUnit, "%s") != 1 {
		return nil, fmt.Errorf("backup_unit %q must contain exactly one %%s", c.BackupUnit)
	}
	if len(c.Units) == 0 && len(c.Backups) == 0 {
		return nil, errors.New("nothing to watch: set units and/or backups")
	}
	m := &Module{cfg: c, env: env, sd: sd}
	for j := range c.Backups {
		m.jobs = append(m.jobs, j)
	}
	slices.Sort(m.jobs)
	return m, nil
}

// Checks implements bot.Module.
func (m *Module) Checks() []bot.Check {
	return []bot.Check{{Name: "systemd", Run: m.check}}
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{{Name: "services", Aliases: []string{"sv"}, Help: "服务与备份状态", Run: m.cmdServices}}
}

func (m *Module) unit(job string) string { return fmt.Sprintf(m.cfg.BackupUnit, job) }

// lastRun is a backup job's last result and exit time (zero when never run).
func (m *Module) lastRun(ctx context.Context, job string) (result string, at time.Time, err error) {
	p, err := m.sd.Show(ctx, m.unit(job), "Result", "ExecMainExitTimestamp")
	if err != nil {
		return "", time.Time{}, err
	}
	if ts := strings.TrimPrefix(p["ExecMainExitTimestamp"], "@"); ts != "" {
		if sec, err := strconv.ParseInt(ts, 10, 64); err == nil {
			at = time.Unix(sec, 0)
		}
	}
	return p["Result"], at, nil
}

func (m *Module) check(ctx context.Context) []bot.Finding {
	var out []bot.Finding
	bad := func(key, format string, a ...any) {
		out = append(out, bot.Finding{Key: key, Msg: fmt.Sprintf(format, a...)})
	}
	unreachable := func(err error) []bot.Finding {
		// One alert about the monitor itself, not a false "down" for every unit.
		return []bot.Finding{{Key: "self:systemd", Msg: m.env.BotID + " 读不到 systemd 状态，服务和备份检查暂停：" + err.Error()}}
	}
	for _, u := range m.cfg.Units {
		p, err := m.sd.Show(ctx, u, "ActiveState")
		if err != nil {
			return unreachable(err)
		}
		if st := p["ActiveState"]; st != "active" {
			bad("svc:"+u, "服务 %s 状态 %s", u, orQ(st))
		}
	}
	for _, job := range m.jobs {
		result, at, err := m.lastRun(ctx, job)
		if err != nil {
			return unreachable(err)
		}
		switch age := time.Since(at); {
		case result != "success":
			bad("bak:"+job, "备份 %s 上次运行失败（%s）", job, orQ(result))
		case at.IsZero():
			bad("bak:"+job, "备份 %s 还没有成功运行过", job)
		case age > m.cfg.Backups[job].Duration:
			bad("bak:"+job, "备份 %s 已 %s 没有运行", job, human.Duration(age))
		}
	}
	return out
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func (m *Module) cmdServices(ctx context.Context, _ bot.Request) (message.Message, error) {
	var lines []string
	nameW := 12
	for _, u := range m.cfg.Units {
		nameW = max(nameW, len(u))
	}
	for _, u := range m.cfg.Units {
		p, err := m.sd.Show(ctx, u, "ActiveState", "MemoryCurrent")
		if err != nil {
			return message.Message{}, err
		}
		mem := ""
		if v, err := strconv.ParseUint(p["MemoryCurrent"], 10, 64); err == nil && v != 1<<64-1 {
			mem = human.Bytes(v)
		}
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%-*s %-8s %s", nameW, u, orQ(p["ActiveState"]), mem), " "))
	}
	for _, job := range m.jobs {
		result, at, err := m.lastRun(ctx, job)
		if err != nil {
			return message.Message{}, err
		}
		when := "从未运行"
		if !at.IsZero() {
			when = human.Duration(time.Since(at)) + "前"
		}
		lines = append(lines, fmt.Sprintf("备份 %-8s %-8s %s", job, orQ(result), when))
	}
	return message.Message{Icon: "🧩", Title: "[" + m.env.Host + "] 服务", Pre: strings.Join(lines, "\n")}, nil
}

// Summary implements bot.Summarizer: units up, and each backup's last run.
func (m *Module) Summary(ctx context.Context) ([]string, error) {
	var lines []string
	if len(m.cfg.Units) > 0 {
		var down []string
		for _, u := range m.cfg.Units {
			p, err := m.sd.Show(ctx, u, "ActiveState")
			if err != nil {
				return nil, err
			}
			if st := p["ActiveState"]; st != "active" {
				down = append(down, u+" "+orQ(st))
			}
		}
		l := fmt.Sprintf("服务  %d/%d 正常", len(m.cfg.Units)-len(down), len(m.cfg.Units))
		if len(down) > 0 {
			l += "（" + strings.Join(down, "，") + "）"
		}
		lines = append(lines, l)
	}
	if len(m.jobs) > 0 {
		var parts []string
		for _, job := range m.jobs {
			result, at, err := m.lastRun(ctx, job)
			if err != nil {
				return nil, err
			}
			switch {
			case at.IsZero():
				parts = append(parts, job+" 从未运行")
			case result != "success":
				parts = append(parts, job+" 失败（"+orQ(result)+"）")
			default:
				parts = append(parts, job+" "+human.Duration(time.Since(at))+"前")
			}
		}
		lines = append(lines, "备份  "+strings.Join(parts, " · "))
	}
	return lines, nil
}
