// Package ircmanage owns the upstream IRC connections. The invariant: one
// connection per (user, network), each with the member's own nick. This is
// exactly how per-user BNC sessions behave - members of the same network
// never share a socket and appear as regular distinct clients to the server.
package ircmanage

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	// occupancy (optional) is notified when upstream membership events
	// change who sits in a channel (JOIN/PART/QUIT/KICK/MODE/NICK), so
	// the network service can push fresh member lists. An empty
	// ircChannel means "everything this user sees changed" (QUIT/NICK
	// affect every shared channel at once).
	occupancy func(userID, networkID, ircChannel string)

	// linkChange (optional) is notified on upstream link state
	// transitions so the Discord side can mark guilds (un)available.
	linkChange func(userID, networkID string, up bool)

	mu    sync.Mutex
	conns map[string]*conn // key: userID + "\x00" + networkID

	// reconnectBackoff is the initial delay before the first retry after
	// an upstream drop; it doubles per consecutive failure (capped at
	// reconnectBackoffMax) and resets once a link stays up for a while.
	reconnectBackoff time.Duration

	// awayPollInterval paces the no-away-notify fallback WHO poller (one
	// channel per tick). Generous by design: away status is cosmetic, and
	// the bouncer must stay polite to small servers.
	awayPollInterval time.Duration
}

const (
	reconnectBackoffInitial = 5 * time.Second
	reconnectBackoffMax     = 60 * time.Second
	// A link that lived at least this long counts as "was healthy":
	// the next drop retries with the initial backoff again.
	reconnectHealthyAfter = 30 * time.Second
	// Fallback away polling cadence (servers without away-notify only).
	defaultAwayPollInterval = 60 * time.Second
)

type conn struct {
	userID    string
	networkID string
	client    *girc.Client
	cancel    chan struct{}
	done      chan struct{}

	// linkUp tracks upstream connection health (set on CONNECTED, cleared
	// when the read loop dies); drives guild (un)availability on the
	// Discord side.
	linkUp atomic.Bool

	// pendingJoins tracks channels this connection is trying to join
	// (lowercased). Optimistic channel-creates only roll back on upstream
	// error numerics while their channel is pending; a stale numeric for a
	// channel we parted long ago must not trigger a rollback.
	pendMu       sync.Mutex
	pendingJoins map[string]bool

	// away tracks per-nick away state (true = away/idle). Fed by the
	// away-notify cap when the server offers it, and by a lazy classic-WHO
	// sweep otherwise (352's H/G flag); away-notify only pushes CHANGES,
	// so every join also gets one seeding WHO.
	awayMu sync.Mutex
	away   map[string]bool

	// typingLast throttles inbound draft/typing TAGMSG relays per
	// (nick, target): clients display typing for several seconds, so
	// forwarding everything a chatty upstream sends is pure noise.
	typingMu   sync.Mutex
	typingLast map[string]time.Time

	// msgid registry: IRC msgid <-> Discord message identity. Reactions
	// (draft/react +reply=<msgid>) can only be bridged when the msgid can
	// be resolved to the Discord snowflake, so every relayed message with
	// a msgid is registered here. In-memory: reaction state is live-wire
	// only (a restart drops it, like any IRC session would).
	msgidMu     sync.Mutex
	snowToMsgid map[string]string // Discord message id -> IRC msgid
	msgidToRef  map[string]msgRef // IRC msgid -> message identity

	// pendingSends queues the Discord identity of our own outgoing
	// PRIVMSGs per target (lowercased, FIFO). With echo-message the
	// server returns our message stamped with its msgid; the echo pops
	// the queue head and completes the msgid <-> snowflake mapping.
	pendSendMu  sync.Mutex
	pendingSend map[string][]msgRef

	// reactions tracks emoji -> reacting user ids per Discord message id
	// (count/me rendering for message fetches; live updates go out as
	// MESSAGE_REACTION_ADD/REMOVE).
	reactMu   sync.Mutex
	reactions map[string]map[string]map[string]bool
}

// msgRef is the Discord identity of one bridged IRC message.
type msgRef struct {
	Snowflake string
	ChannelID string
	GuildID   string
	// echoed is closed when the echo-message echo for this send arrives;
	// Send* waits on it to turn delivery into a synchronous guarantee.
	echoed chan struct{}
}

// setAway records a nick's away state and reports whether it changed.
func (c *conn) setAway(nick string, away bool) bool {
	if nick == "" {
		return false
	}
	c.awayMu.Lock()
	defer c.awayMu.Unlock()
	if c.away == nil {
		c.away = make(map[string]bool)
	}
	if c.away[nick] == away {
		return false
	}
	if away {
		c.away[nick] = true
	} else {
		delete(c.away, nick)
	}
	return true
}

// isAway reports a nick's away state.
func (c *conn) isAway(nick string) bool {
	c.awayMu.Lock()
	defer c.awayMu.Unlock()
	return c.away[nick]
}

// renameAway re-keys the away state when a tracked user renames (away is
// per-connection, it survives the rename).
func (c *conn) renameAway(oldNick, newNick string) {
	if oldNick == "" || newNick == "" || oldNick == newNick {
		return
	}
	c.awayMu.Lock()
	defer c.awayMu.Unlock()
	if c.away == nil {
		return
	}
	if away, ok := c.away[oldNick]; ok {
		delete(c.away, oldNick)
		if away {
			c.away[newNick] = true
		}
	}
}

