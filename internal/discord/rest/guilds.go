package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

type createGuildRequest struct {
	// Discord's "Create a server" form sends {name, ...}; Voidbar reuses the
	// field as the IRC connection string.
	Name string `json:"name"`
}

func (s *Server) handleCreateGuild(w http.ResponseWriter, r *http.Request, u *storage.User) {
	var req createGuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		jsonError(w, http.StatusBadRequest, "connection string is required in name")
		return
	}
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	net, err := s.net.Join(u.ID, req.Name)
	if err != nil {
		if errors.Is(err, network.ErrBadConnString) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, "failed to join network")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   net.ID,
		"name": net.Name,
	})
}

// invitePayload builds a Discord-shaped Invite object for an IRC network
// the member has joined. The client renders the join-preview card from
// this (guild name, channel, member counts).
func (s *Server) invitePayload(code string, net *storage.Network, mem *storage.Membership, chans []*storage.Channel, u *storage.User) map[string]any {
	channel := map[string]any{"id": "0", "name": "irc", "type": 0}
	if len(chans) > 0 {
		channel = map[string]any{"id": chans[0].ID, "name": chans[0].Name, "type": 0}
		// Prefer the channel the user pasted (the invite code carries its
		// IRC name): the client navigates into invite.channel after joining.
		for _, ch := range chans {
			if strings.EqualFold(strings.TrimPrefix(ch.Name, "#"), strings.TrimPrefix(code, "#")) {
				channel = map[string]any{"id": ch.ID, "name": ch.Name, "type": 0}
				break
			}
		}
	}
	count := len(chans)
	if count == 0 {
		count = 1
	}
	return map[string]any{
		"type":                       0,
		"code":                       code,
		"guild":                      map[string]any{"id": net.ID, "name": net.Name, "splash": nil, "banner": nil, "description": nil, "icon": nil, "features": []any{}, "verification_level": 0, "vanity_url_code": nil, "nsfw_level": 0, "premium_subscription_count": 0},
		"guild_id":                   net.ID,
		"channel":                    channel,
		"inviter":                    map[string]any{"id": u.ID, "username": u.Username, "avatar": nil, "discriminator": "0", "public_flags": 0, "bot": false},
		"target_type":                nil,
		"target_user":                nil,
		"target_application":         nil,
		"expires_at":                 nil,
		"uses":                       0,
		"max_uses":                   0,
		"max_age":                    0,
		"temporary":                  false,
		"created_at":                 net.CreatedAt.UTC().Format(time.RFC3339),
		"member_count":               count,
		"presence_count":             count,
		"approximate_member_count":   count,
		"approximate_presence_count": count,
	}
}

// handleGetInvite answers the join-preview request the client fires while
// typing into the "join a server" field. Voidbar connection strings
// (irc://host:port/#chan?name=) are not Discord invite codes: the client
// extracts the last path segment as the "code" and passes the full string
// in inputValue - that query parameter carries the real request.
func (s *Server) handleGetInvite(w http.ResponseWriter, r *http.Request, u *storage.User) {
	code := r.PathValue("code")
	inputValue := r.URL.Query().Get("inputValue")
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	// Plain invite codes (no connection string in inputValue) are not part
	// of the Voidbar model: unknown invite.
	if _, err := network.ParseConnString(inputValue); err != nil {
		jsonError(w, http.StatusNotFound, "Unknown Invite")
		return
	}
	net, err := s.net.Join(u.ID, inputValue)
	if err != nil {
		if errors.Is(err, network.ErrBadConnString) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, "failed to join network")
		return
	}
	mem, err := s.net.MembershipFor(u.ID, net.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "membership missing")
		return
	}
	chans, err := s.net.ChannelsFor(net.ID, mem.AutoJoin)
	if err != nil {
		chans = nil
	}
	// The guild rail updates from GUILD_CREATE, not from this response.
	if s.gw != nil {
		for _, payload := range s.net.GuildCreateForUser(u.ID) {
			if g, ok := payload.(map[string]any); ok && g["id"] == net.ID {
				s.gw.Dispatch(u.ID, "GUILD_CREATE", g)
			}
		}
	}
	writeJSON(w, http.StatusOK, s.invitePayload(code, net, mem, chans, u))
}

// handleJoinInvite completes the join started by the preview GET: the
// client submits the fragment it parsed out of the pasted connection
// string (e.g. "#vbtest2") as the invite code. The preview GET has already
// created the network and membership, so the code resolves back through
// the member's auto-join channels; a full connection string as the code
// works too.
func (s *Server) handleJoinInvite(w http.ResponseWriter, r *http.Request, u *storage.User) {
	code := r.PathValue("code")
	if code == "" {
		jsonError(w, http.StatusBadRequest, "missing invite code")
		return
	}
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	var (
		net *storage.Network
		mem *storage.Membership
		err error
	)
	if _, perr := network.ParseConnString(code); perr == nil {
		// The raw connection string itself was submitted as the code.
		net, err = s.net.Join(u.ID, code)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid connection string")
			return
		}
		mem, err = s.net.MembershipFor(u.ID, net.ID)
	} else {
		// A parsed fragment: find the network the preview already joined.
		net, mem, err = s.net.FindByChannelName(u.ID, code)
	}
	var chans []*storage.Channel
	if err == nil {
		chans, _ = s.net.ChannelsFor(net.ID, mem.AutoJoin)
	} else {
		jsonError(w, http.StatusNotFound, "Unknown Invite")
		return
	}
	writeJSON(w, http.StatusOK, s.invitePayload(code, net, mem, chans, u))
}

