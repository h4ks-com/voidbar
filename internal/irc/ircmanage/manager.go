// Package ircmanage owns the upstream IRC connections. The invariant: one
// connection per (user, network), each with the member's own nick. This is
// exactly how per-user BNC sessions behave - members of the same network
// never share a socket and appear as regular distinct clients to the server.
package ircmanage

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// Manager holds one connection per (user, network).
type Manager struct {
	store *storage.Storage
	gw    *gateway.Server
	log   *slog.Logger
	sf    *util.Snowflake

	mu    sync.Mutex
	conns map[string]*conn // key: userID + "\x00" + networkID

	// reconnectBackoff is the initial delay before the first retry after
	// an upstream drop; it doubles per consecutive failure (capped at
	// reconnectBackoffMax) and resets once a link stays up for a while.
	reconnectBackoff time.Duration
}

const (
	reconnectBackoffInitial = 5 * time.Second
	reconnectBackoffMax     = 60 * time.Second
	// A link that lived at least this long counts as "was healthy":
	// the next drop retries with the initial backoff again.
	reconnectHealthyAfter = 30 * time.Second
)

type conn struct {
	userID    string
	networkID string
	client    *girc.Client
	cancel    chan struct{}
	done      chan struct{}
}

// New creates the manager.
func New(store *storage.Storage, gw *gateway.Server, log *slog.Logger, sf *util.Snowflake) *Manager {
	return &Manager{
		store:            store,
		gw:               gw,
		log:              log,
		sf:               sf,
		conns:            make(map[string]*conn),
		reconnectBackoff: reconnectBackoffInitial,
	}
}

func key(userID, networkID string) string { return userID + "\x00" + networkID }

