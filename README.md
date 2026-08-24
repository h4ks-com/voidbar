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

Work in progress — initial scaffolding only.

## Planned stack

- Go 1.22+
- Embedded [BadgerDB](https://github.com/dgraph-io/badger)
- IRCv3: SASL, server-time, message-tags, away-notify, account-notify, batches,
  multiline, nick v3, `+typing`, `+react`/`+unreact`, `draft/message-redaction`,
  `draft/ICON` (network icon)
- Discord Gateway v10 + REST v9 (scoped to frozen client builds)

## License

MIT