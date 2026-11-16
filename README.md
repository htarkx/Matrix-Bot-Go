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
| `c` | livekit | calls in progress: Matrix room, who, which media, recent audio loss/jitter/RTT |
| `tls` | certs | certificate expiry |
| `p` `up` | probe | other machines from the outside (HTTP status, TCP port); availability over 24 h / 7 d / 30 d, monthly downtime budget, incidents; weekly and monthly reports |
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
internal/features/livekit     MatrixRTC calls on a LiveKit SFU; line quality from its Prometheus endpoint
internal/features/certs       TLS certificate expiry
internal/features/probe       outside probes of other hosts, their availability history and reports
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

**Call quality.** With `metrics_url` (LiveKit's `prometheus.port`, keep it
on loopback) the livekit module scrapes every `metrics_every` (15s) and shows
audio loss, jitter and RTT over the last `metrics_window` (1m), uplink
(participants → SFU) and downlink (SFU → participants). LiveKit aggregates
these over all participants, so they are not per person. Mean audio loss at
or above `loss_alert_pct` (default 5, 0 = off) in either direction raises
`call:loss`; nothing is measured, so nothing alerts, when nobody is calling.

**Watching each other.** A bot cannot report its own host going down. Run a
second bot on another machine with the `probe` module pointed at the first
host's services, and let the first host probe the second. The second bot may
use Matrix too (its own, non-admin account and rooms), but its probe alerts
should also go out of band (`out_of_band: ["probe:", "self:"]`), since the
homeserver may be what is down; with `telegram.commands: false` the main bot
keeps `getUpdates`. Probes run on every alert evaluation; `sustain` absorbs a
single lost probe. HTTP probes do not follow redirects and verify TLS. Each
target names the `host` it runs on: that machine is the subject of its alerts.

**Latency: round trip vs. request.** Two figures, because they differ by
design. *Round trip* (往返) is the TCP connect alone: one RTT, about what
`ping` shows, and the same in both directions. *Request* (请求) is a whole
HTTPS probe on a fresh connection (keep-alives are off): TCP connect, TLS
handshake, the HTTP exchange, so about three round trips, plus the server's
own time. A TCP target only has the round trip. So with 88 ms between two
machines, A probing B's OpenVPN port shows 88 ms while B probing A's
websites shows a round trip of 88 ms and requests of about 300 ms.

**Availability ("how many nines").** The probe module records every check
round in hourly buckets (35 days) plus incidents (a year) in
`$STATE_DIRECTORY/probe.json` (`-state`, systemd `StateDirectory=`), saved
every 5 minutes and on shutdown. Availability is good rounds / recorded
rounds against `slo_pct` (default 99.9); the downtime budget is what the SLO
allows in the calendar month. Tables keep everything before the last column
ASCII (names come last), so they line up whatever a chat client's font
does with CJK. `controls` are outside services (a public
resolver, a 204 endpoint): when all of them fail too, this machine is
offline, so the round is neither recorded nor alerted (alerts are held, not
resolved). With `weekly_report` / `monthly_report` a report is posted
after each Monday-to-Monday week / calendar month at `report_hour` in
`timezone`; one missed while the bot was down is sent when it is back.

**Module API.** Besides checks, commands and jobs, a module gets from
`bot.Env` a `StateDir`, the check `Interval` and `Post` (an informational
message: alerts room without a push, Telegram as fallback); it may implement
`bot.Summarizer` (dashboard) and `bot.Stopper` (called on shutdown). Jobs
that write state or send are marked `SideEffects`: `-dry-run` and `-exec`
skip them.

**Alert wording.** Messages lead with the machine that has the problem, then
the thing and its state: `🔴 edge-1：服务 nginx 停了（inactive）`, and on
recovery `🟢 edge-1：服务 nginx 恢复正常` with how long it lasted. A problem
seen on another machine (probes) adds who noticed it. Modules fill
`bot.Finding` with `What` (the thing), `Msg` (what is wrong) and, for other
machines, `Subject`.

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