// EnsureConn opens an upstream connection for (user, network) unless one
// already exists. The network and membership records must already exist.
func (m *Manager) EnsureConn(userID, networkID string) {
	k := key(userID, networkID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[k]; ok {
		return
	}

	net, err := m.store.GetNetwork(networkID)
	if err != nil {
		m.log.Warn("ensure conn: network missing", "network", networkID, "err", err)
		return
	}
	mem, err := m.store.GetMembership(networkID, userID)
	if err != nil {
		m.log.Warn("ensure conn: membership missing", "user", userID, "network", networkID, "err", err)
		return
	}

	cfg := girc.Config{
		Server: net.Host,
		Port:   net.Port,
		SSL:    net.TLS,
		Nick:   mem.Nick,
		User:   mem.Username,
		Name:   mem.Realname,
	}
	if net.Password != "" {
		cfg.ServerPass = net.Password
	}
	c := &conn{
		userID:    userID,
		networkID: networkID,
		cancel:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	m.conns[k] = c

	// Supervisor: owns the (user, network) link for the lifetime of the
	// membership. Bouncer semantics demand the upstream connection outlives
	// any single TCP session, so every drop - network flap, server restart,
	// ping timeout - is retried with exponential backoff until cancelled
	// (Drop / Leave). A fresh girc client is built per attempt: a client
	// whose read loop died is single-use (tracking state, buffers).
	go func() {
		defer close(c.done)
		backoff := m.reconnectBackoff
		for {
			client := girc.New(cfg)
			m.mu.Lock()
			c.client = client
			m.mu.Unlock()
			m.registerHandlers(c)

			// Per-attempt closer: Drop() may fire while we're blocked
			// inside Connect(); closing the client forces it to return.
			attempt := make(chan struct{})
			go func() {
				select {
				case <-c.cancel:
					client.Close()
					<-attempt
				case <-attempt:
				}
			}()

			m.log.Info("irc connecting", "user", userID, "network", networkID, "server", net.Host)
			started := time.Now()
			cErr := client.Connect()
			lived := time.Since(started)
			close(attempt)

			select {
			case <-c.cancel:
				return
			default:
			}
			if cErr != nil {
				m.log.Warn("irc link down, retrying", "user", userID, "network", networkID, "err", cErr, "backoff", backoff.String(), "lived", lived.Round(time.Millisecond).String())
			} else {
				m.log.Warn("irc link closed, retrying", "user", userID, "network", networkID, "backoff", backoff.String(), "lived", lived.Round(time.Millisecond).String())
			}
			select {
			case <-c.cancel:
				return
			case <-time.After(backoff):
			}
			if lived >= reconnectHealthyAfter {
				backoff = m.reconnectBackoff
			} else if backoff *= 2; backoff > reconnectBackoffMax {
				backoff = reconnectBackoffMax
			}
		}
	}()
}

// EnsureAll opens upstream connections for every recorded membership.
// Upstream connections do not survive server restarts, so this runs at boot.
func (m *Manager) EnsureAll() {
	nets, err := m.store.ListNetworks()
	if err != nil {
		m.log.Warn("ensure all: networks", "err", err)
		return
	}
	for _, net := range nets {
		members, err := m.store.ListMemberships(net.ID)
		if err != nil {
			continue
		}
		for _, mem := range members {
			m.EnsureConn(mem.UserID, net.ID)
		}
	}
}

// Drop closes a connection; it will be re-created on the next EnsureConn.
func (m *Manager) Drop(userID, networkID string) {
	k := key(userID, networkID)
	m.mu.Lock()
	c, ok := m.conns[k]
	if ok {
		delete(m.conns, k)
		close(c.cancel)
	}
	m.mu.Unlock()
	if ok {
		<-c.done
	}
}

func (m *Manager) registerHandlers(c *conn) {
	c.client.Handlers.Add(girc.PRIVMSG, func(client *girc.Client, e girc.Event) {
		// girc already filters echoes natively: events whose source equals
		// the CURRENT nick (GetID, i.e. post-collision) are flagged Echo and
		// never reach command handlers (girc conn.go/handler.go). Do NOT
		// compare against Config.Nick here - that stays at the configured
		// nick after a collision rename, which silently ate the foreign
		// user's messages.
		m.dispatchMessage(c, e.Params[0], e.Source.Name, e.Last(), time.Now().UTC().Format(time.RFC3339))
	})

	c.client.Handlers.Add(girc.CONNECTED, func(client *girc.Client, e girc.Event) {
		channels := ""
		if mem, err := m.store.GetMembership(c.networkID, c.userID); err == nil {
			// The server may have renamed us during registration (nick
			// collision: doesnm -> doesnm_); 001 carries the actual nick
			// and girc tracks it before CONNECTED fires. Persist it so the
			// Discord side shows the nick we really hold on IRC.
			if actual := client.GetNick(); actual != mem.Nick {
				mem.Nick = actual
				if err := m.store.UpsertMembership(mem); err != nil {
					m.log.Warn("irc nick sync failed", "user", c.userID, "nick", actual, "err", err)
				} else {
					m.log.Info("irc nick synced", "user", c.userID, "nick", actual)
				}
			}
			channels = strings.Join(mem.AutoJoin, ",")
			for _, ch := range mem.AutoJoin {
				client.Cmd.Join(ch)
			}
		}
		m.log.Info("irc connected", "user", c.userID, "network", c.networkID, "autojoin", channels)
	})

	// Later renames (ghost reclaim, manual /nick): keep the membership nick
	// in sync so the Discord display name always matches the IRC nick.
	c.client.Handlers.Add(girc.NICK, func(client *girc.Client, e girc.Event) {
		if e.Source == nil || len(e.Params) == 0 {
			return
		}
		// girc has already updated its tracked nick by the time user
		// handlers run, so Params[0] == GetNick() only for our own renames.
		if !strings.EqualFold(e.Params[0], client.GetNick()) {
			return
		}
		if mem, err := m.store.GetMembership(c.networkID, c.userID); err == nil && mem.Nick != e.Params[0] {
			mem.Nick = e.Params[0]
			if err := m.store.UpsertMembership(mem); err != nil {
				m.log.Warn("irc nick sync failed", "user", c.userID, "nick", e.Params[0], "err", err)
			} else {
				m.log.Info("irc nick synced", "user", c.userID, "nick", e.Params[0])
			}
		}
	})
}

func (m *Manager) dispatchMessage(c *conn, target, author, content, ts string) {
	if !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "&") {
		// Query (DM): target is our own nick, author is the peer.
		m.dispatchQuery(c, author, content, ts)
		return
	}
	channelID := ""
	if ch, err := m.store.EnsureChannel(c.networkID, target, m.sf.New); err == nil {
		channelID = ch.ID
	} else {
		m.log.Warn("irc channel resolve failed", "err", err, "target", target)
		return
	}
	// Snowflake message id is mandatory: the client's message store drops
	// MESSAGE_CREATE payloads without one.
	msgID := m.sf.New()
	payload := map[string]any{
		"id":               msgID,
		"channel_id":       channelID,
		"content":          content,
		"timestamp":        ts,
		"edited_timestamp": nil,
		"tts":              false,
		"mention_everyone": false,
		"mentions":         []any{},
		"mention_roles":    []any{},
		"mention_channels": []any{},
		"attachments":      []any{},
		"embeds":           []any{},
		"reactions":        []any{},
		"nonce":            nil,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            model.IrcAuthorID("irc:" + author),
			"username":      author,
			"discriminator": "0",
			"bot":           false,
		},
	}
	m.log.Info("irc message relayed", "user", c.userID, "network", c.networkID, "from", author, "target", target, "msg_id", msgID)
	m.gw.Dispatch(c.userID, "MESSAGE_CREATE", payload)
	// Persist into the channel's replay buffer (bouncer semantics: history
	// survives client reconnects and server restarts). The live relay must
	// not depend on storage health, so failures are logged, not fatal.
	if err := m.store.AppendMessage(storage.BufferedMessage{
		ID:         msgID,
		ChannelID:  channelID,
		AuthorID:   "irc:" + author,
		AuthorName: author,
		Content:    content,
		Timestamp:  ts,
		Type:       0,
	}); err != nil {
		m.log.Warn("buffer append failed", "err", err, "channel", channelID, "msg_id", msgID)
	}
}

