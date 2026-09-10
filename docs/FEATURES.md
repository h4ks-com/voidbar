# Feature notes

Background and implementation notes behind the feature list in the README.
Everything here works end-to-end against a live IRC network unless noted
otherwise.

## Platform

- **REST v9 + Gateway v10**: HELLO/IDENTIFY/READY/HEARTBEAT/RESUME,
  zlib-stream, session replay — enough for the client to boot and render.
- **Register/login** (argon2id, raw bearer tokens) and a `user add/list` CLI.
  Admin is CLI-only; no web panel. While the server runs, badger's storage lock
  blocks the CLI's direct path — pass `--server <base-url>` to provision
  through the master-key admin API (`POST/GET /api/v9/admin/users`,
  `X-Master-Key: <hex of master.key>`).
- **Connection strings as invites**: paste `irc://host:port/#chan?name=X` into
  the client's "Join a server" field → preview card, join, GUILD_CREATE, the
  guild appears in the rail, the client navigates into the pasted channel.
- **Channel registry**: snowflake channel ids — IRC names never hit URLs.
- **Server lifecycle**: leaving removes membership and disconnects upstream;
  the network (channels, replay buffers) is garbage-collected when the last
  member leaves.
- **Upstream auto-reconnect** with backoff; nick collisions survive (the live
  wire nick is what the client shows).
