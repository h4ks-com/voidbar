# Voidbar

Discord-compatible IRCv3 bouncer — like [Spacebar](https://www.spacebar.chat/), but for IRC.

Voidbar speaks the Discord protocol (REST + Gateway) on one side and connects
to IRC networks on the other, so a real Discord client can be used to chat on
IRC.

**Backend only.** Voidbar serves no web client of its own and ships no Discord
assets — bring your own client: repackage the Discord Android build with
[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher) and point
it at your instance (third-party web clients such as Flicker work too, via
`/.well-known/spacebar` discovery). This keeps the project on the same safe
side of the C&D line Spacebar lives on: the server is a clean-room
implementation, the client is the user's own repackaged build.

## Concept

- **A guild is an IRC connection string.** No preconfigured guilds: pasting a
  connection string as an invite creates (or joins) a network; each member
  gets their own upstream connection with their own nick.
- **Multi-user.** Users register independently and hold separate connections
  to the same IRC servers.
- **Bouncer semantics.** Upstream connections persist independently of client
  sessions; history is buffered and replayed on reconnect.
- **Networks are "owned" by [Clyde](https://discord.com/wiki/clyde)** —
  owner-only UI is suppressed ("Delete server" becomes "Leave server"), which
  is what a bouncer user does.

## Status

Beta. Works **end-to-end against a live IRC network** with the Discord
**Android** client (126.21, repackaged) and **web clients** (Flicker):
login → READY → history renders → send/receive relay both ways, no crashes.

**Try it:** register, paste an `irc://…` connection string as an invite, point
your client at the instance — [issues](https://github.com/h4ks-com/voidbar/issues)
and crash reports welcome.

## Features

- **Platform** — REST v9 + Gateway v10 (zlib-stream, resume), register/login
  (argon2id) + admin CLI, `irc://` connection strings as invites, snowflake
  channel registry, server/channel lifecycle (optimistic creates with rollback,
  inline +k keys, topics, `draft/channel-rename` renames), upstream
  auto-reconnect with backoff.
- **Messaging** — bidirectional relay, `draft/multiline` batches both ways,
  500-message replay buffer per channel, `draft/chathistory` prefill/backfill,
  DMs, search over everything the bouncer has seen.
- **People** — member sidebar with IRC prefixes (`~&@%+` → hoisted roles),
  presence ↔ AWAY, peer facts (account, user@host), nick changes, avatars via
  `draft/metadata-2` (both ways, eris).
- **Interactions** — mentions, reactions (`+draft/react`, msgid-gated),
  reaction reactors, typing (`@+typing`), pins, user notes, invite relays,
  standard replies.
- **Media** — attachments both ways on bouncer-local storage (cloud-upload
  flow + legacy multipart; incoming image URLs mirrored and attached).
- **Settings** — settings sync (legacy + settings-proto), channel categories
  (local), network rename, identity (server password / SASL PLAIN).

Details and implementation notes: [docs/FEATURES.md](docs/FEATURES.md).

## Running

Build and run `serve`, then point your client at the instance and register
(or `voidbar user add`). Press **Добавить сервер → Присоединиться к серверу**
and paste a connection string, e.g.
`irc://irc.libera.chat:6697/#voidbar?name=Libera`.

### Windows (Go 1.25+)

```powershell
& "C:\Program Files\Go\bin\go.exe" build -o voidbar.exe ./cmd/voidbar

$env:VOIDBAR_SERVER_LISTEN    = "0.0.0.0:18084"
$env:VOIDBAR_SERVER_PUBLIC_URL= "http://192.168.1.20:18084"   # your LAN IP for phone clients
$env:VOIDBAR_AUTH_REGISTRATION= "open"                        # default is "closed"; or use `voidbar user add`
.\voidbar.exe serve
```

### Linux (Go 1.25+)

```bash
go build -o voidbar ./cmd/voidbar
export VOIDBAR_SERVER_LISTEN="127.0.0.1:18084"
export VOIDBAR_SERVER_PUBLIC_URL="http://127.0.0.1:18084"
export VOIDBAR_STORAGE_PATH="$HOME/.local/share/voidbar/data"
export VOIDBAR_AUTH_REGISTRATION=open
./voidbar serve
```

### Docker

`docker-compose.yml` bind-mounts `./data` and publishes the server on
`127.0.0.1:18084`:

```bash
docker compose build && docker compose up
```

### Configuration

Env vars can also live in a TOML file (`--config path`); keys mirror them
(`server.listen`, `auth.registration`, ...). See `voidbar.example.toml`.

## Discord client compatibility

[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher)
repackages the stock Android build. The server carries several Android-specific
workarounds (gateway frame ordering, age-gate suppression, snowflake
normalization, stubbed probes) — see
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) before touching
`internal/discord/`.

## Roadmap

- Network icon (read from `draft/ICON` ISUPPORT; upload is voidbar-local —
  the spec is read-only server-side).
- IRCv3 to adopt: `monitor` (nick tracking beyond joined channels), `setname`.
- Cleanup: prune permission bits the platform can't honor from the role
  editor.

Out of scope: voice/video, threads/forums, custom emoji, guild discovery.

## Known issues

- Peer avatars are nick-scoped (eris doesn't re-push values on JOIN).
- Remote-auth QR is stubbed — periodic WS errors in client logs on http
  instances; non-fatal.
- Profile shows per-channel IRC modes as guild-wide roles (wontfix).
- Flicker re-inserts deleted messages from its scroll cache (client-side,
  wontfix).

Details: [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## Related projects

- **[matrix2078](https://github.com/h4ks-com/matrix2078)** — the sibling
  project: an IRC server backed by Matrix (Rust, matrix-sdk, native E2EE).
  Stack it under voidbar to reach Matrix from a Discord client:
  `Discord client → voidbar → matrix2078 → homeserver`.

## Stack

- Go 1.25+, embedded [BadgerDB](https://github.com/dgraph-io/badger)
- IRCv3: SASL, server-time, message-tags, away-notify, account-notify,
  batches, multiline, `+typing`, `+react`/`+unreact`,
  `draft/message-redaction`, `draft/ICON`
- Discord Gateway v10 + REST v9 (scoped to frozen/patched client builds)

## License

MIT
