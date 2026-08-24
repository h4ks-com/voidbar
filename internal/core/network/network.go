// Package network implements the create-or-join of IRC networks from a
// connection string (Voidbar's invite), membership management and the glue
// that spawns per-user upstream connections and mirrors IRC state into
// Discord gateway events.
package network

import (
	"errors"
	"fmt"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/connstr"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

var ErrBadConnString = errors.New("invalid connection string")

type Service struct {
	store   *storage.Storage
	gw      *gateway.Server
	sf      *util.Snowflake
	manager *ircmanage.Manager
}

func NewService(store *storage.Storage, gw *gateway.Server, sf *util.Snowflake, manager *ircmanage.Manager) *Service {
	return &Service{store: store, gw: gw, sf: sf, manager: manager}
}

// Join parses a connection string (the invite), creates the network if it
// does not exist, records membership and spawns the user's own upstream
// connection. It is idempotent: joining the same connection string again
// returns the existing network.
func (s *Service) Join(userID, raw string) (*storage.Network, error) {
	conn, err := connstr.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadConnString, err)
	}

	connID := conn.ID()
	net, err := s.store.NetworkByConnID(connID)
	if errors.Is(err, storage.ErrNotFound) {
		net = &storage.Network{
			ID:        s.sf.New(),
			ConnID:    connID,
			Name:      conn.DisplayName(),
			Host:      conn.Host,
			Port:      conn.Port,
			TLS:       conn.TLS,
			Password:  conn.Password,
			CreatedBy: userID,
			CreatedAt: time.Now().UTC(),
		}
		if err := s.store.UpsertNetwork(net); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// Nickname is per-user; the connection string may not carry one, and even
	// if it does, each member keeps their own identity.
	nick := username(userID)
	if u, err := s.store.GetUserByID(userID); err == nil && u.Username != "" {
		nick = u.Username
	}

	mem, err := s.store.GetMembership(net.ID, userID)
	if errors.Is(err, storage.ErrNotFound) {
		mem = &storage.Membership{
			UserID:    userID,
			NetworkID: net.ID,
			Nick:      nick,
			Username:  nick,
			Realname:  nick,
			AutoJoin:  conn.Channels,
			JoinedAt:  time.Now().UTC(),
		}
		if err := s.store.UpsertMembership(mem); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	if s.manager != nil {
		s.manager.EnsureConn(userID, net.ID)
	}
	return net, nil
}

// Network returns the network record by id.
func (s *Service) Network(id string) (*storage.Network, error) {
	return s.store.GetNetwork(id)
}

// MembershipFor returns the user's membership on a network.
func (s *Service) MembershipFor(userID, netID string) (*storage.Membership, error) {
	return s.store.GetMembership(netID, userID)
}

// ChannelByID resolves a snowflake channel id to its registry record.
func (s *Service) ChannelByID(id string) (*storage.Channel, error) {
	return s.store.GetChannel(id)
}

// NewMessageID mints a snowflake id for a message.
func (s *Service) NewMessageID() string { return s.sf.New() }

// ChannelsFor resolves (registers if needed) the network's IRC channels.
func (s *Service) ChannelsFor(netID string, ircNames []string) ([]*storage.Channel, error) {
	return s.store.ChannelsByIRC(netID, ircNames, s.sf.New)
}

// GuildCreatePayloads returns full GUILD_CREATE payloads for every network
// the user belongs to, dispatched by the gateway right after READY. Channels
// come from the member's auto-join list (live IRC channel state follows in
// later phases); members are the network's memberships with synthesized
// Discord-shaped users.
func (s *Service) GuildCreatePayloads(userID string) []any {
	memberships, err := s.store.ListMembershipsForUser(userID)
	if err != nil {
		return nil
	}
	var out []any
	for _, m := range memberships {
		net, err := s.store.GetNetwork(m.NetworkID)
		if err != nil {
			continue
		}
		out = append(out, s.buildGuild(m, net))
	}
	return out
}

// ReadyGuildPayloads returns the READY guild entries for every network the
// user belongs to. Per the Gateway Guild Object spec the wire format carries
// channels as a flat array - the client's hydrateReadyPayloadPrioritized
// wraps them into its internal {channels, wasCached} structure itself.
func (s *Service) ReadyGuildPayloads(userID string) []any {
	return s.GuildCreatePayloads(userID)
}

func (s *Service) buildGuild(m *storage.Membership, net *storage.Network) any {
	all, err := s.store.ListMemberships(net.ID)
	if err != nil {
		all = nil
	}

	// Channels carry URL-safe snowflake ids resolved through the channel
	// registry (the IRC name itself would break client routing).
	chans, err := s.store.ChannelsByIRC(net.ID, m.AutoJoin, s.sf.New)
	if err != nil {
		chans = nil
	}
	channels := make([]any, 0, len(chans))
	for i, ch := range chans {
		channels = append(channels, map[string]any{
			"id":                    ch.ID,
			"guild_id":              net.ID,
			"name":                  ch.Name,
			"type":                  0,
			"position":              i,
			"topic":                 nil,
			"last_message_id":       "0",
			"permission_overwrites": []any{},
			"rate_limit_per_user":   0,
			"nsfw":                  false,
			"flags":                 0,
			"parent_id":             nil,
		})
	}

	members := make([]any, 0, len(all))
	for _, mem := range all {
		members = append(members, map[string]any{
			"user": map[string]any{
				"id":            mem.UserID,
				"username":      mem.Nick,
				"discriminator": "0",
				"bot":           false,
			},
			"roles":     []any{},
			"joined_at": mem.JoinedAt.Format(time.RFC3339),
		})
	}

	// Every Discord guild has an @everyone role whose id equals the guild id;
	// the client computes channel permissions through it, and without the
	// role channels appear inaccessible.
	everyoneRole := map[string]any{
		"id":            net.ID,
		"name":          "@everyone",
		"color":         0,
		"hoist":         false,
		"icon":          nil,
		"unicode_emoji": nil,
		"position":      0,
		"permissions":   "104324673682433", // everything, owner-oriented bouncer
		"managed":       false,
		"mentionable":   false,
		"flags":         0,
	}

	return map[string]any{
		"id":                            net.ID,
		"name":                          net.Name,
		"icon":                          nil,
		"owner_id":                      net.CreatedBy,
		"joined_at":                     m.JoinedAt.Format(time.RFC3339),
		"channels":                      channels,
		"members":                       members,
		"member_count":                  len(all),
		"large":                         false,
		"roles":                         []any{everyoneRole},
		"presences":                     []any{},
		"voice_states":                  []any{},
		"threads":                       []any{},
		"emojis":                        []any{},
		"stickers":                      []any{},
		"stage_instances":               []any{},
		"guild_scheduled_events":        []any{},
		"embedded_activities":           []any{},
		"features":                      []any{},
		"premium_tier":                  0,
		"nsfw":                          false,
		"verification_level":            0,
		"default_message_notifications": 0,
		"explicit_content_filter":       0,
		"mfa_level":                     0,
		"system_channel_id":             nil,
		"rules_channel_id":              nil,
		"public_updates_channel_id":     nil,
		"afk_channel_id":                nil,
		"afk_timeout":                   300,
		"preferred_locale":              "en-US",
		"guild_hashes": map[string]any{
			"version":  0,
			"hashes":   map[string]any{},
			"guild_id": net.ID,
		},
		"unavailable": false,
	}
}

// GuildCreateForUser is the gateway hook for the above.
func (s *Service) GuildCreateForUser(userID string) []any {
	return s.GuildCreatePayloads(userID)
}

func username(userID string) string {
	// Snowflakes are too long for nicks; fall back to a stable short form.
	h := util.SHA256Hex(userID)
	return "vb_" + h[:9]
}

// GuildsForUser assembles the Discord wire representation of every network
// the user belongs to, for READY.
func (s *Service) GuildsForUser(userID string) ([]any, error) {
	memberships, err := s.store.ListMembershipsForUser(userID)
	if err != nil {
		return nil, err
	}
	guilds := make([]any, 0, len(memberships))
	for _, m := range memberships {
		net, err := s.store.GetNetwork(m.NetworkID)
		if err != nil {
			continue
		}
		guilds = append(guilds, map[string]any{
			"id":          net.ID,
			"name":        net.Name,
			"unavailable": false,
			"joined_at":   m.JoinedAt.Format(time.RFC3339),
		})
	}
	return guilds, nil
}
