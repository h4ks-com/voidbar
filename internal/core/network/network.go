// Package network implements the create-or-join of IRC networks from a
// connection string (Voidbar's invite), membership management and the glue
// that spawns per-user upstream connections and mirrors IRC state into
// Discord gateway events.
package network

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/irc/connstr"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

var ErrBadConnString = errors.New("invalid connection string")

// ParseConnString re-exports the connection-string parser so REST handlers
// can validate invite inputs without importing the irc package.
func ParseConnString(raw string) (*connstr.Conn, error) {
	return connstr.Parse(raw)
}

type Service struct {
	store   *storage.Storage
	gw      *gateway.Server
	sf      *util.Snowflake
	manager *ircmanage.Manager
}

func NewService(store *storage.Storage, gw *gateway.Server, sf *util.Snowflake, manager *ircmanage.Manager) *Service {
	return &Service{store: store, gw: gw, sf: sf, manager: manager}
}

// AppendBufferedMessage records a message into the channel's replay buffer
// (own sends; inbound relays write via the IRC manager directly).
func (s *Service) AppendBufferedMessage(m storage.BufferedMessage) error {
	if s.store == nil {
		return nil
	}
	return s.store.AppendMessage(m)
}

// ChannelMessages reads the channel replay buffer; see
// storage.Storage.ChannelMessages for ordering semantics.
func (s *Service) ChannelMessages(channelID, before, after string, limit int) []storage.BufferedMessage {
	if s.store == nil {
		return nil
	}
	return s.store.ChannelMessages(channelID, before, after, limit)
}

// UserSettings returns the persisted legacy client settings for a user.
func (s *Service) UserSettings(userID string) map[string]any {
	if s.store == nil {
		return map[string]any{}
	}
	return s.store.UserSettings(userID)
}

// MergeUserSettings persists a settings PATCH body for a user.
func (s *Service) MergeUserSettings(userID string, patch map[string]any) error {
	if s.store == nil {
		return nil
	}
	return s.store.MergeUserSettings(userID, patch)
}

