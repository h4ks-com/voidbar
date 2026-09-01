package ircmanage

import (
	"fmt"
	"io"
	"net/http"
	"strings"

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

// handleMetadataEvent processes an inbound `METADATA <target> <key>
// <visibility> <value>` notification. Own-nick targets are skipped: the
// bouncer is the source of truth for its own avatar.
func (m *Manager) handleMetadataEvent(c *conn, client *girc.Client, e *girc.Event) {
	if len(e.Params) < 3 || e.Params[1] != metadataAvatarKey {
		return
	}
	nick := e.Params[0]
	if nick == "" || strings.EqualFold(nick, client.GetNick()) {
		return
	}
	url := ""
	if len(e.Params) > 3 {
		url = e.Params[3]
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
