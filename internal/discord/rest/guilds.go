package rest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
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
			"id":              ch.ID,
			"guild_id":        guildID,
			"name":            ch.Name,
			"type":            0,
			"position":        i,
			"topic":           ircmanage.TopicValue(ch.Topic),
			"member_list_id":  model.MemberListID(guildID, ch.ID),
		})
	}
	// Keep the role set in sync with GUILD_CREATE: the member sidebar's
	// role sections and name colors resolve through the guild's roles.
	// ADD_REACTIONS only on msgid-capable upstreams (no picker otherwise).
	roles := append([]any{model.EveryoneRolePayload(guildID, s.irc != nil && s.irc.ReactionsSupported(u.ID, net.ID))}, model.IrcRolePayloads()...)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           guildID,
		"name":         net.Name,
		"owner_id":     model.ClydeID,
		"channels":     channels,
		"roles":        roles,
		"member_count": len(channels) + 1,
	})
}

type updateChannelRequest struct {
	Name  *string `json:"name"`
	Topic *string `json:"topic"`
}

// handleUpdateChannel answers PATCH /channels/:id - the channel settings
// screen's topic editor. Only the topic relays upstream (rename is a
// separate IRCv3 feature); the response echoes the request the way
// Discord does, and the authoritative CHANNEL_UPDATE follows from the
// server's TOPIC broadcast.
func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	var req updateChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	ch, err := s.net.ChannelByID(r.PathValue("channel"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "Unknown Channel")
		return
	}
	topic := ch.Topic
	if req.Topic != nil {
		topic = *req.Topic
		if s.irc != nil {
			if err := s.irc.SetTopic(u.ID, ch.NetworkID, ch.IRCName, *req.Topic); err != nil {
				jsonError(w, http.StatusServiceUnavailable, "upstream unavailable")
				return
			}
		}
	}
	name := ch.Name
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		raw := strings.TrimSpace(strings.TrimPrefix(*req.Name, "#"))
		if raw == "" || strings.ContainsAny(raw, " ,:\x07") || len(raw) > 50 {
			jsonError(w, http.StatusBadRequest, "invalid channel name")
			return
		}
		if !strings.EqualFold(raw, ch.Name) {
			if s.irc != nil {
				// draft/channel-rename upstream; the broadcast echo is
				// what persists (see Manager.applyRename).
				if err := s.irc.RenameChannel(u.ID, ch.NetworkID, ch.IRCName, "#"+raw); err != nil {
					jsonError(w, http.StatusServiceUnavailable, "upstream unavailable")
					return
				}
			}
		}
		name = raw
	}
	position := 0
	if mem, err := s.net.MembershipFor(u.ID, ch.NetworkID); err == nil {
		for i, name := range mem.AutoJoin {
			if strings.EqualFold(name, ch.IRCName) {
				position = i
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             ch.ID,
		"guild_id":       ch.NetworkID,
		"name":           name,
		"type":           0,
		"position":       position,
		"topic":          ircmanage.TopicValue(topic),
		"member_list_id": model.MemberListID(ch.NetworkID, ch.ID),
	})
}

