// Package network implements the create-or-join of IRC networks from a
// connection string (Voidbar's invite), membership management and the glue
// that spawns per-user upstream connections and mirrors IRC state into
// Discord gateway events.
package network

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	log     *slog.Logger
}

func NewService(store *storage.Storage, gw *gateway.Server, sf *util.Snowflake, manager *ircmanage.Manager, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, gw: gw, sf: sf, manager: manager, log: log}
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

// CreateUpload records a pending attachment upload slot.
func (s *Service) CreateUpload(up *storage.PendingUpload) error {
	if s.store == nil {
		return nil
	}
	return s.store.CreateUpload(up)
}

// GetUpload fetches a pending attachment upload slot.
func (s *Service) GetUpload(token string) (*storage.PendingUpload, error) {
	if s.store == nil {
		return nil, storage.ErrAttachmentNotFound
	}
	return s.store.GetUpload(token)
}

// BindAttachment stores uploaded bytes and binds them to their id.
func (s *Service) BindAttachment(upload *storage.PendingUpload, att *storage.Attachment, data []byte) error {
	if s.store == nil {
		return nil
	}
	return s.store.BindAttachment(upload, att, data)
}

// PutAttachment stores attachment metadata and bytes (multipart flow).
func (s *Service) PutAttachment(att *storage.Attachment, data []byte) error {
	if s.store == nil {
		return nil
	}
	return s.store.PutAttachment(att, data)
}

// BindUpload ties an upload_filename to a stored attachment id.
func (s *Service) BindUpload(uploadFilename, attachmentID string) error {
	if s.store == nil {
		return nil
	}
	return s.store.BindUpload(uploadFilename, attachmentID)
}

// ResolveUpload maps an upload_filename to the stored attachment.
func (s *Service) ResolveUpload(uploadFilename string) (*storage.Attachment, error) {
	if s.store == nil {
		return nil, storage.ErrAttachmentNotFound
	}
	return s.store.ResolveUpload(uploadFilename)
}

// GetAttachment returns attachment metadata by id.
func (s *Service) GetAttachment(id string) (*storage.Attachment, error) {
	if s.store == nil {
		return nil, storage.ErrAttachmentNotFound
	}
	return s.store.GetAttachment(id)
}

// GetAttachmentData returns attachment bytes by id.
func (s *Service) GetAttachmentData(id string) ([]byte, error) {
	if s.store == nil {
		return nil, storage.ErrAttachmentNotFound
	}
	return s.store.GetAttachmentData(id)
}

// UserSettings returns the persisted legacy client settings for a user,
// merged over the defaults so clients validating the full shape (web
// clients with zod schemas) always see a complete object.
func (s *Service) UserSettings(userID string) map[string]any {
	if s.store == nil {
		return model.SettingsWithDefaults(nil)
	}
	return model.SettingsWithDefaults(s.store.UserSettings(userID))
}

// MergeUserSettings persists a settings PATCH body for a user.
func (s *Service) MergeUserSettings(userID string, patch map[string]any) error {
	if s.store == nil {
		return nil
	}
	return s.store.MergeUserSettings(userID, patch)
}

// UserSettingsProto returns the persisted serialized settings protobuf for
// the kind (nil when never written).
func (s *Service) UserSettingsProto(userID, kind string) []byte {
	if s.store == nil {
		return nil
	}
	return s.store.UserSettingsProto(userID, kind)
}

