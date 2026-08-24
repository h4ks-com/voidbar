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
}

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
		store: store,
		gw:    gw,
		log:   log,
		sf:    sf,
		conns: make(map[string]*conn),
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
	client := girc.New(cfg)

	c := &conn{
		userID:    userID,
		networkID: networkID,
		client:    client,
		cancel:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	m.conns[k] = c

	m.registerHandlers(c)

	go func() {
		<-c.cancel
		client.Close()
	}()
	go func() {
		defer close(c.done)
		m.log.Info("irc connecting", "user", userID, "network", networkID, "server", net.Host)
		if err := client.Connect(); err != nil {
			m.log.Warn("irc connect failed", "user", userID, "network", networkID, "err", err)
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
			channels = strings.Join(mem.AutoJoin, ",")
			for _, ch := range mem.AutoJoin {
				client.Cmd.Join(ch)
			}
		}
		m.log.Info("irc connected", "user", c.userID, "network", c.networkID, "autojoin", channels)
	})
}

func (m *Manager) dispatchMessage(c *conn, target, author, content, ts string) {
	if !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "&") {
		// Queries (DMs) arrive as a bare nick target; DM wiring comes later.
		m.log.Info("irc query skipped", "user", c.userID, "from", author)
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
			"id":            "irc:" + author,
			"username":      author,
			"discriminator": "0",
			"bot":           false,
		},
	}
	m.log.Info("irc message relayed", "user", c.userID, "network", c.networkID, "from", author, "target", target, "msg_id", msgID)
	m.gw.Dispatch(c.userID, "MESSAGE_CREATE", payload)
}

// SendChannel relays a Discord message into an IRC channel.
func (m *Manager) SendChannel(userID, networkID, channel, content string) error {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("not connected to %s", networkID)
	}
	c.client.Cmd.Message(channel, content)
	return nil
}
