# Discord client compatibility notes

Client quirks the server compensates for, plus known client-side issues we
explicitly do not fix server-side.

## Android client

[discord-apk-patcher](https://github.com/CyberL1/discord-apk-patcher)
repackages the stock Discord Android build (decode → repoint hosts → rebuild →
sign). A few Android-specific compatibility details worth knowing about:

- **Gateway frame field order matters**: dispatch frames serialize `op` first,
  then `t`/`s` before `d` — the client's streaming JSON parser
  (IncomingParser) reads the header before the body. Go emits struct fields in
  declaration order, so `internal/discord/gateway/types.go` pins the order.
- **`nsfw_allowed: true`** on the user object doubles as "this account has a
  date of birth" for the client (MeUser maps it through NsfwAllowance).
  Without it, every account with a 2021+ snowflake hits the un-dismissable
  REGISTER_AGE_GATE modal after login.
- **IRC authors get deterministic snowflake ids**: the client parses message
  author ids as 64-bit integers; a literal `"irc:<nick>"` crashes its
  deserializer and takes down message rendering and the gateway dispatch
  handler.
- **Sends are right-trimmed**: the Android compose box appends a trailing
  newline to every message; real Discord trims it server-side, so the bouncer
  does too.
- **Snowflakes arrive as JSON numbers**: although the Discord docs specify
  snowflake IDs as strings, this client serializes them as bare numbers in
  outgoing gateway payloads (op 8 `guild_id`/`user_ids` arrive as
  `[1541479714630139904]`). Never unmarshal client-sent snowflakes into a
  string field — normalize through `rawIDsToStrings`
  (`internal/discord/gateway/server.go`), which accepts string, number and
  arrays of either.
- **Post-login probes are stubbed** so they don't 404-loop:
  `POST /auth/fingerprint`, `GET /users/{id}/profile`,
  `GET /users/@me/survey`, `POST /users/@me/devices`,
  `GET /guilds/{id}/preview` ("Delete server" in settings is
  `POST /guilds/{id}/delete` — also routed).
  (`GET /sticker-packs` still 404s — harmless.)

`VOIDBAR_READY_MINIMAL=1` on `serve` shrinks the READY payload to the minimum
known-good set — a bisect switch for future client-compat work.

## Known issues

- **Peer avatars are nick-scoped.** IRC metadata attaches to the services
  account, but every Discord-side identity in voidbar is the nick (the
  author-id seed). A user switching nicks starts with a blank avatar until
  they set (or change) one from the new nick — eris does not re-push current
  values on JOIN (the spec makes it a SHOULD).
- **Remote-auth QR** (`/remote-auth`) is stubbed; the client hardcodes `wss:`,
  so on an http instance you'll see periodic WS errors in the client logs.
  Non-fatal; login by email/password works.
- The test client is RU-localized; UI labels elsewhere ("Добавить сервер"
  etc.) are the Russian ones.
- **Profile shows IRC channel modes as guild-wide roles (wontfix).** IRC
  membership prefixes are per-channel; Discord roles are guild-global. The
  channel member list always shows the per-channel truth, but a member profile
  shows their highest mode across channels. Splitting the user id per channel
  (one Discord user per channel) would break DMs, mentions and message
  authorship continuity, so one identity per nick stands and the imprecise
  profile is accepted.
- **Flicker: a deleted message can reappear after scrolling (client-side,
  wontfix here).** Flicker keeps a local scroll cache that is appended on
  every render and never evicted on `MESSAGE_DELETE`: delete the newest
  message and scroll near the bottom, and the cached copy is re-inserted
  (white copy; messages sent from Flicker itself can additionally leave a gray
  `temp-<nonce>` ghost). The server side was verified innocent on the wire and
  with a Playwright e2e, and the same ghosts reproduce against Oldcord
  Staging — an independent server — so no server payload can fix it; the
  client must evict its cache on delete. Self-heals on channel switch,
  reload, or the next message arriving.