// SetUserSettingsProto stores the merged serialized settings protobuf for
// the kind.
func (s *Service) SetUserSettingsProto(userID, kind string, blob []byte) error {
	if s.store == nil {
		return nil
	}
	return s.store.SetUserSettingsProto(userID, kind, blob)
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
			ID:          s.sf.New(),
			ConnID:      connID,
			Name:        conn.DisplayName(),
			Host:        conn.Host,
			Port:        conn.Port,
			TLS:         conn.TLS,
			Password:    conn.Password,
			SASLUser:    conn.SASLUser,
			SASLPass:    conn.SASLPass,
			ChannelKeys: conn.ChannelKeys,
			CreatedBy:   userID,
			CreatedAt:   time.Now().UTC(),
		}
		if err := s.store.UpsertNetwork(net); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		// Metadata re-join of a known network: keys and SASL credentials
		// are network-wide, so a fresh string can add or rotate them.
		changed := false
		if len(conn.ChannelKeys) > 0 {
			if net.ChannelKeys == nil {
				net.ChannelKeys = map[string]string{}
			}
			for ch, key := range conn.ChannelKeys {
				if net.ChannelKeys[ch] != key {
					net.ChannelKeys[ch] = key
					changed = true
				}
			}
		}
		if conn.SASLUser != "" && (conn.SASLUser != net.SASLUser || conn.SASLPass != net.SASLPass) {
			net.SASLUser, net.SASLPass = conn.SASLUser, conn.SASLPass
			changed = true
		}
		if changed {
			if err := s.store.UpsertNetwork(net); err != nil {
				return nil, err
			}
		}
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