- **Channel management**:
  - *Create*: IRC offers no create API, so creates are optimistic JOINs; an
    upstream refusal (invite-only, banned, account-required, +k, full) rolls
    the channel back and Clyde DMs the reason.
  - *Delete*: PART; history is kept, so re-adding the channel recovers it.
  - *Keys*: keyed (+k) channels take their key inline in the connection string
    (`#chan:key`); keys are network-wide metadata, re-pasting rotates them.
  - *Topics* round-trip both ways: client edits relay as TOPIC; every network
    broadcast (join's RPL_TOPIC, peers, own echo) persists and dispatches
    CHANNEL_UPDATE; a rejection upstream (+t, not op) never corrupts the stored
    truth.
  - *Renames* relay as `draft/channel-rename RENAME`; the broadcast rewrites
    the registry under the same snowflake id and every membership's auto-join;
    a rejected rename (not op, name taken) leaves the old truth standing.

## Messaging

- **Relay both ways**: typing in the client reaches the IRC channel (own nick,
  collision-suffixed, shown as the author); channel PRIVMSGs arrive live as
  MESSAGE_CREATE and render in the client; nick collisions don't eat foreign
  messages. Unread badges work.
- **Multiline**: the composer's shift+enter travels as `draft/multiline`
  batch(es) upstream (blank inner lines intact, trailing blanks trimmed) and
  comes back joined — one message, one msgid, one reaction anchor. The
  advertised `max-bytes`/`max-lines` budget (read off the CAP LS value) splits
  bigger pastes into several batches, and lines too long for one frame are
  word-split into `draft/multiline-concat` chunks that re-join without a
  newline (both directions). Upstreams without the cap degrade to one PRIVMSG
  per non-empty line (blank lines cannot survive the wire there). Incoming
  batches — live or nested inside a chathistory page — join into a single
  message with embedded newlines; `message-tags` is requested so the batch
  frames carry their `@batch` reference client-to-server. Wire writes go
  through a per-connection ordered queue: girc's rate limiter (~1s/event)
  paces batches without ever blocking the REST send path.
- **Replay buffer**: the last 500 messages per channel are persisted (Badger)
  and served over `GET /channels/:id/messages` with Discord pagination
  (`limit`/`before`/`after`); own sends are buffered too; history survives
  server restarts.
- **Chathistory prefill and backfill**: on upstreams that offer
  `draft/chathistory` (eris, ergo, soju), joining a channel asks the network
  for its most recent 50 messages and merges them into the replay buffer —
  server-time timestamps, time-anchored snowflakes (id order stays
  chronological), msgid-anchored (so prefilled messages are reactable and
  deletable), deduped against everything the bouncer ever buffered, and
  one-shot per channel (a persisted watermark keeps reconnects from re-asking).
  Scrolling past the buffer floor keeps going: a short `?before=` page
  transparently asks the network for older history (msgid-anchored BEFORE,
  inserted silently — no gateway dispatch) and re-reads, until the network's
  own history runs dry. Networks without the cap keep bouncer-only history.
- **Search**: the client search box works over the replay buffer — IRC has no
  server-side search, so results cover everything the bouncer has seen (live
  traffic plus chathistory pulled during scrolls). Both routes live:
  `GET /channels/:id/messages/search` and the one the official client calls,
  `GET /guilds/:id/messages/search` (scoped via repeated `?channel_id=`).
  Terms arrive as `?content=` (tokenized `?contents=slop|text` too), `?text=`,
  or `?query=`; ANDed case-insensitively over content and author names.
  Documented response shape: per-hit context groups with `hit: true`, newest
  first, 25/page via `?offset=`, `total_results` across pages. Filter syntax
  beyond free text (`author_id`, `has:`, `from:`) is not interpreted yet.
- **DMs**: both directions, history replay, client-initiated DMs
  (`POST /users/@me/channels` — recipient resolved from the member sidebar or
  fellow bouncer users).

## Members, presence, identity

- **Member sidebar**: per-channel lists from live NAMES state, hoisted role
  sections for IRC prefixes (`~&@%+` → Founder/Admin/Operator/Half-op/Voice,
  colored names), live JOIN/PART/QUIT/KICK/MODE updates; away shown as "idle"
  (away-notify cap, WHO seeding, lazy poller fallback for servers without it).
- **Presence**: the status picker maps onto IRC AWAY on every network of the
  user — online returns, idle/dnd go away (dnd says so), invisible is a no-op
  (IRC can't hide a connected client). The persisted status re-asserts on
  every reconnect (IRC forgets AWAY across connections); others' away renders
  as "idle" (see member sidebar).
- **Peer facts** (extended-join, account-notify, chghost, WHO 352): per-nick
  services account and user@host, seeded at JOIN, updated by ACCOUNT/CHGHOST
  broadcasts, surviving renames — the profile sheet resolves the hashed author
  id back to them (nick as display name, "NickServ: ..." and "Host: ..." lines
  in the bio).
- **Nick change** from the client: Edit Server Profile → Nickname
  (`PATCH /guilds/:id/members/@me`) relays IRC NICK; the server's own-nick
  echo — which also catches ghost reclaims and collision renames — persists
  the membership and pushes GUILD_MEMBER_UPDATE, so the display name follows
  the live wire nick. A rejected change (nick in use) leaves the stored nick
  standing.
- **Avatars** (IRC counterpart: `draft/metadata-2`, the avatar key — eris
  serves it): global avatar upload through `PATCH /users/@me`, per-guild
  override through `PATCH /guilds/:id/members/@me`. Stored images are served
  from `/avatars/{uid}/{hash}.png` off the discovered CDN base (the same
  origin the client already uses for attachments). Global-vs-guild follows
  Discord semantics — per-guild wins in its guild — with one IRC twist:
  setting the global avatar fans `METADATA * SET avatar` out to every upstream
  speaking draft/metadata-2 where the bouncer holds a services account (eris:
  `REGISTER * * <pass>` then SASL, verified live). Inbound, the bouncer
  subscribes (`METADATA * SUB avatar`) on connect, mirrors peers' avatar URLs
  into the local store (hash of the bytes, so a changed picture is a new
  hash), and refreshes member rows via GUILD_MEMBER_UPDATE.

## Interactions

- **Mentions** both ways: outgoing Discord markers relay as bare nicks (IRC
  convention — no `@`), incoming bare nicks (`nick: look`, `@nick` too) become
  `<@id>` pills with a real `mentions` array, and `#channel` references
  markerize both ways (`<#id>` ↔ `#name`). Own sends carry the mentions arrays
  too, so pills highlight. Peers are upserted into the clients' user stores
  via GUILD_MEMBER_UPDATE on foreign JOIN, on roster sweeps after every
  upstream (re)connect, and ahead of incoming mention messages — without
  that, pills render `@invalid-user` (clients ingest users from neither the
  mentions array nor the member-list rows). Candidate nicks are the channel's
  live roster plus every bouncer member of the network; unknown ids/nicks
  pass through verbatim. Renamed channels keep their roster via a rename
  alias (girc predates `draft/channel-rename`), and member lists carry bouncer
  members under their real user ids — one row per person everywhere
  (autocomplete included).
- **Reactions** both ways on msgid upstreams (eris fork, ergo, soju): pills
  with count/me, `+draft/react`/`+draft/unreact` TAGMSGs with
  `+reply`/`+draft/reply`, REST PUT/DELETE bridging, restart-proof (msgid
  registry + reaction state persisted). The picker is hidden on networks
  without msgids (`MSGREFTYPES`-gated). Verified live against a locally built
  eris fork (fastidious/eris) with Halloy as the IRC peer.
- **Reaction reactors**: `GET /channels/:id/messages/:id/reactions/:emoji` —
  who reacted (tap on a pill): members as their real user rows, IRC peers
  resolved from live facts.
- **Typing indicators** both ways: client typing → `@+typing` TAGMSG (with
  `done` on send), IRC typing → TYPING_START. Works on any `message-tags`
  server (no capability advertisement needed; honors `CLIENTTAGDENY`; outbound
  `pause` is not synthesized — the indicator just expires).
- **Pins** (bouncer-local): the client's pin button PUTs/DELETEs
  `/channels/:id/pins/:mid` (204, 50-pin ceiling), `GET /pins` lists pinned
  replay-buffer messages oldest-first, and pin flips fan MESSAGE_UPDATE
  (partial) + CHANNEL_PINS_UPDATE. Pins whose message aged out of the buffer
  stay stored but drop from the list. Pinning drops the "{user} pinned a
  message" system row (type 6) into the channel, live and replayed.
- **User notes** ("Add Note" in the profile sheet): `GET/PUT
  /users/@me/notes/:id` + the bulk map, the READY `notes` field, and
  USER_NOTE_UPDATE fan-out across sessions. Notes target any user-shaped id
  (members and IRC peers alike); an empty note clears.
- **Invite relays** (invite-notify): INVITE broadcasts for channels we're in
  land as channel messages from the inviter; a direct INVITE aimed at us lands
  as a DM ("invited you to #chan") instead of vanishing. No ghost channel rows
  for channels we're not in.
- **Standard replies** (FAIL/WARN/NOTE, standard-replies cap): a reply whose
  context names a joined channel surfaces there as a message from the
  "server" pseudo-user; everything else (registration-time, NickServ) is
  logged.

## Media

- **Attachments** both ways, on the bouncer's own storage:
  - *Sending*: the documented cloud-upload flow (`POST
    /channels/:id/attachments` mints TTLed slots, `PUT /api/v9/uploads/<token>`
    takes the bytes — the token is the credential, like the presigned GCS URL
    it replaces; image dimensions are sniffed) plus the legacy multipart
    `files[0]`-style inline uploads. The Discord copy carries full attachment
    rows (persisted in the replay buffer, so history renders them); the IRC
    wire copy appends the public `/attachments/:id/:filename` URLs — IRC
    peers fetch them token-free, exactly like a pasted link.
  - *Receiving*: direct image URLs in IRC messages are mirrored into local
    storage and attached as attachment rows (live and in chathistory
    backfill). Embed-image rendering is broken in the official client on
    third-party instances (the same build shows a blank box against Oldcord
    Staging), while attachments render fine — so unfurls ride the upload
    pipeline. The fetcher tolerates hosts that dribble the body (partial read
    after a 4s budget, headers live up front). `/attachments/refresh-urls`
    echoes URLs back — ours never expire.

## Settings and customization

- **Settings sync**: client settings survive reloads. The legacy store
  (`PATCH /users/@me/settings`) merges PATCH bodies into persisted settings
  and answers with the full object (the client deserializes
  `ModelUserSettings`; a bare 204 crashed it). The proto store
  (`PATCH /users/@me/settings-proto/{type}`) persists the serialized blob per
  user and kind, merging by top-level field number straight from the protobuf
  wire format — no schema linked. Theme, locale and appearance settings stick
  across restarts.
- **Identity**: server password (`user:pass@` in the connection string) and
  SASL PLAIN (`?sasl=user:pass` — replaces the server password; credentials
  are network-wide metadata, re-pasting rotates them); nick change is covered
  above.
- **Channel categories**: local grouping only (IRC has none) — type 4 channels
  recorded on the network; create/rename/delete from the client, move channels
  in and out, sidebar groups by parent. Nothing upstream ever hears about
  them.
- **Network rename**: the guild settings' Overview `PATCH /guilds/:id` renames
  the network (1-100 chars); GUILD_UPDATE fans the full guild payload so every
  session re-renders the rail. @everyone carries MANAGE_GUILD so the client
  unlocks the rename field.