// handleUpdateMemberMe relays the client's Change-Nickname flow
// (PATCH /guilds/:id/members/@me) to IRC NICK. Nothing persists here: the
// server's own-nick echo is the writer (see the NICK handler), so a
// rejected change (433 nick in use) leaves the stored nick standing and
// the follow-up GUILD_MEMBER_UPDATE never lies.
func (s *Server) handleUpdateMemberMe(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	mem, err := s.net.MembershipFor(u.ID, r.PathValue("guild"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "Unknown Guild")
		return
	}
	var req struct {
		Nick *string `json:"nick"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	live := mem.Nick
	if s.irc != nil {
		if ln := s.irc.LiveNick(u.ID, mem.NetworkID); ln != "" {
			live = ln
		}
	}
	nick := live
	if req.Nick != nil && strings.TrimSpace(*req.Nick) != "" {
		cand := strings.TrimSpace(*req.Nick)
		// IRC nick limits: no spaces/commas/colons (mask syntax), no #
		// prefix (channel), no control chars, within NICKLEN.
		if len(cand) > 30 || strings.ContainsAny(cand, " ,:#") || strings.ContainsFunc(cand, func(r rune) bool {
			return r < 0x20 || r == 0x7f
		}) {
			jsonError(w, http.StatusBadRequest, "invalid nickname")
			return
		}
		if !strings.EqualFold(cand, live) {
			if s.irc == nil {
				jsonError(w, http.StatusServiceUnavailable, "upstream unavailable")
				return
			}
			if err := s.irc.SetNick(u.ID, mem.NetworkID, cand); err != nil {
				jsonError(w, http.StatusServiceUnavailable, "upstream unavailable")
				return
			}
		}
		nick = cand
	}
	// A null nick ("Reset Nickname") has no IRC counterpart: answer with
	// the current member instead of erroring - an idempotent no-op.
	writeJSON(w, http.StatusOK, s.net.MemberPayload(u.ID, mem.NetworkID, nick))
}

type sendMessageRequest struct {
	Content string `json:"content"`
	Nonce   any    `json:"nonce"`
	// Attachments reference uploaded files: either cloud uploads
	// (uploaded_filename from POST /channels/:id/attachments) or - in
	// the legacy multipart flow - files[] parts of this very request.
	Attachments []sendAttachment `json:"attachments"`
}

type sendAttachment struct {
	ID              string `json:"id"`
	Filename        string `json:"filename"`
	UploadedFilename string `json:"uploaded_filename"`
}

// flexID accepts a snowflake as string OR bare JSON number: the Android
// client serializes some ids as numbers (see the gateway op 8/14 quirk),
// and a plain string field would fail the whole body decode.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*f = ""
		return nil
	}
	*f = flexID(s)
	return nil
}

// createDMRequest is the Create DM body: recipient_id (v9) or the first
// entry of recipients (v10+ shape, same effect for 1:1 threads).
type createDMRequest struct {
	RecipientID flexID   `json:"recipient_id"`
	Recipients  []flexID `json:"recipients"`
}

func (r createDMRequest) recipient() string {
	if r.RecipientID != "" {
		return string(r.RecipientID)
	}
	if len(r.Recipients) > 0 {
		return string(r.Recipients[0])
	}
	return ""
}

// handleStartTyping answers POST /channels/{id}/typing: the client fires
// this every few seconds while the user types; we relay it upstream as a
// draft/typing TAGMSG. Always 204 - a missing upstream indicator must
// never break the composer.
func (s *Server) handleStartTyping(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net != nil {
		if err := s.net.StartTyping(u.ID, r.PathValue("channel")); err != nil {
			s.log.Warn("typing relay failed", "err", err, "user", u.ID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCreateDM answers POST /users/@me/channels: the client's
// "new DM" picker hands us a user id; we resolve it back to an IRC nick
// (or a fellow bouncer member) and return the 1:1 channel.
func (s *Server) handleCreateDM(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	var req createDMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	recipient := req.recipient()
	if recipient == "" {
		jsonError(w, http.StatusBadRequest, "recipient_id is required")
		return
	}
	payload, err := s.net.CreateDMChannel(u.ID, recipient)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "unknown recipient")
		return
	}
	writeJSON(w, http.StatusOK, payload)
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
	// Scroll-up paging into network history: a short ?before= page means
	// the buffer floor is in sight - ask the upstream for older history
	// (draft/chathistory BEFORE) and re-read. Networks without the cap
	// (or nothing older) insert nothing and the page stands as-is.
	if q.Get("before") != "" && q.Get("after") == "" && len(buffered) < limit {
		if s.net.FetchOlderMessages(u.ID, r.PathValue("channel"), q.Get("before"), limit-len(buffered)) > 0 {
			buffered = s.net.ChannelMessages(r.PathValue("channel"), q.Get("before"), q.Get("after"), limit)
		}
	}
	out := make([]any, 0, len(buffered))
	for _, m := range buffered {
		payload := messagePayload(m.ID, m.ChannelID, m.Content, m.Timestamp, model.IrcAuthorID(m.AuthorID), m.AuthorName, m.Nonce)
		// Reaction pills come from the persisted state on the message
		// (updated on every live change), so history is restart-proof.
		if len(m.Reactions) > 0 {
			emojis := make([]string, 0, len(m.Reactions))
			for emoji := range m.Reactions {
				emojis = append(emojis, emoji)
			}
			sort.Strings(emojis)
			list := make([]any, 0, len(emojis))
			for _, emoji := range emojis {
				rc := map[string]any{"count": len(m.Reactions[emoji]), "me": false, "emoji": map[string]any{"id": nil, "name": emoji}}
				for _, uid := range m.Reactions[emoji] {
					if uid == u.ID {
						rc["me"] = true
						break
					}
				}
				list = append(list, rc)
			}
			payload["reactions"] = list
		}
		if len(m.Attachments) > 0 {
			payload["attachments"] = m.Attachments
		}
		if len(m.Embeds) > 0 {
			payload["embeds"] = m.Embeds
		}
		out = append(out, payload)
	}
	writeJSON(w, http.StatusOK, out)
}

// searchChannel returns every replay-buffer hit for the terms (AND,
// case-insensitive, over content and author names), newest first.
func (s *Server) searchChannel(channelID string, terms []string) []map[string]any {
	// The buffer returns newest-first with a hard ceiling; searching it
	// whole is a key-prefix scan, cheap enough at replay-buffer sizes.
	buffered := s.net.ChannelMessages(channelID, "", "", 1<<20)
	var hits []map[string]any
	for _, m := range buffered {
		haystack := strings.ToLower(m.Content + " " + m.AuthorName)
		matched := true
		for _, t := range terms {
			if !strings.Contains(haystack, t) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		hits = append(hits, messagePayload(m.ID, m.ChannelID, m.Content, m.Timestamp, model.IrcAuthorID(m.AuthorID), m.AuthorName, m.Nonce))
	}
	return hits
}

// searchTerms extracts the free-text terms the clients actually send:
// the official client uses ?content= (tokenized multi-word as repeated
// ?contents=slop|text values), other builds ?text= or ?query=.
func searchTerms(r *http.Request) []string {
	q := r.URL.Query()
	var raws []string
	raws = append(raws, q.Get("content"))
	for _, tok := range q["contents"] {
		// Tokenized form: "<slop>|<text>" - the slop (words allowed
		// between tokens) is ignored, we substring-match instead.
		if i := strings.Index(tok, "|"); i >= 0 {
			tok = tok[i+1:]
		}
		raws = append(raws, tok)
	}
	raws = append(raws, q.Get("text"), q.Get("query"), q.Get("q"))
	var terms []string
	seen := map[string]bool{}
	for _, raw := range raws {
		for _, t := range strings.Fields(strings.ToLower(raw)) {
			if !seen[t] {
				seen[t] = true
				terms = append(terms, t)
			}
		}
	}
	return terms
}

// writeSearchResults pages a newest-first hit list Discord-style: groups
// of one, 25 per page via ?offset=, total_results across pages. Search
// hits carry "hit": true (and no reactions key), per the userdoccers
// shape - the client keys its result list off the flag.
func writeSearchResults(w http.ResponseWriter, hits []map[string]any, offset int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(hits) {
		offset = len(hits)
	}
	page := hits[offset:]
	if len(page) > 25 {
		page = page[:25]
	}
	groups := make([]any, 0, len(page))
	for _, h := range page {
		h["hit"] = true
		h["components"] = []any{}
		delete(h, "reactions")
		groups = append(groups, []any{h})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"analytics_id":                "voidbar",
		"doing_deep_historical_index": false,
		"total_results":               len(hits),
		"messages":                    groups,
	})
}

// handleSearchMessages serves GET /channels/{c}/messages/search: the
// client's search box, answered from the channel's replay buffer. IRC
// has no server-side search, so results cover everything the bouncer
// has seen (live traffic plus any chathistory pulled during scrolls).
func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		writeJSON(w, http.StatusOK, map[string]any{"analytics_id": "voidbar", "total_results": 0, "messages": []any{}})
		return
	}
	terms := searchTerms(r)
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	if len(terms) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"analytics_id": "voidbar", "total_results": 0, "messages": []any{}})
		return
	}
	hits := s.searchChannel(r.PathValue("channel"), terms)
	writeSearchResults(w, hits, offset)}

// handleSearchGuildMessages serves GET /guilds/{g}/messages/search: the
// official client's search bar searches the whole guild (scoped to the
// current channel via ?channel_id=). Same replay-buffer backing as the
// channel route, merged across channels, newest first.
func (s *Server) handleSearchGuildMessages(w http.ResponseWriter, r *http.Request, u *storage.User) {
	empty := map[string]any{"analytics_id": "voidbar", "total_results": 0, "messages": []any{}}
	if s.net == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	mem, err := s.net.MembershipFor(u.ID, r.PathValue("guild"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown guild")
		return
	}
	terms := searchTerms(r)
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	if len(terms) == 0 {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	channels, err := s.net.ChannelsFor(mem.NetworkID, mem.AutoJoin)
	if err != nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	// Docs allow repeated ?channel_id= filters; empty means all channels.
	only := map[string]bool{}
	for _, id := range r.URL.Query()["channel_id"] {
		if id != "" {
			only[id] = true
		}
	}
	var hits []map[string]any
	for _, ch := range channels {
		if len(only) > 0 && !only[ch.ID] {
			continue
		}
		hits = append(hits, s.searchChannel(ch.ID, terms)...)
	}
	// Snowflake ids order chronologically; the client shows newest first.
	sort.Slice(hits, func(i, j int) bool {
		return hits[i]["id"].(string) > hits[j]["id"].(string)
	})
	writeSearchResults(w, hits, offset)
}

// decodeEmoji unescapes the emoji path segment if it still carries
// percent-encoding (ServeMux wildcards match escaped paths; non-ASCII
// emoji arrive encoded) and reduces a custom emoji "name:id" to its name.
// percent-encoding (ServeMux wildcards match escaped paths; non-ASCII
// emoji arrive encoded) and reduces a custom emoji "name:id" to its name.
func decodeEmoji(raw string) string {
	if strings.Contains(raw, "%") {
		if unescaped, err := url.PathUnescape(raw); err == nil {
			raw = unescaped
		}
	}
	if i := strings.Index(raw, ":"); i > 0 {
		raw = raw[:i]
	}
	return raw
}

// handleReactSelf serves PUT/DELETE
// /channels/{c}/messages/{m}/reactions/{emoji}/@me: the client toggling
// its own reaction. Relayed upstream as +draft/react / +draft/unreact
// (with +reply) when the message has a known msgid; the local state and
// gateway fanout happen regardless so the pill never sticks dead.
func (s *Server) handleReactSelf(w http.ResponseWriter, r *http.Request, u *storage.User) {
	channelID := r.PathValue("channel")
	messageID := r.PathValue("message")
	emoji := decodeEmoji(r.PathValue("emoji"))
	if emoji == "" {
		jsonError(w, http.StatusBadRequest, "missing emoji")
		return
	}
	if s.net == nil || s.irc == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	target, networkID := "", ""
	if ch, err := s.net.ChannelByID(channelID); err == nil {
		target, networkID = ch.IRCName, ch.NetworkID
	} else if dm, err := s.net.DMChannelByID(channelID); err == nil && dm.OwnerID == u.ID {
		target, networkID = dm.Nick, dm.NetworkID
	} else {
		jsonError(w, http.StatusNotFound, "unknown channel")
		return
	}
	if err := s.irc.SendReaction(u.ID, networkID, target, messageID, channelID, emoji, r.Method == http.MethodDelete); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteMessage serves DELETE /channels/{c}/messages/{m}: the user
// deleting their own message. Relayed upstream as draft/message-redaction
// when the msgid is known and the upstream supports it (eris REDACT);
// without an upstream path the deletion is bouncer-local — replay and all
// sessions drop the message, IRC peers keep the original.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request, u *storage.User) {
	channelID := r.PathValue("channel")
	messageID := r.PathValue("message")
	if s.net == nil || s.irc == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	target, networkID := "", ""
	if ch, err := s.net.ChannelByID(channelID); err == nil {
		target, networkID = ch.IRCName, ch.NetworkID
	} else if dm, err := s.net.DMChannelByID(channelID); err == nil && dm.OwnerID == u.ID {
		target, networkID = dm.Nick, dm.NetworkID
	} else {
		jsonError(w, http.StatusNotFound, "unknown channel")
		return
	}
	err := s.irc.DeleteMessage(u.ID, networkID, target, messageID, channelID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ircmanage.ErrUnknownMessage):
		// Discord also answers 404 for unknown message ids.
		jsonError(w, http.StatusNotFound, "unknown message")
	case errors.Is(err, ircmanage.ErrNotOwner):
		jsonError(w, http.StatusForbidden, err.Error())
	default:
		jsonError(w, http.StatusConflict, err.Error())
	}
}

// handleCreateChannel is the client's "create channel": optimistic — IRC
// servers create channels on JOIN, so the channel returns immediately and
// upstream refusals roll back asynchronously (Clyde DM explains why).
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request, u *storage.User) {
	guildID := r.PathValue("guild")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	payload, err := s.net.CreateChannel(u.ID, guildID, req.Name)
	if err != nil {
		if errors.Is(err, network.ErrBadChannelName) {
			jsonError(w, http.StatusBadRequest, "Invalid channel name for IRC")
			return
		}
		if errors.Is(err, storage.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "Unknown Guild")
			return
		}
		jsonError(w, http.StatusInternalServerError, "create failed")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleDeleteChannel covers both guild channels (PART upstream, registry
// kept for history recovery) and DM closes.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	channelID := r.PathValue("channel")
	// DM close: drop the thread record; IRC queries recreate it on contact.
	if dm, err := s.net.DMChannelByID(channelID); err == nil {
		if dm.OwnerID != u.ID {
			jsonError(w, http.StatusNotFound, "unknown channel")
			return
		}
		if err := s.net.DeleteDMChannel(channelID); err != nil {
			jsonError(w, http.StatusInternalServerError, "close failed")
			return
		}
		if s.gw != nil {
			s.gw.Dispatch(u.ID, "CHANNEL_DELETE", map[string]any{"id": channelID, "type": 1})
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.net.RemoveChannel(u.ID, channelID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "Unknown Channel")
			return
		}
		jsonError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	// Attachment rows for the payload plus the URLs the IRC wire copy
	// must carry (IRC has no attachments - the link IS the transfer).
	var attachRows []any
	var attachURLs []string
	// Web clients (Flicker) always send multipart/form-data - their send
	// path doubles as the upload path, with the message object packed
	// into a payload_json form field (Discord upload semantics) - while
	// the Android client sends plain JSON. Accept both shapes.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if pj := r.FormValue("payload_json"); pj != "" {
			if err := json.Unmarshal([]byte(pj), &req); err != nil {
				jsonError(w, http.StatusBadRequest, "invalid body")
				return
			}
		} else {
			req.Content = r.FormValue("content")
			req.Nonce = r.FormValue("nonce")
		}
		// Legacy inline upload: files[] parts are stored directly.
		if r.MultipartForm != nil {
			files := r.MultipartForm.File["files"]
			for i, fh := range files {
				f, err := fh.Open()
				if err != nil {
					jsonError(w, http.StatusBadRequest, "unreadable file")
					return
				}
				data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
				_ = f.Close()
				if err != nil || int64(len(data)) > maxUploadBytes {
					jsonError(w, http.StatusBadRequest, "unreadable file")
					return
				}
				att := s.newStoredAttachment(fh.Filename, data)
				rowID := strconv.Itoa(i)
				if len(req.Attachments) > i && req.Attachments[i].ID != "" {
					rowID = req.Attachments[i].ID
				}
				attachRows = append(attachRows, s.attachmentPayload(rowID, att))
				attachURLs = append(attachURLs, s.attachmentURL(att.ID, att.Filename))
			}
		}
	} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if s.net == nil || s.irc == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	// Cloud-upload flow: attachments[] rows with uploaded_filename.
	if len(req.Attachments) > 0 {
		rows, urls, err := s.resolveSendAttachments(req.Attachments)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "unknown uploaded attachment")
			return
		}
		attachRows = append(attachRows, rows...)
		attachURLs = append(attachURLs, urls...)
	}
	// Content may be empty when the message is a bare upload (Discord
	// allows that; IRC gets the bare links).
	if strings.TrimSpace(req.Content) == "" && len(attachRows) == 0 {
		jsonError(w, http.StatusBadRequest, "empty message")
		return
	}
	// The Android compose box sends a trailing newline with each send (the
	// same artifact Spacebar renders as a gap under every mobile message);
	// real Discord trims it server-side, so the bouncer does too.
	req.Content = strings.TrimRight(req.Content, " \t\r\n")
	// DM thread: the snowflake resolves to a DMChannel and the relay
	// targets the peer's nick instead of a #channel.
	if dm, err := s.net.DMChannelByID(channelID); err == nil {
		if dm.OwnerID != u.ID {
			jsonError(w, http.StatusNotFound, "unknown channel")
			return
		}
		// Snowflake minted before the send: the manager queues it so the
		// echo-message echo can bind the upstream msgid to it (reactions).
		msgID := s.net.NewMessageID()
		// Discord-side content (with <@id> markers) is what the client
		// sees and what gets buffered; the wire copy carries bare nicks.
		// Attachments travel as plain URLs - IRC has nothing else.
		wire := strings.TrimSpace(s.irc.IRCize(u.ID, dm.NetworkID, dm.Nick, req.Content) + " " + strings.Join(attachURLs, " "))
		if err := s.irc.SendQuery(u.ID, dm.NetworkID, dm.Nick, wire, msgID, channelID); err != nil {
			jsonError(w, http.StatusConflict, err.Error())
			return
		}
		authorName := u.Username
		if mem, err := s.net.MembershipFor(u.ID, dm.NetworkID); err == nil && mem.Nick != "" {
			authorName = mem.Nick
		}
	msg := messagePayload(msgID, channelID, req.Content, model.NowTimestamp(), u.ID, authorName, req.Nonce)
		if len(attachRows) > 0 {
			msg["attachments"] = attachRows
		}
		// Pills render from the user store, but highlighting reads the
		// mentions array - own sends carry it like inbound relays do.
		if mentioned, _ := s.irc.MentionPayloadsFromMarkers(u.ID, dm.NetworkID, dm.Nick, req.Content); len(mentioned) > 0 {
			msg["mentions"] = mentioned
		}
		if s.gw != nil {
			s.gw.Dispatch(u.ID, "MESSAGE_CREATE", msg)
		}
		if err := s.net.AppendBufferedMessage(storage.BufferedMessage{
			ID:          msg["id"].(string),
			ChannelID:   channelID,
			AuthorID:    u.ID,
			AuthorName:  authorName,
			Content:     req.Content,
			Nonce:       req.Nonce,
			Timestamp:   msg["timestamp"].(string),
			Attachments: attachRows,
		}); err != nil {
			s.log.Warn("buffer append failed", "err", err, "channel", channelID)
		}
		s.net.TouchDMChannel(channelID)
		writeJSON(w, http.StatusOK, msg)
		return
	}
	ch, err := s.net.ChannelByID(channelID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown channel")
		return
	}
	// See the DM path: snowflake before the send, for msgid correlation.
	msgID := s.net.NewMessageID()
	// The wire copy carries bare nicks instead of <@id> markers (IRC
	// convention); the Discord-side copy keeps the markers for pills.
	// Attachments become plain URLs on the wire.
	wire := strings.TrimSpace(s.irc.IRCize(u.ID, ch.NetworkID, ch.IRCName, req.Content) + " " + strings.Join(attachURLs, " "))
	if err := s.irc.SendChannel(u.ID, ch.NetworkID, ch.IRCName, wire, msgID, channelID); err != nil {
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
	msg := messagePayload(msgID, channelID, req.Content, model.NowTimestamp(), u.ID, authorName, req.Nonce)
	if len(attachRows) > 0 {
		msg["attachments"] = attachRows
	}
	// Own sends carry the mentions arrays too: highlighting reads them,
	// not the pills (same as inbound relays).
	if mentioned, mentionChans := s.irc.MentionPayloadsFromMarkers(u.ID, ch.NetworkID, ch.IRCName, req.Content); len(mentioned) > 0 || len(mentionChans) > 0 {
		if len(mentioned) > 0 {
			msg["mentions"] = mentioned
		}
		if len(mentionChans) > 0 {
			msg["mention_channels"] = mentionChans
		}
	}
	// Own messages also arrive via the gateway on real Discord (keeping the
	// user's other sessions in sync); the client dedupes by id and the
	// nonce match clears the pending state.
	if s.gw != nil {
		s.gw.Dispatch(u.ID, "MESSAGE_CREATE", msg)
	}
	// And they enter the replay buffer like inbound relays, so history
	// shows the whole conversation after a reconnect/restart.
	if err := s.net.AppendBufferedMessage(storage.BufferedMessage{
		ID:          msg["id"].(string),
		ChannelID:   channelID,
		AuthorID:    u.ID,
		AuthorName:  authorName,
		Content:     req.Content,
		Nonce:       req.Nonce,
		Timestamp:   msg["timestamp"].(string),
		Attachments: attachRows,
	}); err != nil {
		s.log.Warn("buffer append failed", "err", err, "channel", channelID)
	}
	writeJSON(w, http.StatusOK, msg)
}