// FetchOlderMessages extends a channel's history downwards when a
// scroll-up page comes up short: asks the upstream network (via
// draft/chathistory BEFORE, bridged by the IRC manager) for messages
// older than the deepest anchor we know - the buffer floor, or the
// client's ?before cursor when the buffer holds nothing below it. Each
// round inserts silently and re-anchors at the new floor until the page
// is satisfied or the network runs dry. Returns how many messages were
// inserted; 0 means the page stands as-is (no cap, link down, or the
// network has nothing older).
func (s *Service) FetchOlderMessages(userID, channelID, before string, need int) int {
	if s.manager == nil || s.store == nil || need <= 0 {
		return 0
	}
	id, err := util.ParseSnowflake(before)
	if err != nil {
		return 0
	}
	cursor := util.SnowflakeTime(id)
	ch, err := s.ChannelByID(channelID)
	if err != nil || !strings.HasPrefix(ch.IRCName, "#") && !strings.HasPrefix(ch.IRCName, "&") {
		return 0
	}
	total := 0
	// A full page is <=100; each round needs a strictly older anchor, so
	// a handful of rounds can never loop: a dry network returns 0.
	for round := 0; round < 3 && need > 0; round++ {
		anchorMsgID := ""
		ceilingID := ""
		anchor := cursor
		if all := s.store.ChannelMessages(ch.ID, "", "", storage.MsgBufferCap); len(all) > 0 {
			floor := all[len(all)-1]
			anchorMsgID = floor.MsgID
			ceilingID = floor.ID
			if ft, err := time.Parse(time.RFC3339, floor.Timestamp); err == nil && ft.Before(anchor) {
				anchor = ft
			}
		}
		got := s.manager.FetchOlder(userID, ch.NetworkID, ch.IRCName, anchorMsgID, ceilingID, anchor, need)
		total += got
		if got == 0 {
			break
		}
		need -= got
	}
	return total
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

// dmPeerFor renders the DM peer so its id matches what the client
// already saw: a fellow bouncer member is addressed by their real user
// id everywhere (guild payloads, member rows), while plain IRC nicks use
// the IrcAuthorID hash. Matching is by live nick first, stored nick
// second (the server may have renamed them).
func (s *Service) dmPeerFor(netID, nick string) map[string]any {
	others, err := s.store.ListMemberships(netID)
	if err == nil {
		for _, o := range others {
			n := o.Nick
			if s.manager != nil {
				if live := s.manager.LiveNick(o.UserID, netID); live != "" {
					n = live
				}
			}
			if strings.EqualFold(n, nick) {
				return model.DMPeerID(nick, o.UserID)
			}
		}
	}
	return model.DMPeer(nick)
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
			"recipients":                  []any{s.dmPeerFor(dm.NetworkID, dm.Nick)},
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

// ErrUnknownRecipient reports a DM recipient the bouncer can't resolve to
// an IRC nick on any of the user's networks.
var ErrUnknownRecipient = errors.New("unknown recipient")

// CreateDMChannel answers POST /users/@me/channels: resolve the recipient
// id back to an IRC nick on one of the user's networks and open (or
// return) the DM thread. The ids the client can pick came from payloads we
// built - synthetic IRC-author snowflakes from the member sidebar or a
// fellow bouncer member's user id - so the reverse mapping only needs the
// live NAMES state plus the network's membership list.
func (s *Service) CreateDMChannel(userID, recipientID string) (map[string]any, error) {
	// Clyde is one global peer: the sidebar owner entry and every
	// clydeSay notice share a single thread on the pseudo-network.
	if recipientID == model.ClydeID {
		return s.dmPayloadFor(userID, model.ClydeNetID, "Clyde")
	}
	memberships, err := s.store.ListMembershipsForUser(userID)
	if err != nil {
		return nil, err
	}
	for _, m := range memberships {
		// Fellow bouncer members show up with their real user id; their
		// DM peer nick is whatever they hold on the wire right now.
		others, err := s.store.ListMemberships(m.NetworkID)
		if err == nil {
			for _, o := range others {
				if o.UserID != recipientID || o.UserID == userID {
					continue
				}
				nick := o.Nick
				if s.manager != nil {
					if live := s.manager.LiveNick(o.UserID, m.NetworkID); live != "" {
						nick = live
					}
				}
				return s.dmPayloadFor(userID, m.NetworkID, nick)
			}
		}
		// IRC occupants: their payload ids are IrcAuthorID("irc:"+nick).
		for _, cm := range s.ircOccupants(userID, m.NetworkID, m.AutoJoin) {
			if model.IrcAuthorID("irc:"+cm.Nick) == recipientID {
				return s.dmPayloadFor(userID, m.NetworkID, cm.Nick)
			}
		}
	}
	return nil, ErrUnknownRecipient
}

// dmPayloadFor opens the DM thread and shapes it like DMChannelPayloads.
func (s *Service) dmPayloadFor(userID, netID, nick string) (map[string]any, error) {
	dm, err := s.EnsureDMChannel(userID, netID, nick)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":                          dm.ID,
		"type":                        1,
		"flags":                       0,
		"last_message_id":             nil,
		"last_message_timestamp":      nil,
		"recipients":                  []any{s.dmPeerFor(netID, dm.Nick)},
		"is_message_request":           false,
		"is_message_request_timestamp": nil,
		"is_spam":                     false,
	}, nil
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
	// Inline +k key: "name:key" (first colon splits - names never carry
	// one, keys may). The key lands on the network record before the
	// JOIN goes out, so the keyed join finds it.
	key := ""
	if i := strings.IndexByte(rawName, ':'); i >= 0 {
		rawName, key = rawName[:i], rawName[i+1:]
	}
	ircName := ValidateChannelName(rawName)
	if ircName == "" {
		return nil, ErrBadChannelName
	}
	if key != "" {
		net, err := s.store.GetNetwork(guildID)
		if err != nil {
			return nil, err
		}
		if net.ChannelKeys == nil {
			net.ChannelKeys = map[string]string{}
		}
		net.ChannelKeys[strings.ToLower(ircName)] = key
		if err := s.store.UpsertNetwork(net); err != nil {
			return nil, err
		}
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
		"topic":                 ircmanage.TopicValue(ch.Topic),
		"last_message_id":       "0",
		"permission_overwrites": []any{},
		"rate_limit_per_user":   0,
		"nsfw":                  false,
		"flags":                 0,
		"parent_id":             nil,
		// COMPAT: the client resolves the member sidebar through the
		// channel's own member_list_id (Channel.memberListId) — the lazy
		// list map is keyed by exactly this string, and GUILD_MEMBER_LIST_
		// UPDATE ids must match it or the rows render as shimmer
		// placeholders forever. Per-channel, so lists don't bleed between
		// channels.
		"member_list_id": model.MemberListID(guildID, ch.ID),
	}
}

// MemberListPayload answers op 14 (lazy request) with a
// GUILD_MEMBER_LIST_UPDATE SYNC for one channel (or the guild-wide
// everyone-list when channelID is empty). Members come from the upstream
// NAMES state; presence is online for everyone (IRC has no per-user
// offline: you see exactly who is in the channel). Occupants with a
// channel-membership prefix land in their own hoisted role sections
// (founder/admin/operator/half-op/voice), plain members follow under
// "online".
func (s *Service) MemberListPayload(userID, guildID, channelID string) any {
	mem, err := s.store.GetMembership(guildID, userID)
	if err != nil {
		return nil
	}
	// Dead upstream: the guild is flagged unavailable on the Discord side;
	// answering member asks with empty sets just makes clients re-request
	// forever against state they know used to be richer.
	if !s.linkUp(userID, guildID) {
		return nil
	}
	ircName := ""
	if channelID != "" {
		ch, err := s.store.GetChannel(channelID)
		if err != nil {
			return nil
		}
		ircName = ch.IRCName
	}
	var list []ircmanage.ChannelMember
	if ircName != "" {
		if s.manager != nil {
			list = s.manager.ChannelMembersDetailed(userID, guildID, ircName)
		}
	} else {
		// Guild-wide list: union across the member's channels.
		list = s.ircOccupants(userID, guildID, mem.AutoJoin)
	}
	// Bouncer members ride under their REAL user ids (the GUILD_CREATE
	// member rows use them too - a hash id here would render the same
	// person twice in the client's mention autocomplete); their roster
	// occupancy rows are skipped to match. This also keeps the list
	// honest with the link down: members stay even when NAMES is empty.
	bouncers := s.bouncerMembers(guildID)
	modeByLower := make(map[string]string, len(list))
	for _, cm := range list {
		modeByLower[strings.ToLower(cm.Nick)] = cm.Mode
	}
	awayByLower := make(map[string]bool, len(list))
	for _, cm := range list {
		awayByLower[strings.ToLower(cm.Nick)] = cm.Away
	}
	rows := make([]ircmanage.ChannelMember, 0, len(list)+len(bouncers))
	ids := make([]string, 0, cap(rows))
	for _, bm := range bouncers {
		lower := strings.ToLower(bm.nick)
		rows = append(rows, ircmanage.ChannelMember{
			Nick: bm.nick,
			Mode: modeByLower[lower],
			Away: awayByLower[lower],
		})
		ids = append(ids, bm.userID)
	}
	for _, cm := range list {
		if _, dup := bouncers[strings.ToLower(cm.Nick)]; dup {
			continue
		}
		rows = append(rows, cm)
		ids = append(ids, "")
	}
	if len(rows) > 99 {
		rows = rows[:99]
		ids = ids[:99]
	}

	joinedAt := mem.JoinedAt.Format(time.RFC3339)
	byMode := make(map[string]int, len(rows))
	for _, cm := range rows {
		byMode[cm.Mode]++
	}
	groups := make([]any, 0, len(model.IrcRoleModes())+1)
	items := make([]any, 0, len(rows)+1)
	for _, mode := range model.IrcRoleModes() {
		if byMode[mode] == 0 {
			continue
		}
		roleID := model.IrcRoleID(mode)
		groups = append(groups, map[string]any{"id": roleID, "count": byMode[mode]})
		items = append(items, map[string]any{"group": map[string]any{"id": roleID, "count": byMode[mode]}})
		for i, cm := range rows {
			if cm.Mode != mode {
				continue
			}
			items = append(items, memberListItem(cm, ids[i], joinedAt, mode))
		}
	}
	if byMode[""] > 0 {
		// Empty sections must not be sent at all: the client renders
		// whatever groups we emit, including "Online - 0" headers.
		groups = append(groups, map[string]any{"id": "online", "count": byMode[""]})
		items = append(items, map[string]any{"group": map[string]any{"id": "online", "count": byMode[""]}})
		for i, cm := range rows {
			if cm.Mode != "" {
				continue
			}
			items = append(items, memberListItem(cm, ids[i], joinedAt, ""))
		}
	}
	return map[string]any{
		"id":       model.MemberListID(guildID, channelID),
		"guild_id": guildID,
		"groups":   groups,
		// Discord always sends member_count; online_count is the
		// Spacebar extra web clients use for partial-sync heuristics.
		// On IRC everyone listed is connected by definition.
		"member_count": len(rows),
		"online_count": len(rows),
		"ops": []any{
			map[string]any{
				"op":    "SYNC",
				"range": []int{0, 99},
				"items": items,
			},
		},
	}
}

// bouncerMember is one bouncer member of a network under their live
// nick, with their real user id.
type bouncerMember struct {
	nick   string
	userID string
}

// bouncerMembers lists the network's bouncer members under their live
// nicks, keyed by lowercased nick.
func (s *Service) bouncerMembers(networkID string) map[string]bouncerMember {
	all, err := s.store.ListMemberships(networkID)
	if err != nil {
		return nil
	}
	out := make(map[string]bouncerMember, len(all))
	for _, mem := range all {
		nick := mem.Nick
		if s.manager != nil {
			if live := s.manager.LiveNick(mem.UserID, networkID); live != "" {
				nick = live
			}
		}
		if nick == "" {
			continue
		}
		out[strings.ToLower(nick)] = bouncerMember{nick: nick, userID: mem.UserID}
	}
	return out
}

// memberListItem builds one GUILD_MEMBER_LIST_UPDATE member row. uid is
// the Discord user id for the row: a bouncer member's real id, or the
// hashed snowflake for a plain IRC peer. COMPAT: presence lives INSIDE
// the member object here (the item parser only knows the "group"/
// "member" keys, and GuildMember itself carries a presence field the
// client reads via StoreStream.handleItem). IRC away maps to Discord
// "idle".
func memberListItem(cm ircmanage.ChannelMember, uid, joinedAt, mode string) map[string]any {
	if uid == "" {
		uid = model.IrcAuthorID("irc:" + cm.Nick)
	}
	status := presenceStatus(cm.Away)
	return map[string]any{
		"member": map[string]any{
			// Discord's op14 member carries the user id at both levels;
			// strict web-client schemas require member.id (and the member
			// list React keys derive from it).
			"id": uid,
			"user": map[string]any{
				"id":            uid,
				"username":      cm.Nick,
				"discriminator": "0",
				"bot":           false,
			},
			"roles":     ircRoleIDsFor(mode),
			"joined_at": joinedAt,
			"presence": map[string]any{
				"user":   map[string]any{"id": uid},
				"status": status,
			},
		},
	}
}

// presenceStatus maps IRC presence to Discord wire statuses: IRC has two
// visible states (here, away) and Discord's closest to away is "idle".
func presenceStatus(away bool) string {
	if away {
		return "idle"
	}
	return "online"
}

// ircRoleIDsFor wraps a membership mode's role id for a member payload
// (empty slice for plain members).
func ircRoleIDsFor(mode string) []any {
	if model.IrcModeRank(mode) == 0 {
		return []any{}
	}
	return []any{model.IrcRoleID(mode)}
}

// ircOccupants returns the union of channel occupants across ircNames,
// each with their highest mode across those channels (away if away in
// any - IRC away is per-connection, so it's the same everywhere), sorted
// by nick.
func (s *Service) ircOccupants(userID, networkID string, ircNames []string) []ircmanage.ChannelMember {
	if s.manager == nil {
		return nil
	}
	best := map[string]string{}
	away := map[string]bool{}
	seen := map[string]bool{}
	var nicks []string
	for _, chName := range ircNames {
		for _, cm := range s.manager.ChannelMembersDetailed(userID, networkID, chName) {
			if !seen[cm.Nick] {
				seen[cm.Nick] = true
				nicks = append(nicks, cm.Nick)
			}
			if model.IrcModeRank(cm.Mode) > model.IrcModeRank(best[cm.Nick]) {
				best[cm.Nick] = cm.Mode
			}
			if cm.Away {
				away[cm.Nick] = true
			}
		}
	}
	sort.Slice(nicks, func(i, j int) bool {
		return strings.ToLower(nicks[i]) < strings.ToLower(nicks[j])
	})
	out := make([]ircmanage.ChannelMember, 0, len(nicks))
	for _, n := range nicks {
		out = append(out, ircmanage.ChannelMember{Nick: n, Mode: best[n], Away: away[n]})
	}
	return out
}

// RefreshOccupancy is the ircmanage occupancy callback: an upstream
// membership event changed who sits in ircChannel (empty = everything
// changed, i.e. QUIT/NICK). Every member list the user's sessions
// actually subscribed to (op 14) and that the event touches gets a
// fresh GUILD_MEMBER_LIST_UPDATE SYNC - a full replace, so no INSERT/
// DELETE index arithmetic against the client's flattened rows.
func (s *Service) RefreshOccupancy(userID, guildID, ircChannel string) {
	if s.gw == nil {
		return
	}
	specs := s.gw.RequestedMemberLists(userID)
	s.log.Info("occupancy refresh", "user", userID, "guild", guildID, "channel", ircChannel, "subscribed", len(specs))
	for _, spec := range specs {
		if spec.GuildID != guildID {
			continue
		}
		if ircChannel != "" && spec.ChannelID != "" {
			ch, err := s.store.GetChannel(spec.ChannelID)
			if err != nil || !strings.EqualFold(ch.IRCName, ircChannel) {
				continue
			}
		}
		if payload := s.MemberListPayload(userID, spec.GuildID, spec.ChannelID); payload != nil {
			s.gw.Dispatch(userID, "GUILD_MEMBER_LIST_UPDATE", payload)
		}
	}
}

// RefreshMember is the ircmanage member callback: a bouncer member's own
// nick changed upstream (client relay, ghost reclaim, collision rename).
// GUILD_MEMBER_UPDATE carries the new nick - the client's member rows and
// the nickname dialog then match IRC reality. Roles ride along so a
// prefixed member keeps its sidebar section and name color.
func (s *Service) RefreshMember(userID, guildID, nick string) {
	if s.gw == nil {
		return
	}
	payload := s.MemberPayload(userID, guildID, nick)
	if payload == nil {
		return
	}
	s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", payload)
}

// MemberPayload builds one bouncer member's guild-member object under the
// new nick, with the roles their highest channel mode grants. nil when the
// user isn't a member of that network.
func (s *Service) MemberPayload(userID, guildID, nick string) map[string]any {
	mem, err := s.store.GetMembership(guildID, userID)
	if err != nil {
		return nil
	}
	mode := ""
	for _, cm := range s.ircOccupants(userID, guildID, mem.AutoJoin) {
		if strings.EqualFold(cm.Nick, nick) {
			mode = cm.Mode
			break
		}
	}
	return map[string]any{
		"guild_id": guildID,
		"user": map[string]any{
			"id":            mem.UserID,
			"username":      nick,
			"discriminator": "0",
			"bot":           false,
		},
		"nick":      nick,
		"roles":     ircRoleIDsFor(mode),
		"joined_at": mem.JoinedAt.Format(time.RFC3339),
	}
}

// StartTyping relays a client typing indicator (POST /channels/{id}/typing)
// upstream as a draft/typing TAGMSG, for both guild channels and DMs.
func (s *Service) StartTyping(userID, channelID string) error {
	if s.manager == nil {
		return nil
	}
	if ch, err := s.store.GetChannel(channelID); err == nil {
		return s.manager.SendTyping(userID, ch.NetworkID, ch.IRCName)
	}
	if dm, err := s.store.GetDMChannel(channelID); err == nil {
		return s.manager.SendTyping(userID, dm.NetworkID, dm.Nick)
	}
	return ErrUnknownRecipient
}

// MemberChunkPayload answers op 8 (Request Guild Members) with a single
// GUILD_MEMBERS_CHUNK (userdocs shape). With userIDs set, only those
// members are returned (the client resolves message authors it has seen);
// without, the union of occupants across the member's channels.
func (s *Service) MemberChunkPayload(userID, guildID, nonce string, userIDs []string) any {
	mem, err := s.store.GetMembership(guildID, userID)
	if err != nil {
		return nil
	}
	// See MemberListPayload: no member answers for dead upstreams.
	if !s.linkUp(userID, guildID) {
		return nil
	}
	want := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		want[id] = true
	}
	// Same id discipline as the op 14 lists: bouncer members under their
	// real ids (the own id resolves here too - with a hash it never
	// matched and the client kept re-asking), peers under their hashes.
	bouncers := s.bouncerMembers(guildID)
	list := s.ircOccupants(userID, guildID, mem.AutoJoin)
	modeByLower := make(map[string]string, len(list))
	awayByLower := make(map[string]bool, len(list))
	for _, cm := range list {
		modeByLower[strings.ToLower(cm.Nick)] = cm.Mode
		awayByLower[strings.ToLower(cm.Nick)] = cm.Away
	}
	type row struct {
		cm  ircmanage.ChannelMember
		uid string
	}
	rows := make([]row, 0, len(list)+len(bouncers))
	for _, bm := range bouncers {
		lower := strings.ToLower(bm.nick)
		rows = append(rows, row{ircmanage.ChannelMember{
			Nick: bm.nick,
			Mode: modeByLower[lower],
			Away: awayByLower[lower],
		}, bm.userID})
	}
	for _, cm := range list {
		if _, dup := bouncers[strings.ToLower(cm.Nick)]; dup {
			continue
		}
		rows = append(rows, row{cm, ""})
	}
	members := make([]any, 0, len(rows))
	presences := make([]any, 0, len(rows))
	for _, r := range rows {
		uid := r.uid
		if uid == "" {
			uid = model.IrcAuthorID("irc:" + r.cm.Nick)
		}
		if len(want) > 0 && !want[uid] {
			continue
		}
		user := map[string]any{
			"id":            uid,
			"username":      r.cm.Nick,
			"discriminator": "0",
			"bot":           false,
		}
		members = append(members, map[string]any{
			"user":      user,
			"roles":     ircRoleIDsFor(r.cm.Mode),
			"joined_at": mem.JoinedAt.Format(time.RFC3339),
		})
		status := presenceStatus(r.cm.Away)
		presences = append(presences, map[string]any{
			"user":       map[string]any{"id": uid},
			"status":     status,
			"activities": []any{},
		})
	}
	if len(members) == 0 && len(want) > 0 {
		// Requested users are not in any channel right now: per the docs
		// they'd come back in not_found so the client stops asking.
		members = []any{}
		presences = []any{}
	}
	out := map[string]any{
		"guild_id":    guildID,
		"members":     members,
		"chunk_index": 0,
		"chunk_count": 1,
	}
	if len(presences) > 0 {
		out["presences"] = presences
	}
	if nonce != "" {
		out["nonce"] = nonce
	}
	return out
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
		channels = append(channels, s.channelPayload(net.ID, ch, i))
	}

	// IRC occupants ride along as guild members. The client only lazy-loads
	// the member sidebar (op 14 lazy list) for "large" guilds; with
	// large:false it renders straight from GUILD_CREATE's members/presences,
	// so the NAMES state has to be inlined here too. Occupants carry their
	// highest channel-membership mode as a synthetic role (name color +
	// hoisted sidebar section).
	occupants := s.ircOccupants(m.UserID, net.ID, m.AutoJoin)
	modeByLower := make(map[string]string, len(occupants))
	for _, cm := range occupants {
		modeByLower[strings.ToLower(cm.Nick)] = cm.Mode
	}
	// A member's visible nick is their LIVE upstream nick: the server may
	// rename on collision (stored "doesnm" connects as "doesnm_"), and the
	// stored nick can belong to a different human - matching modes by the
	// stored nick would hand the member that human's prefixes (and a
	// duplicate row for the live nick).
	nickFor := func(mem *storage.Membership) string {
		if s.manager != nil {
			if live := s.manager.LiveNick(mem.UserID, net.ID); live != "" {
				return live
			}
		}
		return mem.Nick
	}
	memberNicks := make(map[string]bool, len(all))
	for _, mem := range all {
		memberNicks[strings.ToLower(nickFor(mem))] = true
	}

	members := make([]any, 0, len(all)+1)
	presences := make([]any, 0)
	for _, mem := range all {
		nick := nickFor(mem)
		members = append(members, map[string]any{
			"user": map[string]any{
				"id":            mem.UserID,
				"username":      nick,
				"discriminator": "0",
				"bot":           false,
			},
			// The IRC nick rides as the guild nickname too: the client's
			// Change-Nickname dialog prefills from member.nick, and it is
			// exactly what the field means here - the name on this guild.
			"nick":      nick,
			"roles":     ircRoleIDsFor(modeByLower[strings.ToLower(nick)]),
			"joined_at": mem.JoinedAt.Format(time.RFC3339),
		})
	}
	// The owner is always Clyde: users only ever join networks (the
	// "Delete server" owner flow stays hidden, "Leave server" shows).
	members = append(members, model.ClydeMember(net.CreatedAt.Format(time.RFC3339)))

	// Bouncer members get real presences: the client's bottom panel and
	// settings sheet render the OWN user from presences/PRESENCE_UPDATE,
	// not from the member list - skipping member rows here left the panel
	// stuck on the boot default. Members without a live connection carry
	// no presence (renders offline).
	if s.manager != nil {
		for _, mem := range all {
			if st, ok := s.manager.SelfPresence(mem.UserID, net.ID); ok {
				presences = append(presences, map[string]any{
					"user":   map[string]any{"id": mem.UserID},
					"status": st,
				})
			}
		}
	}

	for _, cm := range occupants {
		// Occupants that ARE a member's live nick were emitted above.
		if memberNicks[strings.ToLower(cm.Nick)] {
			continue
		}
		uid := model.IrcAuthorID("irc:" + cm.Nick)
		members = append(members, map[string]any{
			"user": map[string]any{
				"id":            uid,
				"username":      cm.Nick,
				"discriminator": "0",
				"bot":           false,
			},
			"roles":     ircRoleIDsFor(cm.Mode),
			"joined_at": m.JoinedAt.Format(time.RFC3339),
		})
		presences = append(presences, map[string]any{
			"user":   map[string]any{"id": uid},
			"status": presenceStatus(cm.Away),
		})
	}

	// Every Discord guild has an @everyone role whose id equals the guild id;
	// the client computes channel permissions through it, and without the
	// role channels appear inaccessible. The IRC prefix roles ride along so
	// member name colors and hoisted sidebar sections resolve. ADD_REACTIONS
	// only where the upstream can anchor them (MSGREFTYPES msgid).
	roles := append([]any{model.EveryoneRolePayload(net.ID, s.manager != nil && s.manager.ReactionsSupported(m.UserID, net.ID))}, model.IrcRolePayloads()...)

	return map[string]any{
		"id":                            net.ID,
		"name":                          net.Name,
		"icon":                          nil,
		"owner_id":                      model.ClydeID,
		"joined_at":                     m.JoinedAt.Format(time.RFC3339),
		"channels":                      channels,
		"members":                       members,
		"member_count":                  len(members),
		"large":                         false,
		"roles":                         roles,
		"presences":                     presences,
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
		"unavailable": !s.linkUp(m.UserID, net.ID),
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
			"unavailable": !s.linkUp(m.UserID, net.ID),
			"joined_at":   m.JoinedAt.Format(time.RFC3339),
		})
	}
	return guilds, nil
}

// linkUp reports upstream health for a membership; a nil manager (tests)
// counts as up so payloads stay available.
func (s *Service) linkUp(userID, networkID string) bool {
	if s.manager == nil {
		return true
	}
	return s.manager.LinkUp(userID, networkID)
}

// OnLinkChange is the manager's link-state hook: dead upstreams flip their
// guild to GUILD_UNAVAILABLE (clients grey it out and stop requesting
// members instead of hammering a zombie link), restored ones get a fresh
// GUILD_CREATE.
func (s *Service) OnLinkChange(userID, networkID string, up bool) {
	if s.gw == nil {
		return
	}
	if up {
		if mem, err := s.store.GetMembership(networkID, userID); err == nil {
			if net, err := s.store.GetNetwork(networkID); err == nil {
				s.gw.Dispatch(userID, "GUILD_CREATE", s.buildGuild(mem, net))
				return
			}
		}
		return
	}
	s.gw.Dispatch(userID, "GUILD_UNAVAILABLE", map[string]any{
		"guild_id":     networkID,
		"unavailable":  true,
	})
}
