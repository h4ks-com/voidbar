package ircmanage

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// SetPeerAvatarNotifier installs the callback fired when a remote peer's
// avatar arrives or changes (draft/metadata-2 METADATA events and join
// bursts). The Discord side maps it to GUILD_MEMBER_UPDATE.
func (m *Manager) SetPeerAvatarNotifier(fn func(userID, networkID, nick string)) {
	m.peerAvatar = fn
}

// SetAvatar pushes the bouncer account's own avatar URL to one network's
// metadata store (empty url removes the key). Servers where the nick
// holds no services account answer FAIL METADATA KEY_NO_PERMISSION -
// that is normal for anonymous upstreams and logged at debug only.
func (m *Manager) SetAvatar(userID, networkID, url string) {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	if !ok {
		return
	}
	m.sendMetadataSet(c, url)
}

// SetAvatarAll fans an avatar URL out to every network of the user: the
// account-wide avatar from the Discord side is one change, everywhere the
// upstream speaks draft/metadata-2 and the bouncer is logged in.
func (m *Manager) SetAvatarAll(userID, url string) {
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
		m.sendMetadataSet(c, url)
	}
}

// sendMetadataSet emits `METADATA * SET avatar :<url>` on a connection
// (no trailing value removes the key). The sticky ACK flag gates it:
// servers without the extension would just error out.
func (m *Manager) sendMetadataSet(c *conn, url string) {
	if !c.metadataCapUp.Load() {
		return
	}
	m.mu.Lock()
	client := c.client
	m.mu.Unlock()
	if client == nil {
		return
	}
	ev := &girc.Event{Command: "METADATA", Params: []string{"*", "SET", metadataAvatarKey}}
	if url != "" {
		ev.Params = append(ev.Params, url)
	}
	client.Send(ev)
	m.log.Debug("metadata avatar set", "user", c.userID, "network", c.networkID, "url", url)
}

// metadataSubscribe runs once per (re)connect after registration: ask
// for avatar notifications. The server then seeds current values with
// the join bursts and pushes every later change.
func (m *Manager) metadataSubscribe(c *conn, client *girc.Client) {
	if !c.metadataCapUp.Load() {
		return
	}
	client.Send(&girc.Event{Command: "METADATA", Params: []string{"*", "SUB", metadataAvatarKey}})
	m.log.Debug("metadata avatar subscribed", "user", c.userID, "network", c.networkID)
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
// bouncer is the source of truth for its own avatar.
func (m *Manager) handleMetadataEvent(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 3 || e.Params[1] != metadataAvatarKey {
		return
	}
	nick := e.Params[0]
	url := ""
	if len(e.Params) > 3 {
		url = e.Params[3]
	}
	m.applyPeerAvatar(c, client, nick, url)
}

// handleMetadataKeyValue processes 761 RPL_KEYVALUE answers (GET/LIST/
// SYNC payloads and SET confirmations): `761 <ournick> <target> <key>
// <visibility> :<value>`. Same avatar handling as live notifications.
func (m *Manager) handleMetadataKeyValue(c *conn, client *girc.Client, e *girc.Event) {
	// Params: our nick, target, key, visibility, value (last param may
	// be absent for a removed key).
	if len(e.Params) < 4 || e.Params[2] != metadataAvatarKey {
		return
	}
	url := ""
	if len(e.Params) > 4 {
		url = e.Params[4]
	}
	m.applyPeerAvatar(c, client, e.Params[1], url)
}

// handleMetadataNotSet processes 766 RPL_KEYNOTSET (`766 <ournick>
// <target> <key>`): the peer has no avatar - clear any stale mirror.
func (m *Manager) handleMetadataNotSet(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 3 || e.Params[2] != metadataAvatarKey {
		return
	}
	m.applyPeerAvatar(c, client, e.Params[1], "")
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
		_ = m.store.PutPeerAvatar(c.userID, nick, "")
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
		if err := m.store.PutPeerAvatar(c.userID, nick, hash); err != nil {
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

// peerAvatarForUser resolves a nick's mirrored avatar hash across the
// user's networks (payload form: nil when nothing is known) - the store
// is nick-scoped exactly like the author ids the client already holds.
func (m *Manager) peerAvatarForUser(userID, nick string) any {
	if h := m.store.PeerAvatar(userID, nick); h != "" {
		return h
	}
	return nil
}
