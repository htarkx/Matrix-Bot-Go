// Command matrix-bot runs the bots described in its configuration file.
//
//	matrix-bot -config /etc/matrix-bot/config.json
//
// Secrets (the appservice as_token, Telegram bot tokens) are read from
// $CREDENTIALS_DIRECTORY, which systemd's LoadCredential= provides.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/b2"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/certs"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/homeserver"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/hostwatch"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/livekit"
	"github.com/htarkx/Matrix-Bot-Go/internal/features/systemd"
	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
	"github.com/htarkx/Matrix-Bot-Go/internal/telegram"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// factories are the modules a config may name.
var factories = map[string]bot.Factory{
	"hostwatch":  hostwatch.New,
	"systemd":    systemd.New,
	"homeserver": homeserver.New,
	"b2":         b2.New,
	"livekit":    livekit.New,
	"certs":      certs.New,
}

func main() {
	var (
		configPath  = flag.String("config", envOr("MATRIX_BOT_CONFIG", "/etc/matrix-bot/config.json"), "configuration file")
		credentials = flag.String("credentials", os.Getenv("CREDENTIALS_DIRECTORY"), "directory holding the secret files")
		dryRun      = flag.Bool("dry-run", false, "evaluate and log what would be sent; send and receive nothing")
		checkOnly   = flag.Bool("check-config", false, "validate the configuration and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		execCmd     = flag.String("exec", "", "run one command (e.g. \"d\") for the first bot, print the answer and exit; sends nothing")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := newLogger()
	if err := run(log, *configPath, *credentials, *dryRun, *checkOnly, *execCmd); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath, credDir string, dryRun, checkOnly bool, execCmd string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// Secrets are not needed to evaluate checks, so a dry run works without them.
	secret := func(name string) (string, error) {
		if dryRun || checkOnly {
			return "", nil
		}
		return config.ReadCredential(credDir, name)
	}

	var mx *matrix.Client
	if cfg.Matrix != nil {
		tok, err := secret(cfg.Matrix.TokenCredential)
		if err != nil {
			return fmt.Errorf("matrix token: %w", err)
		}
		mx = matrix.New(cfg.Matrix.Homeserver, tok)
	}
	tgClients := map[string]*telegram.Client{} // one per token file
	var bots []*bot.Bot
	for _, bc := range cfg.Bots {
		var tg *telegram.Client
		if bc.Telegram != nil {
			name := bc.Telegram.TokenCredential
			if tg = tgClients[name]; tg == nil {
				tok, err := secret(name)
				if err != nil {
					return fmt.Errorf("bot %s: telegram token: %w", bc.ID, err)
				}
				tg = telegram.New(telegram.DefaultAPI, tok)
				tgClients[name] = tg
			}
		}
		b, err := bot.New(bot.Options{
			Config: bc, Host: cfg.Host, Matrix: mx, MatrixCfg: cfg.Matrix, Telegram: tg,
			Factories: factories, DryRun: dryRun, Log: log, Secret: secret,
		})
		if err != nil {
			return err
		}
		bots = append(bots, b)
	}
	if checkOnly {
		fmt.Printf("config ok: %d bot(s)\n", len(bots))
		return nil
	}
	if execCmd != "" {
		out, err := bots[0].Exec(context.Background(), execCmd)
		fmt.Println(out)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version, "host", cfg.Host, "bots", len(bots), "dry_run", dryRun)
	var wg sync.WaitGroup
	for _, b := range bots {
		wg.Go(func() { b.Run(ctx) })
	}
	wg.Wait()
	log.Info("stopped")
	return nil
}

// newLogger logs text to stderr, without timestamps under journald (it adds
// its own).
func newLogger() *slog.Logger {
	opts := &slog.HandlerOptions{}
	if os.Getenv("JOURNAL_STREAM") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
