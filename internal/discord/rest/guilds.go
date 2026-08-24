package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
// from the member's auto-join channels and live IRC state.
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
	channels := make([]any, 0, len(mem.AutoJoin))
	for i, ch := range mem.AutoJoin {
		channels = append(channels, map[string]any{
			"id":       guildID + ":" + ch,
			"guild_id": guildID,
			"name":     strings.TrimPrefix(ch, "#"),
			"type":     0,
			"position": i,
			"topic":    nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           guildID,
		"name":         net.Name,
		"channels":     channels,
		"member_count": len(mem.AutoJoin),
	})
}

type sendMessageRequest struct {
	Content string `json:"content"`
}

// handleGetMessages answers the channel history request. Voidbar's replay
// buffer arrives in a later phase; for now channels start empty, which the
// client renders as the beginning of history.
func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request, u *storage.User) {
	writeJSON(w, http.StatusOK, []any{})
}

// handleSendMessage relays a Discord message to IRC. The channel id is
// "<networkID>:<irc-channel>".
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
	netID, ircChannel, ok := strings.Cut(channelID, ":")
	if !ok || s.irc == nil {
		jsonError(w, http.StatusBadRequest, "invalid channel")
		return
	}
	if err := s.irc.SendChannel(u.ID, netID, ircChannel, req.Content); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         u.ID + ":" + channelID,
		"channel_id": channelID,
		"content":    req.Content,
		"author": map[string]any{
			"id":            u.ID,
			"username":      u.Username,
			"discriminator": "0",
			"bot":           false,
		},
	})
}
