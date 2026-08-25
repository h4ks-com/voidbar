# Voidbar

Discord-compatible IRCv3 bouncer (like [Spacebar](https://www.spacebar.chat/), but for IRC).

Voidbar speaks the Discord protocol (REST + Gateway) on one side and connects to
IRC networks on the other, so a real Discord client can be used to chat on IRC.

**Backend only.** Voidbar serves no web client and ships no Discord assets —
bring your own client (see [Android client](#android-client)). This keeps the
project on the same safe side of the C&D line Spacebar lives on: the server is
a clean-room implementation, the client is the user's own repackaged build.

## Concept

- **A guild is an IRC connection string.** There are no preconfigured guilds:
  pasting a connection string as an invite creates (or joins) a network, and each
  member gets their own upstream IRC connection with their own nick.
- **Multi-user.** Each user registers, joins networks by connection string, and
  holds independent connections to the same IRC server.
- **Bouncer semantics.** Upstream connections persist independently of client
  sessions; message history is buffered and replayed on reconnect.
- **Networks are "owned" by [Clyde](https://discord.com/wiki/clyde)** — the
  service-bot owner suppresses owner-only UI (notably "Delete server"), so the
  guild menu always offers "Leave server", which is what a bouncer user does.

## Status

Work in progress. The vertical slice is **working end-to-end against a live IRC
network** with the Discord **Android** client, 126.21, repackaged with
`tools/discord-apk-patcher`: login → READY → age gate suppressed → channel
history renders → send/receive relay in both directions, no crashes.

Covered:

- REST v9 + Gateway (HELLO/IDENTIFY/READY/HEARTBEAT/RESUME, zlib-stream,
  session replay) enough for the client to boot and render
- Register/login (argon2id, raw bearer tokens), `user`/`invite` CLI
- Connection strings as invites: paste `irc://host:port/#chan?name=X` into
  the client's "Join a server" field → preview card, join, GUILD_CREATE,
  the guild appears in the rail, the client navigates into the pasted channel
- Channel registry: snowflake channel ids (IRC names never hit URLs)
- **Leave/delete server**: membership removal + upstream disconnect, and the
  network (channels, replay buffers) is garbage-collected when the last
  member leaves
- **Discord → IRC**: typing in the client reaches the IRC channel (own nick,
  collision-suffixed, is shown as the author)
- **IRC → Discord**: channel PRIVMSGs are relayed live as MESSAGE_CREATE and
  render in the client; nick collisions don't eat foreign messages
- **Replay buffer**: the last 500 messages per channel are persisted
  (Badger) and served over `GET /channels/:id/messages` with Discord
  pagination (`limit`/`before`/`after`); own sends are buffered too, and
  history survives server restarts

## Android client

`tools/discord-apk-patcher` repackage of the stock Discord Android build
(decode → repoint hosts → rebuild → sign). The server carries a few
Android-specific compatibility details worth knowing about:

- **Gateway frame field order matters**: dispatch frames serialize `op`
  first, then `t`/`s` before `d` — the client's streaming JSON parser
  (IncomingParser) reads the header before the body. Go emits struct
  fields in declaration order, so `internal/discord/gateway/types.go`
  pins the order.
- **`nsfw_allowed: true`** on the user object doubles as "this account
  has a date of birth" for the client (MeUser maps it through
  NsfwAllowance). Without it, every account with a 2021+ snowflake hits
  the un-dismissable REGISTER_AGE_GATE modal after login.
- **IRC authors get deterministic snowflake ids**: the client parses
  message author ids as 64-bit integers; a literal `"irc:<nick>"` crashes
  its deserializer and takes down message rendering and the gateway
  dispatch handler.
- **Sends are right-trimmed**: the Android compose box appends a trailing
  newline to every message; real Discord trims it server-side, so the
  bouncer does too.
- Post-login probes are stubbed so they don't 404-loop:
  `POST /auth/fingerprint`, `GET /users/{id}/profile`,
  `GET /users/@me/survey`, `POST /users/@me/devices`,
  `GET /guilds/{id}/preview` ("Delete server" in settings is
  `POST /guilds/{id}/delete` — also routed).
  (`GET /sticker-packs` still 404s — harmless.)

`VOIDBAR_READY_MINIMAL=1` on `serve` shrinks the READY payload to the
minimum known-good set — a bisect switch for future client-compat work.

## Running a WIP instance

Three interchangeable ways — pick one. In all cases: build, run `serve`,
then point your client at the instance URL and register (or
`voidbar user add`). Press **Добавить сервер → Присоединиться к серверу**
in the client and paste a connection string, e.g.
`irc://irc.libera.chat:6697/#voidbar?name=Libera`.

### Windows (Go)

Prerequisites: Go 1.22+ (e.g. `C:\Program Files\Go\bin\go.exe`).

```powershell
& "C:\Program Files\Go\bin\go.exe" build -o voidbar.exe ./cmd/voidbar

$env:VOIDBAR_SERVER_LISTEN    = "0.0.0.0:18084"
$env:VOIDBAR_SERVER_PUBLIC_URL= "http://192.168.1.20:18084"   # your LAN IP for phone clients
$env:VOIDBAR_AUTH_REGISTRATION= "open"                        # default is "closed"; or use `voidbar user add`
.\voidbar.exe serve
```

### Linux (Go)

```bash
go build -o voidbar ./voidbar
mkdir -p ~/.local/share/voidbar
export VOIDBAR_SERVER_LISTEN="127.0.0.1:18084"
export VOIDBAR_SERVER_PUBLIC_URL="http://127.0.0.1:18084"
export VOIDBAR_STORAGE_PATH="$HOME/.local/share/voidbar/data"
export VOIDBAR_AUTH_REGISTRATION=open
./voidbar serve
```

Optional systemd unit (`~/.config/systemd/user/voidbar.service`, then
`systemctl --user enable --now voidbar`):

```ini
[Unit]
Description=Voidbar IRC bouncer

[Service]
Environment=VOIDBAR_SERVER_LISTEN=127.0.0.1:18084
Environment=VOIDBAR_SERVER_PUBLIC_URL=http://127.0.0.1:18084
Environment=VOIDBAR_STORAGE_PATH=%h/.local/share/voidbar/data
Environment=VOIDBAR_AUTH_REGISTRATION=open
ExecStart=%h/.local/bin/voidbar serve

[Install]
WantedBy=default.target
```

### Docker

`Dockerfile` and `docker-compose.yml` are in the repo root. The compose file
bind-mounts `./data` (instance storage) and publishes the server on
`127.0.0.1:18084`.

```bash
docker compose build
docker compose up
```

Plain `docker run` equivalent:

```bash
docker build -t voidbar .
docker run --rm -p 127.0.0.1:18084:8080 \
  -v "$PWD/data:/data" \
  -e VOIDBAR_SERVER_LISTEN=0.0.0.0:8080 \
  -e VOIDBAR_SERVER_PUBLIC_URL=http://127.0.0.1:18084 \
  -e VOIDBAR_AUTH_REGISTRATION=open \
  voidbar
```

Note: inside a container the server must listen on `0.0.0.0` (the compose
file already does); clients talk to `127.0.0.1:18084` via the port mapping.
`VOIDBAR_STORAGE_PATH=/data` is baked into the image.

### Configuration

Env vars can also live in a TOML file (`--config path`); keys mirror them
(`server.listen`, `auth.registration`, ...).

## Not implemented yet

- **History prefill via `draft/chathistory`**: the bouncer replays only what
  it saw while connected; it does not ask the upstream server for older
  history on join ( Ergo/solanum expose it, many networks don't —
  testnet.ergo.chat was probed but its hosting subnets were unreachable).
  History is also capped at 500 messages per channel (ring buffer).
- **DMs**: IRC queries (PRIVMSG to a nick) are received upstream but skipped
  with a log line (`irc query skipped`); no DM channels in the client.
- **Upstream auto-reconnect**: connections are (re)established at boot
  (`EnsureAll`) and on join, but a dropped upstream link is not retried
  until the server restarts or the network is re-joined.
- **Presence / member list**: guild member list (beyond memberships + the
  owner bot), online/offline presence, typing indicators (`+typing`),
  unread badges/read state are stubs.
- **Message features**: edits/deletes (redaction), reactions (`+react`),
  attachments/file uploads, embeds, mentions, pins (empty stub), search.
- **Channel management UI**: joining/parting channels at runtime from the
  client, channel create/rename/topic (topic renders as empty), channel
  categories — auto-join channels from the connection string only
  (re-pasting a string for the same network merges new channels in).
- **Identity**: no nick change from the client (`/nick`), no away, no SASL
  (only server password via `pass@` in the connection string), no IRCv3
  caps negotiation (server-time/msgid/batch/multiline ignored; relayed
  messages carry receive-time timestamps).
- **Settings**: legacy client settings are persisted per user
  (`PATCH /users/@me/settings`); the Android client's appearance settings
  are not synced and reset on reload.
- **Invites**: connection strings only — no shareable invite codes for
  other users, no invite expiry/uses semantics (the payload claims
  unlimited).
- **Admin**: `voidbar user add/list`, `voidbar invite create/list` exist;
  no admin UI, no per-user network management beyond the client.
- Voice/video, threads, forums, stickers, guild discovery — out of scope
  for now (stubs return empty shapes).

## Known issues / troubleshooting

- **Remote-auth QR** (`/remote-auth`) is stubbed; the client hardcodes
  `wss:` so on an http instance you'll see periodic WS errors in the
  client logs. Non-fatal, login by email/password works.
- The client is RU-localized in our test profile; UI labels in this README
  are the Russian ones ("Добавить сервер" etc.).

## Planned stack

- Go 1.22+
- Embedded [BadgerDB](https://github.com/dgraph-io/badger)
- IRCv3: SASL, server-time, message-tags, away-notify, account-notify, batches,
  multiline, nick v3, `+typing`, `+react`/`+unreact`, `draft/message-redaction`,
  `draft/ICON` (network icon)
- Discord Gateway v10 + REST v9 (scoped to frozen/patched client builds)

## License

MIT
