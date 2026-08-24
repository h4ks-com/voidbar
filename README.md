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

## Running a WIP instance (Windows)

Prerequisites: Go 1.22+ (e.g. `C:\Program Files\Go\bin\go.exe`).

```powershell
# build
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

Then open `http://127.0.0.1:18084/` in a Chromium browser (register on first
run), press **Добавить сервер → Присоединиться к серверу** and paste a
connection string, e.g. `irc://irc.libera.chat:6697/#voidbar?name=Libera`.

Config can also live in a TOML file (`--config path`), keys mirror the env
vars (`server.listen`, `auth.registration`, `client.*`, ...). `mirror_dir`
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

- **Two lazy chunks (38698, 11976) were never archived** by the Wayback
  Machine (crawler wasn't logged in). They produce `ChunkLoadError`
  pageerrors in the console; harmless — everything visible works.
- **Remote-auth QR** (`/remote-auth`) is stubbed; the client hardcodes
  `wss:` so on an http instance you'll see periodic WS errors in the
  console. Non-fatal, login by email/password works.
- **Client cache (OPFS)**: the loader caches raw client bytes in the
  browser. If an asset set ever changes server-side, bump `CACHE_VERSION`
  in `internal/web/static/loader.js` (v3 currently) — old dirs are pruned
  automatically. If the client misbehaves after an update, hard-reload
  (Ctrl+F5) and, as a last resort, wipe the cache manually — DevTools
  console on the instance origin:
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