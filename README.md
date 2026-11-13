# Matrix-Bot-Go

Small Go bots for a self-hosted Matrix server, built on the standard library
only. Today it ships one job: **watch the host it runs on, and the Matrix
server on it**, and talk about both.

- Alerts (CPU, swap activity, memory, disk, systemd units, backup jobs, TLS
  certificates) go to a read-only Matrix room; recoveries and reminders follow.
- Commands in a private Matrix room, in extra rooms such as the homeserver's
  admin room, or on Telegram (`/status`, …). `d` is a one-message dashboard.
- Telegram is the out-of-band channel: it carries alerts about the Matrix path
  itself and anything Matrix could not deliver. A Matrix message only counts as
  delivered once the admin has joined the room.

| Command | Module | |
|---|---|---|
| `d` | (built in) | dashboard: one section per module that has one |
| `s` `t` `n` | hostwatch | status, top processes, traffic |
| `sv` | systemd | units and backup jobs |
| `u` `r` | homeserver | users (last seen, uploads), rooms |
| `b` | b2 | Backblaze B2 usage per bucket (all versions, as billed) |
| `c` | livekit | calls in progress: Matrix room, who, which media |
| `tls` | certs | certificate expiry |
| `a` `h` | (built in) | active alerts, help |

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
internal/synadmin   Synapse admin API reader (users, rooms, media stats)
internal/features/hostwatch   /proc, vnstat; status / top / net
internal/features/systemd     units and backup jobs; services
internal/features/homeserver  accounts, rooms, uploads (needs a server-admin bot)
internal/features/b2          B2 bucket usage, listed on a schedule
internal/features/livekit     MatrixRTC calls on a LiveKit SFU
internal/features/certs       TLS certificate expiry
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
listing it in `cmd/matrix-bot/main.go`. A module that also implements
`bot.Summarizer` gets a section in the `d` dashboard; summaries answer from
cached readings where a reading costs money or time (B2 listings are billed,
certificates are checked every few hours).

**Command rooms.** `rooms.command_rooms` lists rooms (by alias) where the
admin's commands are answered too, typically the homeserver's admin room,
which a server-admin bot is already in. There the bot is strict: it answers
only its own commands and ignores everything else, so `!admin …` lines stay
the homeserver's.

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
| `rooms.command_rooms` | extra room aliases where commands are answered (bot must be joined) |
| `modules` | module name → its config, in execution/help order |

Secrets are files in `$CREDENTIALS_DIRECTORY` (systemd `LoadCredential=`):
`matrix_token` (the appservice `as_token`) and `telegram_token` by default;
`b2_key_id` + `b2_key` (a key with listBuckets and listFiles; every bucket it
can see is reported) and `livekit_keys` (LiveKit's `key: secret` file) for
those modules.

## Running

```sh
matrix-bot -config config.json -check-config   # validate
matrix-bot -config config.json -dry-run        # evaluate, log, send nothing
matrix-bot -config config.json -exec d         # run jobs once, print one command's answer
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
