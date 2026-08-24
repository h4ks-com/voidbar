package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/core/network"
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
	net, err := s.net.Join(u.ID, code)
	if err != nil {
		if errors.Is(err, network.ErrBadConnString) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, "failed to join network")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"guild": map[string]any{
			"id":   net.ID,
			"name": net.Name,
		},
	})
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
		"channels":     channels,
		"member_count": len(channels),
	})
}

type sendMessageRequest struct {
	Content string `json:"content"`
	Nonce   any    `json:"nonce"`
}

// handleGetMessages answers the channel history request. Voidbar's replay
// buffer arrives in a later phase; for now channels start empty, which the
// client renders as the beginning of history.
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request, u *storage.User) {
	writeJSON(w, http.StatusOK, []any{})
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
	msg := map[string]any{
		"id":               s.net.NewMessageID(),
		"channel_id":       channelID,
		"content":          req.Content,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
		"edited_timestamp": nil,
		"tts":              false,
		"mention_everyone": false,
		"mentions":         []any{},
		"mention_roles":    []any{},
		"mention_channels": []any{},
		"attachments":      []any{},
		"embeds":           []any{},
		"reactions":        []any{},
		"nonce":            req.Nonce,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            u.ID,
			"username":      authorName,
			"discriminator": "0",
			"bot":           false,
		},
	}
	// Own messages also arrive via the gateway on real Discord (keeping the
	// user's other sessions in sync); the client dedupes by id and the
	// nonce match clears the pending state.
	if s.gw != nil {
		s.gw.Dispatch(u.ID, "MESSAGE_CREATE", msg)
	}
	writeJSON(w, http.StatusOK, msg)
}
