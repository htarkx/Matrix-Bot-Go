# Matrix-Bot-Go

Small Go bots for a self-hosted Matrix server, built on the standard library
only. Today it ships one job: **watch the host it runs on** and talk about it.

- Alerts (CPU, swap activity, memory, disk, systemd units, backup jobs) go to a
  read-only Matrix room; recoveries and reminders follow.
- Commands in a private Matrix room (`s`, `t`, `n`, `sv`, `a`, `h`) or on
  Telegram (`/status`, …).
- Telegram is the out-of-band channel: it carries alerts about the Matrix path
  itself and anything Matrix could not deliver. A Matrix message only counts as
  delivered once the admin has joined the room.

User-facing messages are in Chinese.

## Design

```
cmd/matrix-bot      wiring, signals, -dry-run, -check-config
internal/config     JSON config; module order kept; every error reported at once
internal/message    channel-neutral messages, rendered for Matrix and Telegram
internal/matrix     client-server API as an appservice (own user + virtual users)
internal/telegram   sendMessage / getUpdates / setMyCommands
internal/alert      sustain → firing → reminder → resolved
internal/bot        runtime: modules, command routing, delivery policy, rooms
internal/features/hostwatch   /proc, vnstat; status / top / net
internal/features/systemd     units and backup jobs; services
```

Everything is under `internal/`: none of it is a public Go API. Features are
called *modules* in the config and in `bot.Module`; they live in
`internal/features/` to keep them apart from Go modules (`go.mod`).

**Bots and modules.** One process runs one or more bots. A bot is a Matrix
identity with its own two rooms (alerts, query), an admin, optional Telegram,
and a list of modules. A module contributes:

```go
type Module interface {
	Checks() []Check     // run every alert interval, return what is bad now
	Commands() []Command // chat commands with aliases and help text
	Jobs() []Job         // periodic work
}
```

Add a module by writing a `bot.Factory` under `internal/features/` and
listing it in `cmd/matrix-bot/main.go`.

**Matrix as an appservice.** All bots share one appservice token. The bot
whose `user` equals `sender_localpart` acts as the appservice user; any other
bot is a virtual user (`?user_id=`), registered on start, so the
registration's user namespace must cover it. Bots read their query room with
`/sync`. Rooms are found by alias or created (unencrypted, invite-only);
the alerts room's power levels are re-applied if they drift. Purging the query
room uses the Synapse admin API (`purge_history`, also served by Tuwunel), so
that bot must be a server admin.

## Configuration

See [`deploy/config.example.json`](deploy/config.example.json). Durations are
strings (`"30s"`, `"10m"`, `"6h"`). Unknown fields are errors.

| Bot field | Meaning |
|---|---|
| `user`, `admin` | Matrix localpart of the bot; the only user whose commands count |
| `rooms.alerts` / `rooms.query` | alias, name, topic; `retention` purges the query room |
| `telegram` | `chat_id`; `commands: true` makes this process the token's only `getUpdates` consumer |
| `alerts` | `interval` (30s), `sustain` (60s), `remind` (6h) |
| `out_of_band` | alert key prefixes that also go to Telegram |
| `modules` | module name → its config, in execution/help order |

Secrets are files in `$CREDENTIALS_DIRECTORY` (systemd `LoadCredential=`):
`matrix_token` (the appservice `as_token`) and `telegram_token` by default.

## Running

```sh
matrix-bot -config config.json -check-config   # validate
matrix-bot -config config.json -dry-run        # evaluate, log, send nothing
matrix-bot -config config.json
```

[`deploy/matrix-bot.service`](deploy/matrix-bot.service) is a hardened unit.
It must use a static system user: `systemctl show` goes over D-Bus, and
Ubuntu's dbus-daemon rejects `DynamicUser=` UIDs. Release binaries
(linux/amd64, linux/arm64, `SHA256SUMS`) are built for `v*` tags.

## Development

```sh
go test -race ./...
```

Tests run against fixtures (`internal/features/hostwatch/testdata/proc`), fake Matrix
and Telegram servers (`httptest`), and a fake clock (`testing/synctest`).

## License

MIT