// handleGuildDetail answers GET /guilds/:id with the channel list assembled
// from the channel registry (auto-join channels of the member).
func (s *Server) handleGuildDetail(w http.ResponseWriter, r *http.Request, u *storage.User) {
	guildID := r.PathValue("guild")
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	mem, err := s.net.MembershipFor(u.ID, guildID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown guild")
		return
	}
	net, err := s.net.Network(guildID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown guild")
		return
	}
	chans, err := s.net.ChannelsFor(guildID, mem.AutoJoin)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to list channels")
		return
	}
	channels := make([]any, 0, len(chans))
	for i, ch := range chans {
		channels = append(channels, map[string]any{
			"id":       ch.ID,
			"guild_id": guildID,
			"name":     ch.Name,
			"type":     0,
			"position": i,
			"topic":    nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           guildID,
		"name":         net.Name,
		"owner_id":     model.ClydeID,
		"channels":     channels,
		"member_count": len(channels) + 1,
	})
}

type sendMessageRequest struct {
	Content string `json:"content"`
	Nonce   any    `json:"nonce"`
}

// messagePayload builds the full stock Discord message shape shared by the
// send response, gateway MESSAGE_CREATE fanout and buffered history reads.
func messagePayload(id, channelID, content, ts, authorID, authorName string, nonce any) map[string]any {
	return map[string]any{
		"id":               id,
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
		"nonce":            nonce,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            authorID,
			"username":      authorName,
			"discriminator": "0",
			"bot":           false,
		},
	}
}

// handleGetMessages serves the channel replay buffer. Ordering follows the
// Discord REST contract: newest-first (descending by id) by default and
// with ?before=, ascending with ?after=, so the client's scroll-up paging
// works unchanged.
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request, u *storage.User) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	q := r.URL.Query()
	if s.net == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	buffered := s.net.ChannelMessages(r.PathValue("channel"), q.Get("before"), q.Get("after"), limit)
	out := make([]any, 0, len(buffered))
	for _, m := range buffered {
		out = append(out, messagePayload(m.ID, m.ChannelID, m.Content, m.Timestamp, model.IrcAuthorID(m.AuthorID), m.AuthorName, m.Nonce))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGuildPreview answers GET /guilds/:id/preview — the client fetches
// this when opening guild screens (e.g. settings). Minimal GuildPreview.
func (s *Server) handleGuildPreview(w http.ResponseWriter, r *http.Request, u *storage.User) {
	guildID := r.PathValue("guild")
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	net, err := s.net.Network(guildID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "Unknown Guild")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                       guildID,
		"name":                     net.Name,
		"icon":                     nil,
		"splash":                   nil,
		"discovery_splash":         nil,
		"emojis":                   []any{},
		"stickers":                 []any{},
		"features":                 []any{},
		"description":              "",
		"approximate_member_count": 1,
		"approximate_presence_count": 1,
	})
}

// handleLeaveGuild implements DELETE /users/@me/guilds/{guild} — the
// client's "Leave server" action. 204 per Discord; the client also reacts
// to the GUILD_DELETE dispatch, which is what actually clears the rail.
func (s *Server) handleLeaveGuild(w http.ResponseWriter, r *http.Request, u *storage.User) {
	guildID := r.PathValue("guild")
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	if err := s.net.Leave(u.ID, guildID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "Unknown Guild")
			return
		}
		jsonError(w, http.StatusInternalServerError, "leave failed")
		return
	}
	if s.gw != nil {
		s.gw.Dispatch(u.ID, "GUILD_DELETE", map[string]any{
			"id":          guildID,
			"unavailable": false,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSendMessage relays a Discord message to IRC. The channel id is a
// snowflake resolved through the channel registry.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, u *storage.User) {
	channelID := r.PathValue("channel")
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		jsonError(w, http.StatusBadRequest, "empty message")
		return
	}
	// The Android compose box sends a trailing newline with each send (the
	// same artifact Spacebar renders as a gap under every mobile message);
	// real Discord trims it server-side, so the bouncer does too.
	req.Content = strings.TrimRight(req.Content, " \t\r\n")
	if s.net == nil || s.irc == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	ch, err := s.net.ChannelByID(channelID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown channel")
		return
	}
	if err := s.irc.SendChannel(u.ID, ch.NetworkID, ch.IRCName, req.Content); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	// The response must be a full, unique message: the client clears the
	// optimistic "sending" entry by nonce and keys the store by id. A
	// constant or malformed id leaves the gray pending copy behind (and the
	// first message rendered twice).
	// Display the nick the bouncer actually holds on IRC (the membership
	// nick is synced on connect/renames): after a collision the account
	// username (doesnm) would otherwise hide the real nick (doesnm_).
	authorName := u.Username
	if mem, err := s.net.MembershipFor(u.ID, ch.NetworkID); err == nil && mem.Nick != "" {
		authorName = mem.Nick
	}
	msg := messagePayload(s.net.NewMessageID(), channelID, req.Content, time.Now().UTC().Format(time.RFC3339), u.ID, authorName, req.Nonce)
	// Own messages also arrive via the gateway on real Discord (keeping the
	// user's other sessions in sync); the client dedupes by id and the
	// nonce match clears the pending state.
	if s.gw != nil {
		s.gw.Dispatch(u.ID, "MESSAGE_CREATE", msg)
	}
	// And they enter the replay buffer like inbound relays, so history
	// shows the whole conversation after a reconnect/restart.
	if err := s.net.AppendBufferedMessage(storage.BufferedMessage{
		ID:         msg["id"].(string),
		ChannelID:  channelID,
		AuthorID:   u.ID,
		AuthorName: authorName,
		Content:    req.Content,
		Nonce:      req.Nonce,
		Timestamp:  msg["timestamp"].(string),
	}); err != nil {
		s.log.Warn("buffer append failed", "err", err, "channel", channelID)
	}
	writeJSON(w, http.StatusOK, msg)
}
