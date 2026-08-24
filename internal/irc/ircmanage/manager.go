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
)

// Manager holds one connection per (user, network).
type Manager struct {
	store *storage.Storage
	gw    *gateway.Server
	log   *slog.Logger

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
func New(store *storage.Storage, gw *gateway.Server, log *slog.Logger) *Manager {
	return &Manager{
		store: store,
		gw:    gw,
		log:   log,
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
		defer close(c.done)
		m.log.Info("irc connecting", "user", userID, "network", networkID, "server", net.Host)
		if err := client.Connect(); err != nil {
			m.log.Warn("irc connect failed", "user", userID, "network", networkID, "err", err)
			return
		}
		<-c.cancel
		client.Close()
	}()
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
		// PRIVMSG from ourselves is echoed back by the server (echo-message);
		// don't re-broadcast our own messages as incoming.
		if e.Source != nil && strings.EqualFold(e.Source.Name, client.Config.Nick) {
			return
		}
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
	channelID := c.networkID + ":" + target
	payload := map[string]any{
		"channel_id": channelID,
		"content":    content,
		"author": map[string]any{
			"id":            "irc:" + author,
			"username":      author,
			"discriminator": "0",
			"bot":           false,
		},
		"timestamp": ts,
	}
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