// dispatchQuery relays an inbound IRC query (PRIVMSG to our nick) into the
// user's DM channel with the peer. The DM channel is created on first
// contact, announced via CHANNEL_CREATE (so it pops into the DM list) and
// feeds the same replay buffer as channels.
func (m *Manager) dispatchQuery(c *conn, author, content, ts string) {
	dm, err := m.store.EnsureDMChannel(c.userID, c.networkID, author, m.sf.New)
	if err != nil {
		m.log.Warn("dm channel ensure failed", "err", err, "user", c.userID, "from", author)
		return
	}
	// First contact: the client learns about the DM thread through
	// CHANNEL_CREATE (it renders in the Direct Messages list).
	if time.Since(dm.CreatedAt) < 3*time.Second {
		m.gw.Dispatch(c.userID, "CHANNEL_CREATE", m.dmChannelPayload(dm))
	}
	msgID := m.sf.New()
	peerID := model.IrcAuthorID("irc:" + author)
	payload := map[string]any{
		"id":               msgID,
		"channel_id":       dm.ID,
		"content":          content,
		"timestamp":        ts,
		"edited_timestamp": nil,
		"tts":              false,
		"mention_everyone": false,
		"mentions":         []any{},
		"mention_roles":    []any{},
		"mention_channels": []any{},
		"attachments":      []any{},
		"embeds":           []any{},
		"reactions":        []any{},
		"nonce":            nil,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            peerID,
			"username":      author,
			"discriminator": "0",
			"bot":           false,
		},
	}
	m.log.Info("irc query relayed", "user", c.userID, "network", c.networkID, "from", author, "dm", dm.ID, "msg_id", msgID)
	m.gw.Dispatch(c.userID, "MESSAGE_CREATE", payload)
	if err := m.store.AppendMessage(storage.BufferedMessage{
		ID:         msgID,
		ChannelID:  dm.ID,
		AuthorID:   "irc:" + author,
		AuthorName: author,
		Content:    content,
		Timestamp:  ts,
		Type:       0,
	}); err != nil {
		m.log.Warn("buffer append failed", "err", err, "channel", dm.ID, "msg_id", msgID)
	}
	if err := m.store.TouchDMChannel(dm.ID); err != nil {
		m.log.Warn("dm touch failed", "err", err, "dm", dm.ID)
	}
}

// dmChannelPayload shapes a DMChannel for the client (DM channel object:
// type 1, recipients = [peer]).
func (m *Manager) dmChannelPayload(dm *storage.DMChannel) map[string]any {
	return map[string]any{
		"id":       dm.ID,
		"type":     1,
		"flags":    0,
		"last_message_id": nil,
		"recipients": []any{map[string]any{
			"id":            model.IrcAuthorID("irc:" + dm.Nick),
			"username":      dm.Nick,
			"discriminator": "0",
			"bot":           false,
		}},
	}
}

// SendQuery relays a Discord DM into an IRC query PRIVMSG (bare nick).
func (m *Manager) SendQuery(userID, networkID, nick, content string) error {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("not connected to %s", networkID)
	}
	if client == nil {
		return fmt.Errorf("connection to %s is down, retrying", networkID)
	}
	client.Cmd.Message(nick, content)
	return nil
}

// JoinChannel makes the user's upstream connection join an IRC channel
// (used when a re-join merges new channels into an existing membership).
// No-op when the connection is not up; EnsureConn callers get the channel
// via auto-join on (re)connect.
func (m *Manager) JoinChannel(userID, networkID, channel string) {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if !ok || client == nil {
		return
	}
	client.Cmd.Join(channel)
}

// SendChannel relays a Discord message into an IRC channel. Between a drop
// and the next successful reconnect this errors (the supervisor swaps
// c.client under mu, so it is read under the same lock).
func (m *Manager) SendChannel(userID, networkID, channel, content string) error {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("not connected to %s", networkID)
	}
	if client == nil {
		return fmt.Errorf("connection to %s is down, retrying", networkID)
	}
	client.Cmd.Message(channel, content)
	return nil
}
