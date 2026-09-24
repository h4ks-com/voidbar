package ircmanage

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/lrstanley/girc"
)

// isMetadataCap matches the metadata capability under its draft and final
// names.
func isMetadataCap(cap string) bool {
	return cap == "draft/metadata-2" || cap == "metadata-2"
}

// metadataAvatarKey is the IRCv3-registered metadata key avatars ride on
// (a URL to an image).
const metadataAvatarKey = "avatar"

// metadataBotKey / metadataColorKey are the peer-flag keys the bouncer
// mirrors: `bot` marks remote users as bots (surfaced as user.bot on the
// Discord side), `color` sets a display color (a synthetic Discord role).
// Both are server-wide on the upstream - one value per user, every
// channel.
const (
	metadataBotKey   = "bot"
	metadataColorKey = "color"
)

// SetPeerAvatarNotifier installs the callback fired when a remote peer's
// avatar arrives or changes (draft/metadata-2 METADATA events and join
// bursts). The Discord side maps it to GUILD_MEMBER_UPDATE.
func (m *Manager) SetPeerAvatarNotifier(fn func(userID, networkID, nick string)) {
	m.peerAvatar = fn
}

// SetPeerFactsNotifier installs the callback fired when a remote peer's
// bot flag or color changes. Same GUILD_MEMBER_UPDATE mapping as avatars:
// the member row the client holds re-reads everything from the store.
func (m *Manager) SetPeerFactsNotifier(fn func(userID, networkID, nick string)) {
	m.peerFacts = fn
}

// SetAvatar pushes the bouncer account's own avatar URL to one network's
// metadata store (empty url removes the key). Servers where the nick
// holds no services account answer FAIL METADATA KEY_NO_PERMISSION -
// that rejection rolls the local avatar back (see avatarSetRejected).
func (m *Manager) SetAvatar(userID, networkID, url, prevHash string) {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	if !ok {
		return
	}
	m.sendMetadataSet(c, url, prevHash, false)
}

