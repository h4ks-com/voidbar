# Voidbar

Discord-compatible IRCv3 bouncer (like [Spacebar](https://www.spacebar.chat/), but for IRC).

Voidbar speaks the Discord protocol (REST + Gateway) on one side and connects to
IRC networks on the other, so a real Discord client can be used to chat on IRC.

**Backend only.** Voidbar serves no web client and ships no Discord assets —
bring your own client: repackage the Discord Android build with
[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher) and
point it at your instance. This keeps the project on the same safe side of
the C&D line Spacebar lives on: the server is a clean-room implementation,
the client is the user's own repackaged build.

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

Beta. The vertical slice is **working end-to-end against a live IRC
network** with two client generations at once: the Discord **Android**
client, 126.21, repackaged with
[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher),
and **web clients** (e.g. Flicker), which discover the instance via
`/.well-known/spacebar`: login → READY → channel history renders →
send/receive relay in both directions, no crashes.

**Try it:** register an account, paste an `irc://` connection string as
an invite, and point your client at the instance —
[issues](https://github.com/h4ks-com/voidbar/issues) and crash reports
welcome.

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
- **Channel management**: create channels from the client (IRC servers
  create channels on JOIN, so creates are optimistic; an upstream refusal —
  invite-only, banned, account-required, +k, full — rolls the channel back
  and Clyde DMs the reason) and delete them (PART; history is kept, so
  re-adding the channel recovers it)
- **Discord → IRC**: typing in the client reaches the IRC channel (own nick,
  collision-suffixed, is shown as the author)
- **IRC → Discord**: channel PRIVMSGs are relayed live as MESSAGE_CREATE and
  render in the client; nick collisions don't eat foreign messages
- **Replay buffer**: the last 500 messages per channel are persisted
  (Badger) and served over `GET /channels/:id/messages` with Discord
  pagination (`limit`/`before`/`after`); own sends are buffered too, and
  history survives server restarts
- **Chathistory prefill and backfill**: on upstreams that offer
  `draft/chathistory` (eris, ergo, soju), joining a channel asks the
  network for its most recent 50 messages and merges them into the replay
  buffer - server-time timestamps, time-anchored snowflakes (id order
  stays chronological), msgid-anchored so prefilled messages are
  reactable/deletable, deduped against everything the bouncer ever
  buffered, and one-shot per channel (a persisted watermark keeps
  reconnects from re-asking). Scrolling past the buffer floor keeps
  going: a short `?before=` page transparently asks the network for
  older history (msgid-anchored `BEFORE`, inserted silently - no gateway
  dispatch) and re-reads, until the network's own history runs dry.
  Networks without the cap keep bouncer-only history.

## Android client

[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher)
repackages the stock Discord Android build (decode → repoint hosts → rebuild
→ sign). The server carries a few Android-specific compatibility details
worth knowing about:

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
- **Snowflakes arrive as JSON numbers**: although the Discord docs specify
  snowflake IDs as strings, this client serializes them as bare numbers in
  outgoing gateway payloads (op 8 `guild_id`/`user_ids` arrive as
  `[1541479714630139904]`). Never unmarshal client-sent snowflakes into a
  string field — normalize through `rawIDsToStrings`
  (`internal/discord/gateway/server.go`), which accepts string, number and
  arrays of either.
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

## What works end-to-end (Android client)

- Login/register, guild rail from IRC networks, channel create/delete
  with upstream rollback, message relay both ways, unread badges.
- **DMs**: both directions, history replay, client-initiated DMs
  (`POST /users/@me/channels` — recipient resolved from the member
  sidebar or fellow bouncer users).
- **Member sidebar**: per-channel lists from live NAMES state, hoisted
  role sections for IRC prefixes (`~&@%+` → Founder/Admin/Operator/
  Half-op/Voice, colored names), live JOIN/PART/QUIT/KICK/MODE updates,
  away shown as "idle" (away-notify cap, WHO seeding, lazy poller
  fallback for servers without the cap).
- **Upstream auto-reconnect** with backoff; nick collisions survive
  (the live wire nick is what the client shows).
- **Typing indicators** both ways: client typing -> `@+typing` TAGMSG
  (with `done` on send), IRC typing -> `TYPING_START`. Works on any
  `message-tags` server (no capability advertisement needed; honors
  `CLIENTTAGDENY`; outbound `pause` is not synthesized - the indicator
  just expires).
- **Reactions** both ways on msgid upstreams (eris fork, ergo, soju):
  pills with count/me, `+draft/react`/`+draft/unreact` TAGMSGs with
  `+reply`/`+draft/reply`, REST PUT/DELETE bridging, restart-proof
  (msgid registry + reaction state persisted); the picker is hidden on
  networks without msgids (`MSGREFTYPES`-gated). Verified live against
  a locally built eris fork (fastidious/eris) with Halloy as the IRC
  peer.

## Not implemented yet

- **Message features**: attachments/file uploads, embeds, mentions,
  search. Pins serve an empty stub (`GET /channels/:id/pins`), which both
  clients render as zero pinned messages.
- **Channel management UI**: rename/topic (topic renders as empty), channel
  categories, keyed channels (+k) — auto-join channels from the connection
  string and runtime create/delete are covered
  (re-pasting a string for the same network merges new channels in).
- **Identity**: no nick change from the client (`/nick`), no away toggle
  from the client (upstream away of others renders as "idle"), no SASL
  (only server password via `pass@` in the connection string). Live
  relays stamp receive time; network-fetched history (chathistory
  prefill/backfill) carries server time, so ids and timestamps stay
  chronological across both paths.
- **Settings**: legacy client settings are persisted per user
  (`PATCH /users/@me/settings`); the Android client's appearance settings
  are not synced and reset on reload.
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
- **Profile shows IRC channel modes as guild-wide roles (wontfix).** IRC
  membership prefixes are per-channel; Discord roles are guild-global.
  The channel member list always shows the per-channel truth, but a
  member profile shows their highest mode across channels. Splitting
  the user id per channel (one Discord user per channel) would break DMs,
  mentions and message authorship continuity, so we keep one identity
  per nick and accept the imprecise profile.
- **Flicker: a deleted message can reappear after scrolling (client-side,
  wontfix here).** Flicker keeps a local scroll cache that is appended on
  every render and never evicted on `MESSAGE_DELETE`: delete the newest
  message and scroll near the bottom, and the cached copy is re-inserted
  (white copy; messages sent from Flicker itself can additionally leave a
  gray `temp-<nonce>` ghost). The server side was verified innocent on the
  wire and with a Playwright e2e, and the same ghosts reproduce against
  Oldcord Staging - an independent server - so no server payload can fix
  it; the client must evict its cache on delete. Self-heals on channel
  switch, reload, or the next message arriving.

## Planned stack

- Go 1.22+
- Embedded [BadgerDB](https://github.com/dgraph-io/badger)
- IRCv3: SASL, server-time, message-tags, away-notify, account-notify, batches,
  multiline, nick v3, `+typing`, `+react`/`+unreact`, `draft/message-redaction`,
  `draft/ICON` (network icon)
- Discord Gateway v10 + REST v9 (scoped to frozen/patched client builds)

## License

MIT
