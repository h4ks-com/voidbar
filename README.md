# Voidbar

Discord-compatible IRCv3 bouncer (like [Spacebar](https://www.spacebar.chat/), but for IRC).

Voidbar speaks the Discord protocol (REST + Gateway) on one side and connects to
IRC networks on the other, so a real Discord client can be used to chat on IRC.

## Concept

- **A guild is an IRC connection string.** There are no preconfigured guilds:
  pasting a connection string as an invite creates (or joins) a network, and each
  member gets their own upstream IRC connection with their own nick.
- **Multi-user.** Each user registers, joins networks by connection string, and
  holds independent connections to the same IRC server.
- **Bouncer semantics.** Upstream connections persist independently of client
  sessions; message history is buffered and replayed on reconnect.

## Client assets & legal notes

Voidbar stores and serves **no Discord assets**. The bundled web loader
downloads a frozen client build from a mirror you configure (e.g. an
archive.org item, which sends CORS headers) and patches it in the browser;
patched assets are cached client-side in OPFS.

`client.proxy_cdn = true` is an **opt-in** mode where the instance proxies and
caches the assets itself (removes CORS issues, enables Wayback CDX recovery of
missing files). This means the instance distributes Discord-owned assets — the
exposure that drew a cease & desist onto Fosscord. **Only use it on instances
that are not reachable from the public internet** (localhost / LAN / VPN) and
at your own risk.

## Status

Work in progress. Phase 1–3 vertical slice is **working end-to-end against a
live IRC network** (verified with the real Discord web client, build 130303 /
June 2022, loaded through the loader from the Wayback Machine):

- REST v9 + Gateway (HELLO/IDENTIFY/READY/HEARTBEAT/RESUME, zlib-stream,
  session replay) enough for the frozen client build to boot and render
- Register/login (argon2id, raw bearer tokens), `user`/`invite` CLI
- Connection strings as invites: paste `irc://host:port/#chan?name=X` into
  the client's "Join a server" field → preview card, join, GUILD_CREATE,
  the guild appears in the rail, the client navigates into the pasted channel
- Channel registry: snowflake channel ids (IRC names never hit URLs)
- **Discord → IRC**: typing in the client reaches the IRC channel (own nick,
  collision-suffixed, is shown as the author)
- **IRC → Discord**: channel PRIVMSGs are relayed live as MESSAGE_CREATE and
  render in the client; nick collisions don't eat foreign messages
- `voidbar mirror` downloads/repairs the client build locally (brotli/gzip
  bodies from Wayback are decoded; runs self-heal old mirrors)

## Running a WIP instance

Three interchangeable ways — pick one. In all cases: open the instance URL
in a Chromium browser, register on first run, press **Добавить сервер →
Присоединиться к серверу** and paste a connection string, e.g.
`irc://irc.libera.chat:6697/#voidbar?name=Libera`.

**If the client is stuck on a gray/blank screen or crashes**, first read
[Client cache (OPFS)](#client-cache-opfs) below — it is almost always a
poisoned browser cache, not the server.

### Windows (Go)

Prerequisites: Go 1.22+ (e.g. `C:\Program Files\Go\bin\go.exe`).

```powershell
& "C:\Program Files\Go\bin\go.exe" build -o voidbar.exe ./cmd/voidbar

# 1) mirror the frozen client (once; resumable, ~950 files, needs no server)
$env:VOIDBAR_STORAGE_PATH = "$env:TEMP\voidbar-data"   # storage dir for serve
.\voidbar.exe mirror `
  --from "https://web.archive.org/web/20220601000000id_/https://discord.com" `
  --out "$env:TEMP\mirror-2022-06" --html app

# 2) serve (same shell, or put the env vars in a TOML config / start script)
$env:VOIDBAR_SERVER_LISTEN    = "127.0.0.1:18084"
$env:VOIDBAR_SERVER_PUBLIC_URL= "http://127.0.0.1:18084"
$env:VOIDBAR_CLIENT_ENABLED   = "true"
$env:VOIDBAR_CLIENT_CDN_BASE  = "https://web.archive.org/web/20220601000000id_/https://discord.com"
$env:VOIDBAR_CLIENT_HTML      = "app"
$env:VOIDBAR_CLIENT_PROXY_CDN = "true"                                  # opt-in! see legal notes
$env:VOIDBAR_CLIENT_MIRROR_DIR= "$env:TEMP\mirror-2022-06"              # serve assets from the local mirror
$env:VOIDBAR_AUTH_REGISTRATION= "open"                                  # default is "closed"; or use `voidbar user add`
.\voidbar.exe serve
```

### Linux (Go)

```bash
go build -o voidbar ./cmd/voidbar

# 1) mirror the frozen client (once; resumable)
mkdir -p ~/.local/share/voidbar
./voidbar mirror \
  --from "https://web.archive.org/web/20220601000000id_/https://discord.com" \
  --out ~/.local/share/voidbar/mirror-2022-06 --html app

# 2) serve
export VOIDBAR_SERVER_LISTEN="127.0.0.1:18084"
export VOIDBAR_SERVER_PUBLIC_URL="http://127.0.0.1:18084"
export VOIDBAR_STORAGE_PATH="$HOME/.local/share/voidbar/data"
export VOIDBAR_CLIENT_ENABLED=true
export VOIDBAR_CLIENT_CDN_BASE="https://web.archive.org/web/20220601000000id_/https://discord.com"
export VOIDBAR_CLIENT_HTML=app
export VOIDBAR_CLIENT_PROXY_CDN=true                                    # opt-in! see legal notes
export VOIDBAR_CLIENT_MIRROR_DIR="$HOME/.local/share/voidbar/mirror-2022-06"
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
Environment=VOIDBAR_CLIENT_ENABLED=true
Environment=VOIDBAR_CLIENT_CDN_BASE=https://web.archive.org/web/20220601000000id_/https://discord.com
Environment=VOIDBAR_CLIENT_HTML=app
Environment=VOIDBAR_CLIENT_PROXY_CDN=true
Environment=VOIDBAR_CLIENT_MIRROR_DIR=%h/.local/share/voidbar/mirror-2022-06
Environment=VOIDBAR_AUTH_REGISTRATION=open
ExecStart=%h/.local/bin/voidbar serve

[Install]
WantedBy=default.target
```

### Docker

`Dockerfile` and `docker-compose.yml` are in the repo root. The compose file
bind-mounts `./data` (instance storage) and `./mirror` (the mirrored
client) and publishes the server on `127.0.0.1:18084`.

```bash
docker compose build

# one-off: mirror the frozen client into ./mirror (resumable)
docker compose run --rm voidbar mirror \
  --from "https://web.archive.org/web/20220601000000id_/https://discord.com" \
  --out /mirror --html app

docker compose up
```

Plain `docker run` equivalent:

```bash
docker build -t voidbar .
docker run --rm -p 127.0.0.1:18084:8080 \
  -v "$PWD/data:/data" -v "$PWD/mirror:/mirror" \
  -e VOIDBAR_SERVER_LISTEN=0.0.0.0:8080 \
  -e VOIDBAR_SERVER_PUBLIC_URL=http://127.0.0.1:18084 \
  -e VOIDBAR_CLIENT_ENABLED=true \
  -e VOIDBAR_CLIENT_CDN_BASE=https://web.archive.org/web/20220601000000id_/https://discord.com \
  -e VOIDBAR_CLIENT_HTML=app \
  -e VOIDBAR_CLIENT_PROXY_CDN=true \
  -e VOIDBAR_CLIENT_MIRROR_DIR=/mirror \
  -e VOIDBAR_AUTH_REGISTRATION=open \
  voidbar
```

Note: inside a container the server must listen on `0.0.0.0` (the compose
file already does); the browser still talks to `127.0.0.1:18084` via the
port mapping. `VOIDBAR_STORAGE_PATH=/data` is baked into the image.

### Configuration

Env vars can also live in a TOML file (`--config path`); keys mirror them
(`server.listen`, `auth.registration`, `client.*`, ...). `mirror_dir`
makes the proxy serve purely from the local mirror (no network at runtime).

## Not implemented yet

- **History/replay**: `GET /channels/:id/messages` always returns `[]`; the
  bouncer buffer (store + replay on session start/backfill on join) is the
  next phase. Restarting the server loses nothing except unread history.
- **DMs**: IRC queries (PRIVMSG to a nick) are received upstream but skipped
  with a log line (`irc query skipped`); no DM channels in the client.
- **Upstream auto-reconnect**: connections are (re)established at boot
  (`EnsureAll`) and on join, but a dropped upstream link is not retried
  until the server restarts or the network is re-joined.
- **Presence / member list**: guild member list, online/offline presence,
  typing indicators (`+typing`), unread badges/read state are stubs.
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
- **Settings**: client settings PATCHes are acknowledged (204) but not
  persisted; per-user appearance/locale reset on reload.
- **Invites**: connection strings only — no shareable invite codes for
  other users, no invite expiry/uses semantics (the payload claims
  unlimited).
- **Admin**: `voidbar user add/list`, `voidbar invite create/list` exist;
  no admin UI, no per-user network management beyond the client.
- Voice/video, threads, forums, stickers, guild discovery — out of scope
  for now (stubs return empty shapes).

## Known issues / troubleshooting

- **Gray/blank screen on a fresh clone**: two causes, both fixed —
  (1) mirrors downloaded by pre-repair builds contain still-compressed
  bodies: `serve` now self-heals `mirror_dir` at startup (log line
  `mirror: revalidated ...`), and `voidbar mirror --check --out <dir>`
  probes a mirror without writing (exit 1 if repair is needed);
  (2) the browser may have cached those bytes in OPFS — open the
  instance with `?voidbar-wipe` once (see below).
- **Two lazy chunks (38698, 11976) were never archived** by the Wayback
  Machine (crawler wasn't logged in). They produce `ChunkLoadError`
  pageerrors in the console; harmless — everything visible works.
- **Remote-auth QR** (`/remote-auth`) is stubbed; the client hardcodes
  `wss:` so on an http instance you'll see periodic WS errors in the
  console. Non-fatal, login by email/password works.

### Client cache (OPFS)

The loader caches raw client bytes in the browser. If an asset set ever
changes server-side, bump `CACHE_VERSION` in
`internal/web/static/loader.js` (v3 currently) — old dirs are pruned
automatically on load. To wipe the cache by hand:

- open the instance with `?voidbar-wipe` in the URL
  (e.g. `http://127.0.0.1:18084/?voidbar-wipe`) — works even when the
  cached client is too broken to boot, or
- press **Ctrl+Alt+Shift+R** on the page, or
- as a last resort, run this in DevTools on the instance origin:
  ```js
  const root = await navigator.storage.getDirectory();
  await root.removeEntry('voidbar', { recursive: true });
  ```

- The client is RU-localized in our test profile; UI labels in this README
  are the Russian ones ("Добавить сервер" etc.).

## Planned stack

- Go 1.22+
- Embedded [BadgerDB](https://github.com/dgraph-io/badger)
- IRCv3: SASL, server-time, message-tags, away-notify, account-notify, batches,
  multiline, nick v3, `+typing`, `+react`/`+unreact`, `draft/message-redaction`,
  `draft/ICON` (network icon)
- Discord Gateway v10 + REST v9 (scoped to frozen client builds)

## License

MIT