// mergeChannels merges new IRC channel names into the member's auto-join
// list (case-insensitive dedup, original order kept).
func mergeChannels(existing, add []string) ([]string, bool) {
	seen := make(map[string]bool, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, ch := range existing {
		k := strings.ToLower(ch)
		if !seen[k] {
			seen[k] = true
			out = append(out, ch)
		}
	}
	changed := false
	for _, ch := range add {
		k := strings.ToLower(ch)
		if !seen[k] {
			seen[k] = true
			out = append(out, ch)
			changed = true
		}
	}
	return out, changed
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
	} else {
		// Re-joining the same network with new channels (a fresh connection
		// string for a network already joined) merges the channels into the
		// membership, like accepting another invite to the same server.
		if merged, changed := mergeChannels(mem.AutoJoin, conn.Channels); changed {
			mem.AutoJoin = merged
			if err := s.store.UpsertMembership(mem); err != nil {
				return nil, err
			}
			if s.manager != nil {
				// The upstream connection is already up: join the new IRC
				// channels on it right away.
				for _, ch := range conn.Channels {
					s.manager.JoinChannel(userID, net.ID, ch)
				}
			}
		}
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

// DMChannelByID resolves a snowflake id to a DM thread.
func (s *Service) DMChannelByID(id string) (*storage.DMChannel, error) {
	return s.store.GetDMChannel(id)
}

// DMChannelsFor lists a user's DM threads (newest activity first).
func (s *Service) DMChannelsFor(userID string) []*storage.DMChannel {
	dms, err := s.store.ListDMChannels(userID)
	if err != nil {
		return nil
	}
	return dms
}

// DMChannelPayloads shapes the user's DM threads for the client (READY
// private_channels and GET /users/@me/channels share the wire form).
func (s *Service) DMChannelPayloads(userID string) []any {
	dms := s.DMChannelsFor(userID)
	out := make([]any, 0, len(dms))
	for _, dm := range dms {
		out = append(out, map[string]any{
			"id":                          dm.ID,
			"type":                        1,
			"flags":                       0,
			"last_message_id":             nil,
			"last_message_timestamp":      nil,
			"recipients":                  []any{model.DMPeer(dm.Nick)},
			"is_message_request":           false,
			"is_message_request_timestamp": nil,
			"is_spam":                      false,
		})
	}
	return out
}

// EnsureDMChannel returns (creating if needed) the user's DM thread with a
// nick on a network.
func (s *Service) EnsureDMChannel(userID, netID, nick string) (*storage.DMChannel, error) {
	return s.store.EnsureDMChannel(userID, netID, nick, s.sf.New)
}

// TouchDMChannel bumps a DM thread's activity timestamp.
func (s *Service) TouchDMChannel(id string) {
	if s.store == nil {
		return
	}
	if err := s.store.TouchDMChannel(id); err != nil {
		// Ordering-only metadata: never fatal, the caller has no logger.
		_ = err
	}
}

// DeleteDMChannel closes a DM thread (client "close DM").
func (s *Service) DeleteDMChannel(id string) error {
	if s.store == nil {
		return nil
	}
	return s.store.DeleteDMChannel(id)
}

// NewMessageID mints a snowflake id for a message.
func (s *Service) NewMessageID() string { return s.sf.New() }

// ChannelsFor resolves (registers if needed) the network's IRC channels.
func (s *Service) ChannelsFor(netID string, ircNames []string) ([]*storage.Channel, error) {
	return s.store.ChannelsByIRC(netID, ircNames, s.sf.New)
}

// FindByChannelName resolves the fragment the Discord client extracts from
// a pasted connection string (e.g. "#vbtest2" out of
// irc://host:port/#vbtest2) back to the network the preview GET already
// joined, by matching it against the member's auto-join channels.
func (s *Service) FindByChannelName(userID, ircName string) (*storage.Network, *storage.Membership, error) {
	mems, err := s.store.ListMembershipsForUser(userID)
	if err != nil {
		return nil, nil, err
	}
	for _, mem := range mems {
		for _, ch := range mem.AutoJoin {
			if strings.EqualFold(ch, ircName) {
				net, err := s.store.GetNetwork(mem.NetworkID)
				if err != nil {
					continue
				}
				return net, mem, nil
			}
		}
	}
	return nil, nil, storage.ErrNotFound
}

// Leave removes the user's membership on a network — the Discord "leave
// guild" action. The upstream IRC connection is dropped; when the last
// membership on the network is gone the network itself (channels, replay
// buffers) is garbage-collected, so an accidental join leaves no residue.
func (s *Service) Leave(userID, guildID string) error {
	if _, err := s.store.GetMembership(guildID, userID); err != nil {
		return err
	}
	if err := s.store.DeleteMembership(guildID, userID); err != nil {
		return err
	}
	if s.manager != nil {
		s.manager.Drop(userID, guildID)
	}
	if members, err := s.store.ListMemberships(guildID); err == nil && len(members) == 0 {
		if err := s.store.DeleteNetworkCascade(guildID); err != nil {
			return fmt.Errorf("network gc: %w", err)
		}
	}
	return nil
}

// ValidateChannelName normalizes a client-supplied channel name to IRC form
// ("#name"). Returns "" when the name can't work as an IRC channel.
func ValidateChannelName(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.TrimPrefix(name, "#")
	name = strings.TrimPrefix(name, "&")
	if name == "" || len(name) > 50 {
		return ""
	}
	// RFC 1459 channel-name exclusions, plus the practical ones (spaces
	// break the wire protocol, commas break lists).
	if strings.ContainsAny(name, " ,\x07:*!@") {
		return ""
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			return ""
		}
	}
	return "#" + name
}

// CreateChannel is the client's "create channel" action: IRC servers create
// channels on JOIN (no reserved names), so the channel appears in the
// client immediately and the JOIN goes upstream; a refusal (invite-only,
// banned, account-required...) rolls back via the IRC manager's error
// handlers, with Clyde explaining in a DM.
func (s *Service) CreateChannel(userID, guildID, rawName string) (map[string]any, error) {
	if _, err := s.store.GetMembership(guildID, userID); err != nil {
		return nil, err
	}
	ircName := ValidateChannelName(rawName)
	if ircName == "" {
		return nil, ErrBadChannelName
	}
	if _, err := s.store.MembershipAddChannel(guildID, userID, ircName); err != nil {
		return nil, err
	}
	ch, err := s.store.EnsureChannel(guildID, ircName, s.sf.New)
	if err != nil {
		return nil, err
	}
	if s.manager != nil {
		s.manager.JoinChannel(userID, guildID, ircName)
	}
	payload := s.channelPayload(guildID, ch, 0)
	if s.gw != nil {
		s.gw.Dispatch(userID, "CHANNEL_CREATE", payload)
	}
	return payload, nil
}

// RemoveChannel is the client's channel delete: PART upstream, drop the
// auto-join entry, CHANNEL_DELETE to the client. The registry record and
// replay buffer are kept — re-adding the channel recovers its history,
// matching bouncer semantics.
func (s *Service) RemoveChannel(userID, channelID string) error {
	ch, err := s.store.GetChannel(channelID)
	if err != nil {
		return err
	}
	if _, err := s.store.GetMembership(ch.NetworkID, userID); err != nil {
		return err
	}
	if err := s.store.MembershipRemoveChannel(ch.NetworkID, userID, ch.IRCName); err != nil {
		return err
	}
	if s.manager != nil {
		s.manager.PartChannel(userID, ch.NetworkID, ch.IRCName)
	}
	if s.gw != nil {
		s.gw.Dispatch(userID, "CHANNEL_DELETE", map[string]any{
			"id":       channelID,
			"guild_id": ch.NetworkID,
		})
	}
	return nil
}

// channelPayload is the wire shape shared by guild assembly and
// CHANNEL_CREATE for an IRC-backed text channel.
func (s *Service) channelPayload(guildID string, ch *storage.Channel, position int) map[string]any {
	return map[string]any{
		"id":                    ch.ID,
		"guild_id":              guildID,
		"name":                  ch.Name,
		"type":                  0,
		"position":              position,
		"topic":                 nil,
		"last_message_id":       "0",
		"permission_overwrites": []any{},
		"rate_limit_per_user":   0,
		"nsfw":                  false,
		"flags":                 0,
		"parent_id":             nil,
	}
}

// ErrBadChannelName reports a client-supplied channel name that can't be an
// IRC channel.
var ErrBadChannelName = errors.New("invalid channel name")

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

	members := make([]any, 0, len(all)+1)
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
	// The owner is always Clyde: users only ever join networks (the
	// "Delete server" owner flow stays hidden, "Leave server" shows).
	members = append(members, model.ClydeMember(net.CreatedAt.Format(time.RFC3339)))

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
		"permissions":   "104324673682449", // everything incl. MANAGE_CHANNELS, owner-oriented bouncer
		"managed":       false,
		"mentionable":   false,
		"flags":         0,
	}

	return map[string]any{
		"id":                            net.ID,
		"name":                          net.Name,
		"icon":                          nil,
		"owner_id":                      model.ClydeID,
		"joined_at":                     m.JoinedAt.Format(time.RFC3339),
		"channels":                      channels,
		"members":                       members,
		"member_count":                  len(all) + 1,
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