// SetAvatarAll fans an avatar URL out to every network of the user: the
// account-wide avatar from the Discord side is one change, everywhere the
// upstream speaks draft/metadata-2 and the bouncer is logged in.
func (m *Manager) SetAvatarAll(userID, url, prevHash string) {
	m.mu.Lock()
	var conns []*conn
	prefix := userID + "\x00"
	for k, c := range m.conns {
		if strings.HasPrefix(k, prefix) {
			conns = append(conns, c)
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		m.sendMetadataSet(c, url, prevHash, true)
	}
}

// SetAvatarFailNotifier installs the revert hook fired when an upstream
// rejects the own-avatar METADATA SET: the network service restores the
// previous hash (the Clyde notice is the manager's own).
func (m *Manager) SetAvatarFailNotifier(fn func(userID, networkID, prevHash string, global bool)) {
	m.avatarFail = fn
}
// sendMetadataSet emits `METADATA * SET avatar :<url>` on a connection
// (no trailing value removes the key). The sticky ACK flag gates it:
// servers without the extension would just error out. prevHash arms the
// rejection rollback (avatarSetRejected).
func (m *Manager) sendMetadataSet(c *conn, url, prevHash string, global bool) {
	if !c.metadataCapUp.Load() {
		return
	}
	m.mu.Lock()
	client := c.client
	m.mu.Unlock()
	if client == nil {
		return
	}
	c.avatarSetMu.Lock()
	c.avatarSetPending = true
	c.avatarSetPrevHash = prevHash
	c.avatarSetGlobal = global
	c.avatarSetMu.Unlock()
	ev := &girc.Event{Command: "METADATA", Params: []string{"*", "SET", metadataAvatarKey}}
	if url != "" {
		ev.Params = append(ev.Params, url)
	}
	client.Send(ev)
	m.log.Debug("metadata avatar set", "user", c.userID, "network", c.networkID, "url", url)
}

// avatarSetRejected processes the server's refusal of an own-avatar SET:
// the optimistic local update rolls back (an avatar no IRC peer can see
// is a lie worth un-showing), and Clyde says why - deduped per FAIL code
// so flapping reconnects stay quiet.
func (m *Manager) avatarSetRejected(c *conn, code, desc string) {
	c.avatarSetMu.Lock()
	pending, prevHash, global := c.avatarSetPending, c.avatarSetPrevHash, c.avatarSetGlobal
	c.avatarSetPending = false
	c.avatarSetMu.Unlock()
	m.log.Info("metadata avatar set rejected", "user", c.userID, "network", c.networkID, "code", code, "desc", desc)
	if !pending {
		return
	}
	name := c.networkID
	if net, err := m.store.GetNetwork(c.networkID); err == nil && net.Name != "" {
		name = net.Name
	}
	hint := ""
	if code == "KEY_NO_PERMISSION" {
		hint = " The server requires a registered account - reconnect with SASL (?sasl=user:pass in the connection string)."
	}
	if m.avatarFail != nil {
		m.avatarFail(c.userID, c.networkID, prevHash, global)
	}
	if global {
		// Per-network visibility: the avatar hides in THIS network's
		// guild only; networks that accepted it keep showing it.
		m.clydeSay(c.userID, c.networkID, fmt.Sprintf("%s rejected your avatar (%s: %s), so it's hidden there.%s", name, code, desc, hint))
		return
	}
	m.clydeSay(c.userID, c.networkID, fmt.Sprintf("Your avatar was rejected by %s (%s: %s), so I reverted it locally.%s", name, code, desc, hint))
}

// metadataSubscribe runs once per (re)connect after registration: ask
// for avatar notifications. The server then seeds current values with
// the join bursts and pushes every later change.
func (m *Manager) metadataSubscribe(c *conn, client *girc.Client) {
	if !c.metadataCapUp.Load() {
		return
	}
	client.Send(&girc.Event{Command: "METADATA", Params: []string{"*", "SUB", metadataAvatarKey, metadataBotKey, metadataColorKey}})
	m.log.Debug("metadata subscribed", "user", c.userID, "network", c.networkID)
}

// metadataSyncChannel pulls the current avatar values for everyone in a
// channel right after joining. Pushing current values in the join burst
// is a SHOULD in the spec - some servers (eris) only push changes, so
// without an explicit SYNC the peers' existing avatars never surface.
func (m *Manager) metadataSyncChannel(c *conn, ircChannel string) {
	if !c.metadataCapUp.Load() {
		return
	}
	m.mu.Lock()
	client := c.client
	m.mu.Unlock()
	if client == nil {
		return
	}
	client.Send(&girc.Event{Command: "METADATA", Params: []string{ircChannel, "SYNC"}})
	m.log.Debug("metadata channel sync", "user", c.userID, "network", c.networkID, "channel", ircChannel)
}

// metadataSyncRetry re-asks a postponed sync (RPL_METADATASYNCLATER),
// honoring the server's RetryAfter hint. Capped per target so a busy
// server cannot loop us forever.
func (m *Manager) metadataSyncRetry(c *conn, target string, retryAfter int) {
	if retryAfter <= 0 {
		retryAfter = 5
	}
	c.metaRetryMu.Lock()
	if c.metaRetries == nil {
		c.metaRetries = make(map[string]int)
	}
	c.metaRetries[target]++
	n := c.metaRetries[target]
	c.metaRetryMu.Unlock()
	if n > 3 {
		return
	}
	time.AfterFunc(time.Duration(retryAfter)*time.Second, func() {
		m.mu.Lock()
		ok := m.conns[key(c.userID, c.networkID)] == c
		client := c.client
		m.mu.Unlock()
		if !ok || client == nil || !c.metadataCapUp.Load() {
			return
		}
		client.Send(&girc.Event{Command: "METADATA", Params: []string{target, "SYNC"}})
		m.log.Debug("metadata sync retried", "user", c.userID, "network", c.networkID, "target", target, "attempt", n)
	})
}

// handleMetadataEvent processes an inbound `METADATA <target> <key>
// <visibility> <value>` notification. Own-nick targets are skipped: the
// bouncer is the source of truth for its own facts.
func (m *Manager) handleMetadataEvent(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 3 {
		return
	}
	value := ""
	if len(e.Params) > 3 {
		value = e.Params[3]
	}
	m.applyPeerMeta(c, client, e.Params[0], e.Params[1], value)
}

// handleMetadataKeyValue processes 761 RPL_KEYVALUE answers (GET/LIST/
// SYNC payloads and SET confirmations): `761 <ournick> <target> <key>
// <visibility> :<value>`. Same handling as live notifications.
func (m *Manager) handleMetadataKeyValue(c *conn, client *girc.Client, e *girc.Event) {
	// Params: our nick, target, key, visibility, value (last param may
	// be absent for a removed key).
	if len(e.Params) < 4 {
		return
	}
	// Own-target echo of a SET we issued: the upstream accepted the
	// write, the optimistic local avatar stands.
	if target := e.Params[1]; target == "*" || strings.EqualFold(target, client.GetNick()) {
		c.avatarSetMu.Lock()
		c.avatarSetPending = false
		c.avatarSetMu.Unlock()
		return
	}
	value := ""
	if len(e.Params) > 4 {
		value = e.Params[4]
	}
	m.applyPeerMeta(c, client, e.Params[1], e.Params[2], value)
}

// handleMetadataNotSet processes 766 RPL_KEYNOTSET (`766 <ournick>
// <target> <key>`): the peer has no value - clear any stale mirror.
func (m *Manager) handleMetadataNotSet(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 3 {
		return
	}
	m.applyPeerMeta(c, client, e.Params[1], e.Params[2], "")
}

// applyPeerMeta routes one peer metadata value ("" = unset) by key:
// avatars fetch+store, bot/color flags store directly.
func (m *Manager) applyPeerMeta(c *conn, client *girc.Client, nick, metaKey, value string) {
	switch metaKey {
	case metadataAvatarKey:
		m.applyPeerAvatar(c, client, nick, value)
	case metadataBotKey:
		m.applyPeerBot(c, nick, value)
	case metadataColorKey:
		m.applyPeerColor(c, nick, value)
	}
}

// applyPeerBot mirrors the peer's `bot` flag. Per the IRCv3 bot-metadata
// spec the value is the bot's VERSION - any non-empty value marks a bot.
func (m *Manager) applyPeerBot(c *conn, nick, value string) {
	if nick == "" {
		return
	}
	bot := strings.TrimSpace(value) != ""
	if err := m.store.PutPeerBot(c.userID, c.networkID, nick, bot); err != nil {
		m.log.Warn("peer bot persist failed", "user", c.userID, "nick", nick, "err", err)
		return
	}
	m.firePeerFacts(c, nick)
}

// applyPeerColor mirrors the peer's `color` value: normalized "#rrggbb"
// or cleared on empty/invalid.
func (m *Manager) applyPeerColor(c *conn, nick, value string) {
	if nick == "" {
		return
	}
	color := ""
	if value != "" {
		normalized, ok := model.ParseIrcColor(value)
		if !ok {
			m.log.Debug("peer color ignored", "user", c.userID, "nick", nick, "value", value)
			return
		}
		color = normalized
	}
	if err := m.store.PutPeerColor(c.userID, c.networkID, nick, color); err != nil {
		m.log.Warn("peer color persist failed", "user", c.userID, "nick", nick, "err", err)
		return
	}
	m.firePeerFacts(c, nick)
}

// handleMetadataSyncLater processes 774 RPL_METADATASYNCLATER: the
// server postponed the sync (big channel, load) and says when to retry.
func (m *Manager) handleMetadataSyncLater(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 2 {
		return
	}
	retry := 0
	if len(e.Params) > 2 {
		retry, _ = strconv.Atoi(e.Params[2])
	}
	m.metadataSyncRetry(c, e.Params[1], retry)
}

// applyPeerAvatar mirrors one peer avatar value (empty = clear) from any
// metadata source - notifications, SYNC batches, GET answers.
func (m *Manager) applyPeerAvatar(c *conn, client *girc.Client, nick, url string) {
	if nick == "" || strings.EqualFold(nick, client.GetNick()) {
		return
	}
	if url == "" {
		_ = m.store.PutPeerAvatar(c.userID, c.networkID, nick, "")
		m.firePeerAvatar(c, nick)
		return
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return
	}
	// The fetch runs off the read loop: a slow image host must not stall
	// IRC traffic. One in-flight fetch per nick dedupes bursts.
	lower := strings.ToLower(nick)
	c.peerFetchMu.Lock()
	if c.peerFetching == nil {
		c.peerFetching = make(map[string]bool)
	}
	if c.peerFetching[lower] {
		c.peerFetchMu.Unlock()
		return
	}
	c.peerFetching[lower] = true
	c.peerFetchMu.Unlock()
	go func() {
		defer func() {
			c.peerFetchMu.Lock()
			delete(c.peerFetching, lower)
			c.peerFetchMu.Unlock()
		}()
		hash, err := m.fetchPeerAvatar(url)
		if err != nil {
			m.log.Debug("peer avatar fetch failed", "user", c.userID, "network", c.networkID, "nick", nick, "url", url, "err", err)
			return
		}
		if err := m.store.PutPeerAvatar(c.userID, c.networkID, nick, hash); err != nil {
			m.log.Warn("peer avatar persist failed", "user", c.userID, "nick", nick, "err", err)
			return
		}
		m.firePeerAvatar(c, nick)
	}()
}

// fetchPeerAvatar downloads, validates and stores a remote avatar image,
// returning its hash.
func (m *Manager) fetchPeerAvatar(url string) (string, error) {
	resp, err := fetchClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("avatar fetch: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, storage.MaxAvatarBytes+1))
	if err != nil {
		return "", err
	}
	return m.store.PutAvatar(data, resp.Header.Get("Content-Type"))
}

func (m *Manager) firePeerAvatar(c *conn, nick string) {
	if m.peerAvatar != nil {
		m.peerAvatar(c.userID, c.networkID, nick)
	}
}

func (m *Manager) firePeerFacts(c *conn, nick string) {
	if m.peerFacts != nil {
		m.peerFacts(c.userID, c.networkID, nick)
	}
}

// applyWhoBotFlag mirrors the bot mark from WHO flags: ircds with a bot
// user mode (Unreal's +B, ircd-seven/solanum's +B) include it in the 352
// flags field. When ISUPPORT advertises BOTMODE the mark is authoritative
// - a missing mark clears the badge; without the token a seen mark only
// ever sets (some ircds omit the flag entirely for humans).
func (m *Manager) applyWhoBotFlag(c *conn, client *girc.Client, nick, flags string) {
	if nick == "" || strings.EqualFold(nick, client.GetNick()) {
		return
	}
	botMark := 'B'
	authoritative := false
	if bm, ok := client.GetServerOption("BOTMODE"); ok && len(bm) == 1 {
		botMark = unicode.ToUpper(rune(bm[0]))
		authoritative = true
	}
	mark := strings.ContainsRune(flags, botMark)
	if !mark && !authoritative {
		return
	}
	bot := mark
	if bot == m.store.PeerBot(c.userID, c.networkID, nick) {
		return
	}
	if err := m.store.PutPeerBot(c.userID, c.networkID, nick, bot); err != nil {
		m.log.Warn("peer bot persist failed", "user", c.userID, "nick", nick, "err", err)
		return
	}
	m.firePeerFacts(c, nick)
}

// peerBotForUser resolves a nick's mirrored bot flag on a network.
func (m *Manager) peerBotForUser(userID, networkID, nick string) bool {
	return m.store.PeerBot(userID, networkID, nick)
}

// peerColorForUser resolves a nick's mirrored color on a network
// ("#rrggbb" or "").
func (m *Manager) peerColorForUser(userID, networkID, nick string) string {
	return m.store.PeerColor(userID, networkID, nick)
}

// peerAvatarForUser resolves a nick's mirrored avatar hash on a network
// (payload form: nil when nothing is known).
func (m *Manager) peerAvatarForUser(userID, networkID, nick string) any {
	if h := m.store.PeerAvatar(userID, networkID, nick); h != "" {
		return h
	}
	return nil
}