// forgetAway drops a nick's away state (they left the network).
func (c *conn) forgetAway(nick string) {
	c.awayMu.Lock()
	defer c.awayMu.Unlock()
	delete(c.away, nick)
}

func (c *conn) markPending(ch string) {
	c.pendMu.Lock()
	c.pendingJoins[strings.ToLower(ch)] = true
	c.pendMu.Unlock()
}

func (c *conn) clearPending(ch string) bool {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	k := strings.ToLower(ch)
	ok := c.pendingJoins[k]
	delete(c.pendingJoins, k)
	return ok
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
		awayPollInterval: defaultAwayPollInterval,
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
		// girc negotiates its known caps itself; draft/typing isn't in its
		// list, so request it alongside (servers without it just NAK and
		// typing becomes a no-op). echo-message is in girc's known set but
		// requested explicitly here for clarity: without it our own
		// messages never come back and their msgids stay unknown, which
		// kills reacting to our own messages from IRC peers.
		SupportedCaps: map[string][]string{"draft/typing": nil, "echo-message": nil},
		// TLS is decided by the connection string (ircs:// / port), not by
		// the server: an STS upgrade closes the connection mid-session and
		// a bare (valueless) "sts" cap - which the eris fork advertises on
		// plaintext - reads as an invalid policy and girc aborts as if
		// MITM'd. Deterministic TLS, no auto-upgrades.
		DisableSTS: true,
	}
	if net.Password != "" {
		cfg.ServerPass = net.Password
	}
	c := &conn{
		userID:       userID,
		networkID:    networkID,
		cancel:       make(chan struct{}),
		done:         make(chan struct{}),
		pendingJoins: make(map[string]bool),
		away:         make(map[string]bool),
		typingLast:   make(map[string]time.Time),
		snowToMsgid:  make(map[string]string),
		msgidToRef:   make(map[string]msgRef),
		pendingSend:  make(map[string][]msgRef),
		reactions:    make(map[string]map[string]map[string]bool),
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

			// Away fallback poller, for servers without away-notify: one
			// classic WHO per tick, round-robin over the channels someone
			// is actually looking at (gateway op 14 subscriptions). Servers
			// with the cap never get polled - away-notify pushes everything.
			go func() {
				ticker := time.NewTicker(m.awayPollInterval)
				defer ticker.Stop()
				next := 0
				for {
					select {
					case <-attempt:
						return
					case <-c.cancel:
						return
					case <-ticker.C:
					}
					m.pollAwayOnce(c, client, &next)
				}
			}()

		m.log.Info("irc connecting", "user", userID, "network", networkID, "server", net.Host)
		started := time.Now()
		cErr := client.Connect()
		lived := time.Since(started)
		close(attempt)
		// Link died: flip the guild to unavailable on the Discord side.
		// Transitions only - a failed connect attempt after a down phase
		// must not re-notify.
		if c.linkUp.Swap(false) {
			m.fireLinkChange(c, false)
		}

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
// Waiting on the supervisor is capped: if the current attempt is stuck in
// a hung dial (client.Close can't interrupt net.Dialer), the goroutine
// exits on its own once Connect returns and sees the cancel.
func (m *Manager) Drop(userID, networkID string) {
	k := key(userID, networkID)
	m.mu.Lock()
	c, ok := m.conns[k]
	if ok {
		delete(m.conns, k)
		close(c.cancel)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		m.log.Warn("drop: supervisor still in a hung dial, abandoning", "user", userID, "network", networkID)
	}
}

// pollAwayOnce sends one classic WHO for the next channel worth checking,
// but only when the server lacks away-notify (with the cap, away-notify
// pushes every change and polling is pure waste). "Worth checking" means
// a channel one of the user's live client sessions holds a member-list
// subscription for - idle sidebars cost nothing.
func (m *Manager) pollAwayOnce(c *conn, client *girc.Client, next *int) {
	if m.gw == nil || client.HasCapability("away-notify") {
		return
	}
	mem, err := m.store.GetMembership(c.networkID, c.userID)
	if err != nil {
		return
	}
	// Resolve the subscribed lists to IRC channel names; the guild-wide
	// everyone list (empty channel id) means all auto-join channels.
	var channels []string
	for _, spec := range m.gw.RequestedMemberLists(c.userID) {
		if spec.GuildID != c.networkID {
			continue
		}
		if spec.ChannelID == "" {
			channels = append(channels, mem.AutoJoin...)
			continue
		}
		if ch, err := m.store.GetChannel(spec.ChannelID); err == nil {
			channels = append(channels, ch.IRCName)
		}
	}
	if len(channels) == 0 {
		return
	}
	if *next >= len(channels) {
		*next = 0
	}
	target := channels[*next]
	*next++
	client.Send(&girc.Event{Command: "WHO", Params: []string{target}})
}

// SetOccupancyNotifier installs the callback fired on upstream
// membership events (JOIN/PART/QUIT/KICK/MODE/NICK). Wire it after both
// the manager and the network service exist (same late-wiring pattern
// as the gateway providers).
func (m *Manager) SetOccupancyNotifier(fn func(userID, networkID, ircChannel string)) {	m.occupancy = fn
}

// SetLinkNotifier installs the callback fired on upstream link state
// transitions (down -> up, up -> down). The Discord side maps these to
// GUILD_UNAVAILABLE / GUILD_CREATE so clients grey the guild out instead
// of hammering member requests against a dead link.
func (m *Manager) SetLinkNotifier(fn func(userID, networkID string, up bool)) {
	m.linkChange = fn
}

// fireLinkChange notifies the network service of a link state transition.
func (m *Manager) fireLinkChange(c *conn, up bool) {
	if m.linkChange == nil {
		return
	}
	m.linkChange(c.userID, c.networkID, up)
}

// LinkUp reports whether the (user, network) upstream link is currently
// connected. Missing connections report down.
func (m *Manager) LinkUp(userID, networkID string) bool {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	return ok && c.linkUp.Load()
}

// notifyOccupancy fans an occupancy change out to the network service.
// Runs on the connection's event loop, after girc's own state handlers
// have applied the event - so list rebuilds observe post-event state.
func (m *Manager) notifyOccupancy(c *conn, ircChannel string) {
	if m.occupancy == nil {
		return
	}
	m.log.Info("occupancy change", "user", c.userID, "network", c.networkID, "channel", ircChannel)
	m.occupancy(c.userID, c.networkID, ircChannel)
}

func (m *Manager) registerHandlers(c *conn) {
	c.client.Handlers.Add(girc.PRIVMSG, func(client *girc.Client, e girc.Event) {
		// girc already filters echoes natively: events whose source equals
		// the CURRENT nick (GetID, i.e. post-collision) are flagged Echo and
		// never reach command handlers (girc conn.go/handler.go). Do NOT
		// compare against Config.Nick here - that stays at the configured
		// nick after a collision rename, which silently ate the foreign
		// user's messages.
		msgid, _ := e.Tags.Get("msgid")
		m.dispatchMessage(c, e.Params[0], e.Source.Name, e.Last(), time.Now().UTC().Format(time.RFC3339), msgid)
	})

	// Own-message echo (echo-message cap): girc flags them Echo and only
	// ALL_EVENTS handlers see them. The echo carries the msgid the server
	// stamped on our PRIVMSG; popping the pending identity for the target
	// completes the snowflake <-> msgid mapping reactions need.
	c.client.Handlers.Add(girc.ALL_EVENTS, func(client *girc.Client, e girc.Event) {
		if !e.Echo || (e.Command != "PRIVMSG" && e.Command != "NOTICE") || len(e.Params) == 0 {
			return
		}
		msgid, _ := e.Tags.Get("msgid")
		ref := c.popPendingSend(e.Params[0])
		if ref.Snowflake == "" {
			return
		}
		// Delivery confirmed: the server processed our PRIVMSG. Some
		// servers echo without a msgid - the confirmation must not depend
		// on one being stamped.
		if ref.echoed != nil {
			close(ref.echoed)
		}
		if msgid == "" {
			return
		}
		c.registerMsgid(ref, msgid)
		if err := m.store.SetMessageMsgID(c.networkID, ref.ChannelID, ref.Snowflake, msgid); err != nil {
			m.log.Debug("msgid persist failed", "err", err, "msg", ref.Snowflake)
		}
		m.log.Debug("msgid mapped", "user", c.userID, "network", c.networkID, "snowflake", ref.Snowflake, "msgid", msgid)
	})

	c.client.Handlers.Add(girc.CONNECTED, func(client *girc.Client, e girc.Event) {
		// Registration completed: the guild is available again. Push a
		// fresh GUILD_CREATE so clients re-render and re-subscribe.
		if !c.linkUp.Swap(true) {
			m.fireLinkChange(c, true)
		}
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
			// Pending so a rejection on reconnect (channel went +i, we got
			// banned...) also rolls the channel back instead of silently
			// shadowing it from the client.
			c.markPending(ch)
			client.Cmd.Join(ch)
		}
		// Seed away state: away-notify only pushes changes, so the users
		// already in the channels need one classic-WHO sweep (352's H/G).
		// One burst per connect, no polling.
		for _, ch := range mem.AutoJoin {
			client.Send(&girc.Event{Command: "WHO", Params: []string{ch}})
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
		// Away state is per-connection: it follows the user across the
		// rename (away-notify events for the old nick won't come anymore).
		c.renameAway(e.Source.Name, e.Params[0])
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

	c.client.Handlers.Add(girc.JOIN, func(client *girc.Client, e girc.Event) {
		// Our own join echo confirms a pending optimistic channel-create.
		if e.Source != nil && strings.EqualFold(e.Source.Name, client.GetNick()) && len(e.Params) > 0 {
			c.clearPending(e.Params[0])
		}
		if len(e.Params) > 0 {
			m.notifyOccupancy(c, e.Params[0])
		}
	})
	// Someone left (or was kicked from) a channel we're in: the sidebar
	// rows are stale until resynced.
	c.client.Handlers.Add(girc.PART, func(client *girc.Client, e girc.Event) {
		if len(e.Params) > 0 {
			m.notifyOccupancy(c, e.Params[0])
		}
	})
	c.client.Handlers.Add(girc.KICK, func(client *girc.Client, e girc.Event) {
		if len(e.Params) > 0 {
			m.notifyOccupancy(c, e.Params[0])
		}
	})
	// Channel MODE changes (op/voice granted or taken) reshuffle role
	// sections; user modes elsewhere on the network are irrelevant.
	c.client.Handlers.Add(girc.MODE, func(client *girc.Client, e girc.Event) {
		if len(e.Params) > 0 && girc.IsValidChannel(e.Params[0]) {
			m.notifyOccupancy(c, e.Params[0])
		}
	})
	// QUIT and renames touch every channel the user shared with us; the
	// affected set isn't recoverable after girc's state handlers ran, so
	// refresh everything.
	c.client.Handlers.Add(girc.QUIT, func(client *girc.Client, e girc.Event) {
		if e.Source != nil {
			c.forgetAway(e.Source.Name)
		}
		m.notifyOccupancy(c, "")
	})
	// away-notify pushes AWAY (with message) / BACK (bare) for users in
	// shared channels - zero polling, the only source of away truth on
	// capabale servers. Away shows as Discord "idle".
	c.client.Handlers.Add(girc.AWAY, func(client *girc.Client, e girc.Event) {
		if e.Source == nil {
			return
		}
		if c.setAway(e.Source.Name, e.Last() != "") {
			m.notifyOccupancy(c, "")
		}
	})
	// Classic WHO replies: the H/G flag in params[6] carries away state.
	// The seed sweep (and the no-away-notify fallback poller) both land
	// here; end-of-WHO (315) triggers the one list refresh for the burst.
	c.client.Handlers.Add("352", func(client *girc.Client, e girc.Event) {
		// <me> <channel> <user> <host> <server> <nick> <flags> :<hop> <real>
		if len(e.Params) < 7 {
			return
		}
		flags := e.Params[6]
		if flags == "" {
			return
		}
		c.setAway(e.Params[5], flags[0] == 'G')
	})
	c.client.Handlers.Add("315", func(client *girc.Client, e girc.Event) {
		if len(e.Params) > 1 {
			m.notifyOccupancy(c, e.Params[1])
		}
	})
	// draft/typing: TAGMSG with a +typing client tag ("active" while the
	// user types, "done" when they stop). Relay "active" to the client as
	// TYPING_START; "done" has no Discord equivalent (the indicator
	// auto-expires).
	// draft/react: TAGMSG with +reply=<msgid> and +draft/react (or
	// +draft/unreact) is a reaction on that message, bridged to
	// MESSAGE_REACTION_ADD / MESSAGE_REACTION_REMOVE.
	c.client.Handlers.Add("TAGMSG", func(client *girc.Client, e girc.Event) {
		if e.Source == nil || len(e.Params) == 0 {
			return
		}
		// Our own TAGMSG echoes (react/typing) must not loop back.
		if strings.EqualFold(e.Source.Name, client.GetNick()) {
			return
		}
		if state, _ := e.Tags.Get("+typing"); state == "active" {
			m.dispatchTyping(c, e.Source.Name, e.Params[0])
			return
		}
		reply, hasReply := e.Tags.Get("+reply")
		if !hasReply || reply == "" {
			// Draft-stage name for the same tag; Halloy sends both, other
			// clients may send only this form.
			reply, hasReply = e.Tags.Get("+draft/reply")
		}
		if !hasReply || reply == "" {
			return
		}
		if emoji, ok := e.Tags.Get("+draft/react"); ok && emoji != "" {
			m.applyReaction(c, e.Source.Name, reply, emoji, false)
			return
		}
		if emoji, ok := e.Tags.Get("+draft/unreact"); ok && emoji != "" {
			m.applyReaction(c, e.Source.Name, reply, emoji, true)
		}
	})
	c.client.Handlers.Add(girc.NICK, func(client *girc.Client, e girc.Event) {
		m.notifyOccupancy(c, "")
	})

	// Upstream join failures for pending creates. IRC servers usually just
	// create channels, but +i/+b/+R/+k/+l and bad names do reject JOINs —
	// the optimistic channel in the client must be rolled back.
	for numeric, why := range joinErrorReasons {
		reason := why
		c.client.Handlers.Add(numeric, func(client *girc.Client, e girc.Event) {
			// Layout for all of these: <our-nick> <channel> [:text]
			if len(e.Params) < 2 {
				return
			}
			m.rollbackJoin(c, e.Params[1], reason)
		})
	}
}

// joinErrorReasons maps IRC numerics that reject JOIN to user-facing text.
var joinErrorReasons = map[string]string{
	"403": "the channel does not exist on this server",
	"471": "the channel is full (+l)",
	"473": "the channel is invite-only (+i)",
	"474": "you are banned from that channel (+b)",
	"475": "the channel requires a key (+k), which Voidbar does not support yet",
	"476": "that is not a valid channel name for this server",
	"477": "the channel requires a registered account (+R)",
}

// rollbackJoin undoes a join the upstream refused (optimistic create or
// auto-join on reconnect): drop the auto-join entry, CHANNEL_DELETE the
// channel out of the client, and have Clyde explain the refusal in a DM.
// The registry record and replay buffer are kept — if the channel becomes
// joinable again (invite granted, ban lifted), re-adding it recovers the
// history.
func (m *Manager) rollbackJoin(c *conn, ircName, reason string) {
	if !c.clearPending(ircName) {
		return // not ours / not pending
	}
	m.log.Warn("irc join rejected", "user", c.userID, "network", c.networkID, "channel", ircName, "reason", reason)
	_ = m.store.MembershipRemoveChannel(c.networkID, c.userID, ircName)
	if ch, err := m.store.GetChannelByIRC(c.networkID, ircName); err == nil {
		m.gw.Dispatch(c.userID, "CHANNEL_DELETE", map[string]any{
			"id":       ch.ID,
			"guild_id": c.networkID,
		})
	}
	m.clydeSay(c.userID, c.networkID, "Could not join **"+ircName+"**: "+reason+".")
}

// clydeSay delivers a system notice as a DM from Clyde (bot), creating the
// thread on first contact like a real query would.
func (m *Manager) clydeSay(userID, networkID, text string) {
	dm, err := m.store.EnsureDMChannel(userID, networkID, "Clyde", m.sf.New)
	if err != nil {
		m.log.Warn("clyde dm ensure failed", "err", err, "user", userID)
		return
	}
	if time.Since(dm.CreatedAt) < 3*time.Second {
		m.gw.Dispatch(userID, "CHANNEL_CREATE", m.dmChannelPayload(dm))
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	msgID := m.sf.New()
	m.gw.Dispatch(userID, "MESSAGE_CREATE", map[string]any{
		"id":               msgID,
		"channel_id":       dm.ID,
		"content":          text,
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
		"author":           model.DMPeer("Clyde"),
	})
	if err := m.store.AppendMessage(storage.BufferedMessage{
		ID:         msgID,
		ChannelID:  dm.ID,
		AuthorID:   "irc:Clyde",
		AuthorName: "Clyde",
		Content:    text,
		Timestamp:  ts,
		Type:       0,
	}); err != nil {
		m.log.Warn("buffer append failed", "err", err, "channel", dm.ID, "msg_id", msgID)
	}
	_ = m.store.TouchDMChannel(dm.ID)
}

func (m *Manager) dispatchMessage(c *conn, target, author, content, ts, msgid string) {
	if !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "&") {
		// Query (DM): target is our own nick, author is the peer.
		m.dispatchQuery(c, author, content, ts, msgid)
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
	if msgid != "" {
		c.registerMsgid(msgRef{Snowflake: msgID, ChannelID: channelID, GuildID: c.networkID}, msgid)
		if err := m.store.SetMessageMsgID(c.networkID, channelID, msgID, msgid); err != nil {
			m.log.Debug("msgid persist failed", "err", err, "channel", channelID, "msg", msgID)
		}
	}
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
func (m *Manager) dispatchQuery(c *conn, author, content, ts, msgid string) {
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
	if msgid != "" {
		c.registerMsgid(msgRef{Snowflake: msgID, ChannelID: dm.ID}, msgid)
		if err := m.store.SetMessageMsgID(c.networkID, dm.ID, msgID, msgid); err != nil {
			m.log.Debug("msgid persist failed", "err", err, "dm", dm.ID, "msg", msgID)
		}
	}
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
		"id":                   dm.ID,
		"type":                 1,
		"flags":                0,
		"last_message_id":      nil,
		"recipients":           []any{model.DMPeer(dm.Nick)},
		"is_message_request":   false,
		"is_spam":              false,
	}
}

// ChannelMember is one channel occupant from the live NAMES state, with
// their highest channel-membership mode (q/a/o/h/v per PREFIX, or "" for
// plain members) and away state (away-notify / WHO H/G).
type ChannelMember struct {
	Nick string
	Mode string
	Away bool
}

// ChannelMembers returns the nicks currently in an IRC channel according
// to girc's live channel state (fed by NAMES on join plus JOIN/PART/QUIT).
// Sorted for stable list rendering. Empty when the channel is unknown.
func (m *Manager) ChannelMembers(userID, networkID, ircName string) []string {
	detailed := m.ChannelMembersDetailed(userID, networkID, ircName)
	nicks := make([]string, 0, len(detailed))
	for _, cm := range detailed {
		nicks = append(nicks, cm.Nick)
	}
	return nicks
}

// ChannelMembersDetailed is ChannelMembers with each occupant's highest
// channel-membership mode, resolved through the PREFIX mapping the server
// advertised (~&@%+ → q a o h v; server-specific prefixes beyond the five
// standard modes have no Discord role and are ignored).
func (m *Manager) ChannelMembersDetailed(userID, networkID, ircName string) []ChannelMember {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if !ok || client == nil {
		return nil
	}
	ch := client.LookupChannel(ircName)
	if ch == nil {
		return nil
	}
	users := ch.Users(client)
	members := make([]ChannelMember, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		mode := ""
		if u.Perms != nil {
			if perms, ok := u.Perms.Lookup(ch.Name); ok {
				switch {
				case perms.Owner:
					mode = model.IrcModeFounder
				case perms.Admin:
					mode = model.IrcModeAdmin
				case perms.Op:
					mode = model.IrcModeOp
				case perms.HalfOp:
					mode = model.IrcModeHalfOp
				case perms.Voice:
					mode = model.IrcModeVoice
				}
			}
		}
		members = append(members, ChannelMember{Nick: u.Nick, Mode: mode, Away: c.isAway(u.Nick)})
	}
	sort.Slice(members, func(i, j int) bool {
		return strings.ToLower(members[i].Nick) < strings.ToLower(members[j].Nick)
	})
	return members
}

// LiveNick returns the nick the user's upstream connection currently
// holds, or "" when there is no live connection. The server may alter
// the requested nick on connect (collisions get a suffix), so this - not
// the stored membership nick - is the nick actually visible in NAMES.
func (m *Manager) LiveNick(userID, networkID string) string {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if client == nil {
		return ""
	}
	return client.GetNick()
}

// SendTyping relays a Discord typing indicator upstream as a
// draft/typing TAGMSG. draft/typing is a CLIENT-ONLY tag: any server
// with message-tags relays it (the draft/typing capability is optional
// and mostly advisory), so the gate is message-tags plus CLIENTTAGDENY.
// Where denied or unsupported, typing is a silent no-op.
func (m *Manager) SendTyping(userID, networkID, target string) error {
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
	if !typingAllowed(client) {
		return nil
	}
	sendTypingTag(client, target, "active")
	return nil
}

// typingAllowed reports whether the upstream will relay +typing tags:
// message-tags must be negotiated, and CLIENTTAGDENY (ISUPPORT) must not
// ban draft/typing ("*" bans everything; "-name" entries are exemptions).
func typingAllowed(client *girc.Client) bool {
	if !client.HasCapability("message-tags") {
		return false
	}
	deny, _ := client.GetServerOption("CLIENTTAGDENY")
	if deny == "" {
		return true
	}
	for _, item := range strings.Split(deny, ",") {
		item = strings.TrimSpace(item)
		if item == "*" {
			return false
		}
		if item == "draft/typing" {
			return false
		}
	}
	return true
}

// sendTypingTag emits one draft/typing TAGMSG.
func sendTypingTag(client *girc.Client, target, state string) {
	client.Send(&girc.Event{
		Tags:   girc.Tags{"+typing": state},
		Command: "TAGMSG",
		Params: []string{target},
	})
}

// dispatchTyping converts an inbound typing TAGMSG into a TYPING_START
// for this user's client sessions: channel targets map to the channel's
// Discord id, queries (TAGMSG to our nick) to the DM thread. Throttled
// per (nick, target) - the client keeps the indicator up for seconds.
func (m *Manager) dispatchTyping(c *conn, nick, target string) {
	k := strings.ToLower(nick) + "\x00" + strings.ToLower(target)
	c.typingMu.Lock()
	if time.Since(c.typingLast[k]) < 3*time.Second {
		c.typingMu.Unlock()
		return
	}
	c.typingLast[k] = time.Now()
	c.typingMu.Unlock()

	channelID, guildID := "", ""
	if ch, err := m.store.GetChannelByIRC(c.networkID, target); err == nil {
		channelID, guildID = ch.ID, c.networkID
	} else {
		// Not a known channel: a query typing (TAGMSG to our nick).
		mem, err := m.store.GetMembership(c.networkID, c.userID)
		if err != nil || !strings.EqualFold(target, mem.Nick) {
			return
		}
		dm, err := m.store.EnsureDMChannel(c.userID, c.networkID, nick, m.sf.New)
		if err != nil {
			return
		}
		channelID = dm.ID
	}
	payload := map[string]any{
		"type":       1,
		"channel_id": channelID,
		"user_id":    model.IrcAuthorID("irc:" + nick),
		"timestamp":  time.Now().Unix(),
	}
	if guildID != "" {
		payload["guild_id"] = guildID
	}
	m.gw.Dispatch(c.userID, "TYPING_START", payload)
}

// SendQuery relays a Discord DM into an IRC query PRIVMSG (bare nick).
// msgRef identifies the Discord message so the echo (echo-message) can be
// correlated with the msgid the server stamps on it.
func (m *Manager) SendQuery(userID, networkID, nick, content string, msgID, channelID string) error {
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
	echoed := make(chan struct{})
	if msgID != "" {
		c.pushPendingSend(nick, msgRef{Snowflake: msgID, ChannelID: channelID, echoed: echoed})
	}
	// Tell the other side typing is over (draft/typing "done"), then the
	// message itself.
	if typingAllowed(client) {
		sendTypingTag(client, nick, "done")
	}
	client.Cmd.Message(nick, content)
	return c.awaitEcho(echoed, msgID)
}

// awaitEcho turns an asynchronous PRIVMSG write into a synchronous
// delivery guarantee when (and only when) the server negotiated
// echo-message: the caller blocks until the echo arrives. Without the
// cap there is nothing to wait for - fire-and-forget, as before. A
// zombie socket (TCP up, server hung) fails the wait, so the REST
// caller can tell the client the message never left.
func (c *conn) awaitEcho(echoed chan struct{}, msgID string) error {
	if msgID == "" || echoed == nil || !c.echoCapable() {
		return nil
	}
	select {
	case <-echoed:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("upstream did not confirm delivery")
	}
}

// echoCapable reports whether the live connection negotiated
// echo-message (delivery confirmations require it).
func (c *conn) echoCapable() bool {
	if c == nil || c.client == nil {
		return false
	}
	return c.client.HasCapability("echo-message")
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
	// Marked pending so an upstream refusal (numerics) can roll the
	// optimistic create back.
	c.markPending(channel)
	client.Cmd.Join(channel)
}

// PartChannel leaves an IRC channel upstream (Discord channel delete).
func (m *Manager) PartChannel(userID, networkID, channel string) {
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
	c.clearPending(channel)
	client.Cmd.Part(channel)
}

// SendChannel relays a Discord message into an IRC channel. Between a drop
// and the next successful reconnect this errors (the supervisor swaps
// c.client under mu, so it is read under the same lock). msgRef identifies
// the Discord message for echo/msgid correlation.
func (m *Manager) SendChannel(userID, networkID, channel, content string, msgID, channelID string) error {
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
	echoed := make(chan struct{})
	if msgID != "" {
		c.pushPendingSend(channel, msgRef{Snowflake: msgID, ChannelID: channelID, GuildID: networkID, echoed: echoed})
	}
	// Tell the other side typing is over (draft/typing "done"), then the
	// message itself.
	if typingAllowed(client) {
		sendTypingTag(client, channel, "done")
	}
	client.Cmd.Message(channel, content)
	return c.awaitEcho(echoed, msgID)
}

// registerMsgid records the msgid <-> Discord identity mapping.
func (c *conn) registerMsgid(ref msgRef, msgid string) {
	c.msgidMu.Lock()
	defer c.msgidMu.Unlock()
	c.snowToMsgid[ref.Snowflake] = msgid
	c.msgidToRef[msgid] = ref
}

// lookupMsgid resolves a Discord message id to its IRC msgid.
func (c *conn) lookupMsgid(snowflake string) (string, bool) {
	c.msgidMu.Lock()
	defer c.msgidMu.Unlock()
	msgid, ok := c.snowToMsgid[snowflake]
	return msgid, ok
}

// lookupRef resolves an IRC msgid to the Discord message identity.
func (c *conn) lookupRef(msgid string) (msgRef, bool) {
	c.msgidMu.Lock()
	defer c.msgidMu.Unlock()
	ref, ok := c.msgidToRef[msgid]
	return ref, ok
}

// refBySnowflake resolves a Discord message id to its full identity.
func (c *conn) refBySnowflake(snowflake string) (msgRef, bool) {
	c.msgidMu.Lock()
	defer c.msgidMu.Unlock()
	msgid, ok := c.snowToMsgid[snowflake]
	if !ok {
		return msgRef{}, false
	}
	ref, ok := c.msgidToRef[msgid]
	return ref, ok
}

// pushPendingSend queues our outgoing message identity for a target; the
// echo-message echo pops it (FIFO: IRC writes on one connection are
// ordered, and so are their echoes).
func (c *conn) pushPendingSend(target string, ref msgRef) {
	c.pendSendMu.Lock()
	defer c.pendSendMu.Unlock()
	k := strings.ToLower(target)
	c.pendingSend[k] = append(c.pendingSend[k], ref)
}

// popPendingSend takes the oldest queued identity for a target.
func (c *conn) popPendingSend(target string) msgRef {
	c.pendSendMu.Lock()
	defer c.pendSendMu.Unlock()
	k := strings.ToLower(target)
	q := c.pendingSend[k]
	if len(q) == 0 {
		return msgRef{}
	}
	ref := q[0]
	q = q[1:]
	if len(q) == 0 {
		delete(c.pendingSend, k)
	} else {
		c.pendingSend[k] = q
	}
	return ref
}

// ReactionCount is one emoji's aggregated reaction state on a message.
type ReactionCount struct {
	Emoji string
	Count int
	Me    bool
}

// reactionSnapshot copies a message's live reaction state for persistence.
func (c *conn) reactionSnapshot(snowflake string) map[string][]string {
	c.reactMu.Lock()
	defer c.reactMu.Unlock()
	byEmoji := c.reactions[snowflake]
	if len(byEmoji) == 0 {
		return nil
	}
	out := make(map[string][]string, len(byEmoji))
	for emoji, users := range byEmoji {
		ids := make([]string, 0, len(users))
		for uid := range users {
			ids = append(ids, uid)
		}
		sort.Strings(ids)
		out[emoji] = ids
	}
	return out
}

// persistReactions stores a message's reaction snapshot. Live-wire only:
// logged, never fatal.
func (m *Manager) persistReactions(c *conn, ref msgRef) {
	if err := m.store.UpdateMessageReactions(ref.ChannelID, ref.Snowflake, c.reactionSnapshot(ref.Snowflake)); err != nil {
		m.log.Debug("reaction persist failed", "err", err, "msg", ref.Snowflake)
	}
}

// trackReaction updates the in-memory reaction state and reports whether
// anything changed (idempotent repeats are dropped). Callers persist the
// snapshot on change so pills survive restarts.
func (c *conn) trackReaction(snowflake, emoji, userID string, remove bool) bool {
	c.reactMu.Lock()
	defer c.reactMu.Unlock()
	byEmoji := c.reactions[snowflake]
	if byEmoji == nil {
		if remove {
			return false
		}
		byEmoji = make(map[string]map[string]bool)
		c.reactions[snowflake] = byEmoji
	}
	users := byEmoji[emoji]
	if users == nil {
		if remove {
			return false
		}
		users = make(map[string]bool)
		byEmoji[emoji] = users
	}
	if remove {
		if !users[userID] {
			return false
		}
		delete(users, userID)
		if len(users) == 0 {
			delete(byEmoji, emoji)
		}
		if len(byEmoji) == 0 {
			delete(c.reactions, snowflake)
		}
		return true
	}
	if users[userID] {
		return false
	}
	users[userID] = true
	return true
}

// ReactionsSupported reports whether the upstream can anchor reactions:
// msgid-referencing needs message ids, advertised as MSGREFTYPES
// containing "msgid" in ISUPPORT (chathistory networks: eris fork, ergo,
// soju). Networks without it get no reaction picker in the client.
func (m *Manager) ReactionsSupported(userID, networkID string) bool {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	var client *girc.Client
	if ok {
		client = c.client
	}
	m.mu.Unlock()
	if client == nil {
		return false
	}
	refTypes, _ := client.GetServerOption("MSGREFTYPES")
	for _, t := range strings.Split(refTypes, ",") {
		if strings.TrimSpace(t) == "msgid" {
			return true
		}
	}
	return false
}

// resolveRef maps an IRC msgid to the Discord message identity, from the
// live registry first, then from storage (messages that predate the
// current connection - restarts wipe the in-memory maps).
func (m *Manager) resolveRef(c *conn, msgid string) (msgRef, bool) {
	if ref, ok := c.lookupRef(msgid); ok {
		return ref, true
	}
	channelID, messageID, ok := m.store.LookupMessageByMsgID(c.networkID, msgid)
	if !ok {
		return msgRef{}, false
	}
	ref := msgRef{Snowflake: messageID, ChannelID: channelID, GuildID: c.networkID}
	c.registerMsgid(ref, msgid)
	return ref, true
}

// applyReaction bridges an inbound IRC reaction (+draft/react on a TAGMSG
// with +reply) to MESSAGE_REACTION_ADD / _REMOVE.
func (m *Manager) applyReaction(c *conn, nick, msgid, emoji string, remove bool) {
	ref, ok := m.resolveRef(c, msgid)
	if !ok {
		m.log.Debug("react to unknown msgid", "user", c.userID, "msgid", msgid, "from", nick)
		return
	}
	userID := model.IrcAuthorID("irc:" + nick)
	if !c.trackReaction(ref.Snowflake, emoji, userID, remove) {
		return
	}
	m.persistReactions(c, ref)
	m.dispatchReaction(c, ref, userID, emoji, remove)
}

// dispatchReaction emits the gateway reaction event for all sessions.
func (m *Manager) dispatchReaction(c *conn, ref msgRef, userID, emoji string, remove bool) {
	payload := map[string]any{
		"user_id":    userID,
		"channel_id": ref.ChannelID,
		"message_id": ref.Snowflake,
		"emoji":      map[string]any{"id": nil, "name": emoji},
	}
	if ref.GuildID != "" {
		payload["guild_id"] = ref.GuildID
	}
	event := "MESSAGE_REACTION_ADD"
	if remove {
		event = "MESSAGE_REACTION_REMOVE"
	}
	m.gw.Dispatch(c.userID, event, payload)
}

// SendReaction relays a Discord reaction (REST PUT/DELETE @me) upstream as
// +draft/react / +draft/unreact TAGMSG with +reply, and mirrors it to the
// user's sessions. Unknown message ids (pre-restart messages, upstreams
// without msgid) still update locally so the reacting client is not left
// with a dead pill.
func (m *Manager) SendReaction(userID, networkID, target, messageID, channelID, emoji string, remove bool) error {
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
	msgid, _ := c.lookupMsgid(messageID)
	if msgid == "" && channelID != "" {
		msgid = m.store.MessageMsgID(channelID, messageID)
	}
	if client.HasCapability("message-tags") && msgid != "" {
		tag := "+draft/react"
		if remove {
			tag = "+draft/unreact"
		}
		client.Send(&girc.Event{
			Tags:   girc.Tags{"+reply": msgid, "+draft/reply": msgid, tag: emoji},
			Command: "TAGMSG",
			Params: []string{target},
		})
	}
	// Resolve the channel for the dispatch (REST knows only the target it
	// was given; the registry is authoritative).
	ref, ok := c.refBySnowflake(messageID)
	if !ok {
		if ch, err := m.store.GetChannelByIRC(networkID, target); err == nil {
			ref = msgRef{Snowflake: messageID, ChannelID: ch.ID, GuildID: networkID}
		} else if dm, err := m.store.EnsureDMChannel(userID, networkID, target, m.sf.New); err == nil {
			ref = msgRef{Snowflake: messageID, ChannelID: dm.ID}
		} else {
			ref = msgRef{Snowflake: messageID, ChannelID: target}
		}
	}
	if c.trackReaction(messageID, emoji, userID, remove) {
		m.persistReactions(c, ref)
		m.dispatchReaction(c, ref, userID, emoji, remove)
	}
	return nil
}
