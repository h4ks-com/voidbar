package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

const DefaultHeartbeatInterval = 41250

type Server struct {
	auth               *auth.Service
	cfg                *config.Config
	log                *slog.Logger
	heartbeatInterval  int
	guildsForUser      func(userID string) ([]any, error)
	guildCreateForUser func(userID string) []any
	dmChannelsForUser  func(userID string) []any
	memberListForUser  func(userID, guildID, channelID string) any
	memberChunkForUser func(userID, guildID, nonce string, userIDs []string) any

	mu       sync.RWMutex
	sessions map[string]*Session
	byUser   map[string]map[string]*Session

	settingsForUser func(userID string) map[string]any
}

// SetGuildProviders installs the READY/GUILD_CREATE hooks after
// construction (the network service needs the gateway instance first, so
// the hooks cannot be passed to New in the wiring order main uses).
func (s *Server) SetGuildProviders(guildsForUser func(userID string) ([]any, error), guildCreateForUser func(userID string) []any) {
	s.guildsForUser = guildsForUser
	s.guildCreateForUser = guildCreateForUser
}

// SetDMChannelsProvider installs the READY private_channels hook; same
// late-wiring rationale as SetGuildProviders.
func (s *Server) SetDMChannelsProvider(dmChannelsForUser func(userID string) []any) {
	s.dmChannelsForUser = dmChannelsForUser
}

// SetSettingsProvider installs the READY user_settings hook. Web clients
// (Flicker etc.) validate user_settings with strict schemas, so READY must
// always carry the full defaults-overlaid map, not an empty object.
func (s *Server) SetSettingsProvider(settingsForUser func(userID string) map[string]any) {
	s.settingsForUser = settingsForUser
}

func (s *Server) userSettings(userID string) map[string]any {
	if s.settingsForUser != nil {
		if m := s.settingsForUser(userID); m != nil {
			return m
		}
	}
	return model.DefaultUserSettings()
}

// SetMemberListProvider installs the GUILD_MEMBER_LIST_UPDATE hook serving
// op 14 (lazy request) — the client's channel member list ask.
func (s *Server) SetMemberListProvider(memberListForUser func(userID, guildID, channelID string) any) {
	s.memberListForUser = memberListForUser
}

// SetMemberChunkProvider installs the GUILD_MEMBERS_CHUNK hook serving
// op 8 (Request Guild Members, documented).
func (s *Server) SetMemberChunkProvider(memberChunkForUser func(userID, guildID, nonce string, userIDs []string) any) {
	s.memberChunkForUser = memberChunkForUser
}

// New creates the gateway server. guildsForUser (optional) supplies the
// guild stubs for READY; guildCreateForUser (optional) supplies full
// GUILD_CREATE payloads dispatched right after READY, which is what fills
// the client's guild rail.
func New(a *auth.Service, cfg *config.Config, log *slog.Logger, guildsForUser func(userID string) ([]any, error), guildCreateForUser func(userID string) []any) *Server {
	return &Server{
		auth:               a,
		cfg:                cfg,
		log:                log,
		heartbeatInterval:  DefaultHeartbeatInterval,
		guildsForUser:      guildsForUser,
		guildCreateForUser: guildCreateForUser,
		sessions:           make(map[string]*Session),
		byUser:             make(map[string]map[string]*Session),
	}
}

func (s *Server) Dispatch(userID, t string, d any) {
	s.mu.RLock()
	sessions := make([]*Session, 0, len(s.byUser[userID]))
	for _, sess := range s.byUser[userID] {
		sessions = append(sessions, sess)
	}
	s.mu.RUnlock()
	for _, sess := range sessions {
		if _, err := sess.dispatch(t, d, true); err != nil {
			s.log.Error("dispatch failed", "err", err, "user", userID, "t", t)
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	ch := make(chan []byte, 256)
	done := make(chan struct{})
	compress := r.URL.Query().Get("compress") == "zlib-stream"
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		s.writePump(conn, ch, done, compress)
	}()
	defer func() {
		_ = conn.Close()
		close(done)
		<-writeDone
	}()
	s.handleConn(conn, ch)
}

func (s *Server) writePump(conn *websocket.Conn, ch <-chan []byte, done <-chan struct{}, compress bool) {
	defer conn.Close()
	var (
		zw  *zlib.Writer
		buf *bytes.Buffer
	)
	if compress {
		buf = &bytes.Buffer{}
		zw = zlib.NewWriter(buf)
	}
	for {
		var frame []byte
		select {
		case <-done:
			return
		case frame = <-ch:
			// nil frames come from a channel closed by session takeover.
			if frame == nil {
				return
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		var err error
		if compress {
			buf.Reset()
			if _, err = zw.Write(frame); err == nil {
				err = zw.Flush()
			}
			if err == nil {
				err = conn.WriteMessage(websocket.BinaryMessage, buf.Bytes())
			}
		} else {
			err = conn.WriteMessage(websocket.TextMessage, frame)
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) handleConn(conn *websocket.Conn, ch chan []byte) {
	var sess *Session

	hello, err := json.Marshal(opFrame{Op: OpHello, D: mustJSON(HelloData{
		HeartbeatInterval: s.heartbeatInterval,
		Trace:             []string{"voidbar"},
	})})
	if err != nil {
		return
	}
	ch <- hello

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var p Payload
		if err := json.Unmarshal(data, &p); err != nil {
			s.closeWS(conn, CloseDecodeError, "decode error")
			return
		}
		switch p.Op {
		case OpIdentify:
			if sess != nil {
				s.closeWS(conn, CloseAlreadyAuthenticated, "already authenticated")
				return
			}
			var d IdentifyData
			if err := json.Unmarshal(p.D, &d); err != nil || d.Token == "" {
				s.closeWS(conn, CloseDecodeError, "decode error")
				return
			}
			user, err := s.auth.ValidateToken(authToken(d.Token))
			if err != nil {
				s.closeWS(conn, CloseAuthenticationFailed, "authentication failed")
				return
			}
			sess = s.createSession(user.ID)
			sess.attach(ch)
			if _, err := sess.dispatch("READY", s.buildReady(sess, user), true); err != nil {
				s.log.Error("ready failed", "err", err)
				return
			}
			if s.guildCreateForUser != nil {
				for _, guild := range s.guildCreateForUser(user.ID) {
					if _, err := sess.dispatch("GUILD_CREATE", guild, true); err != nil {
						s.log.Error("guild create failed", "err", err)
						return
					}
				}
			}
		case OpResume:
			if sess != nil {
				s.closeWS(conn, CloseAlreadyAuthenticated, "already authenticated")
				return
			}
			var d ResumeData
			if err := json.Unmarshal(p.D, &d); err != nil || d.Token == "" {
				s.closeWS(conn, CloseDecodeError, "decode error")
				return
			}
			user, err := s.auth.ValidateToken(authToken(d.Token))
			if err != nil {
				s.closeWS(conn, CloseAuthenticationFailed, "authentication failed")
				return
			}
			old := s.findSession(d.SessionID)
			if old == nil || old.UserID != user.ID {
				frame, _ := json.Marshal(opFrame{Op: OpInvalidSession, D: json.RawMessage("false")})
				ch <- frame
				continue
			}
			old.attach(ch)
			old.replay(d.Seq, ch)
			// RESUMED with an empty object: d:null made the client's event
			// hydrator read properties of null (_getConnectionPath/_trace).
			if _, err := old.dispatch("RESUMED", map[string]any{"_trace": []string{"voidbar"}}, false); err != nil {
				s.log.Error("resumed failed", "err", err)
				return
			}
			sess = old
		case OpHeartbeat:
			ack, _ := json.Marshal(opFrame{Op: OpHeartbeatACK})
			ch <- ack
		case OpPresenceUpdate, OpVoiceStateUpdate:
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
		case OpRequestGuildMembers:
			// Documented member fetch (userdocs: Request Guild Members):
			// this client's StoreGuildMemberRequester fires it when a
			// channel opens. Reply with one GUILD_MEMBERS_CHUNK built from
			// the same IRC NAMES state the lazy list uses.
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
			var d struct {
				GuildID  json.RawMessage   `json:"guild_id"`
				UserIDs  []json.RawMessage `json:"user_ids"`
				Query    string            `json:"query"`
				Limit    int               `json:"limit"`
				Presences bool             `json:"presences"`
				Nonce    string            `json:"nonce"`
			}
			if err := json.Unmarshal(p.D, &d); err != nil {
				continue
			}
			if s.memberChunkForUser == nil {
				continue
			}
			guildIDs := rawIDsToStrings(d.GuildID)
			userIDs := make([]string, 0, len(d.UserIDs))
			for _, raw := range d.UserIDs {
				userIDs = append(userIDs, rawIDsToStrings(raw)...)
			}
			s.log.Info("op8 request", "guilds", guildIDs, "users", userIDs, "query", d.Query, "limit", d.Limit)
			for _, gid := range guildIDs {
				if payload := s.memberChunkForUser(sess.UserID, gid, d.Nonce, userIDs); payload != nil {
					if _, err := sess.dispatch("GUILD_MEMBERS_CHUNK", payload, true); err != nil {
						s.log.Error("member chunk dispatch failed", "err", err, "guild", gid)
					}
				}
			}
		case OpGuildMembersApps:
			// Guild subscriptions v2 (op 24): the Android client retries
			// it while its member view is unresolved, but empirically our
			// answers (nonce'd chunk and/or guild-wide list update)
			// CRASH the 126.21 client into a launch loop - verified by
			// logcat: op24 in, answer out, process relaunch, repeat at
			// 1.7s cadence. Until the expected response shape is known,
			// stay silent like before; guild unavailability (which stops
			// the asking for dead upstreams) handles the storm case.
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
		case OpCallConnect:
			// Call-connect sync: the client fires it per open channel with
			// a numeric channel_id. Text channels never have calls, so the
			// correct answer is silence.
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
		case OpGuildSubscriptions:
			// Lazy request: the client opened a channel and wants its
			// member list. Reply with a GUILD_MEMBER_LIST_UPDATE SYNC per
			// requested channel; the list id is the channel's own
			// member_list_id (per-channel, see model.MemberListID).
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
			// guild_id is numeric here too (see rawIDsToStrings).
			var d struct {
				GuildID  json.RawMessage        `json:"guild_id"`
				Channels map[string][][]float64 `json:"channels"`
			}
			if err := json.Unmarshal(p.D, &d); err != nil || len(d.GuildID) == 0 {
				s.log.Warn("op13/14 decode failed", "err", err, "raw", string(p.D))
				continue
			}
			gids := rawIDsToStrings(d.GuildID)
			if len(gids) == 0 {
				s.log.Warn("op13/14 decode failed: no guild id", "raw", string(p.D))
				continue
			}
			s.log.Info("op14 request", "guild", gids[0], "channels", len(d.Channels))
			guildID := gids[0]
			if s.memberListForUser == nil {
				continue
			}
			// Channels omitted (or empty) = the guild-wide "everyone" list.
			if len(d.Channels) == 0 {
				sess.watchMemberList(guildID, "")
				if payload := s.memberListForUser(sess.UserID, guildID, ""); payload != nil {
					if _, err := sess.dispatch("GUILD_MEMBER_LIST_UPDATE", payload, true); err != nil {
						s.log.Error("member list dispatch failed", "err", err)
					}
				}
				continue
			}
			for channelID := range d.Channels {
				sess.watchMemberList(guildID, channelID)
				payload := s.memberListForUser(sess.UserID, guildID, channelID)
				if payload == nil {
					continue
				}
				if _, err := sess.dispatch("GUILD_MEMBER_LIST_UPDATE", payload, true); err != nil {
					s.log.Error("member list dispatch failed", "err", err, "channel", channelID)
				}
			}
		default:
			// Tolerate unknown opcodes (the client sends some pre-IDENTIFY
			// frames we don't model); closing here tore down healthy sockets
			// with 4001 loops.
			s.log.Warn("gateway unknown opcode", "op", p.Op, "authenticated", sess != nil)
		}
		if sess != nil {
			_ = conn.SetReadDeadline(time.Now().Add(time.Duration(float64(s.heartbeatInterval)*1.5) * time.Millisecond))
		}
	}
	if sess != nil {
		sess.detach(ch)
	}
}

func authToken(t string) string {
	t = strings.TrimSpace(t)
	if rest, ok := strings.CutPrefix(t, "Bot "); ok {
		return strings.TrimSpace(rest)
	}
	return t
}

// rawIDsToStrings accepts a JSON snowflake that may be a string, a number
// or an array of either, and normalizes it to decimal strings.
//
// COMPAT QUIRK: the docs say snowflakes are strings, but the Android client
// serializes snowflake fields in OUTGOING gateway payloads as bare JSON
// numbers (op 8 guild_id/user_ids look like [1541479714630139904]). Any
// handler that reads a client-sent snowflake must go through this helper
// (or otherwise accept both) — unmarshalling into a plain string field
// silently yields "" and the request looks empty.
func rawIDsToStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return []string{n.String()}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			out = append(out, rawIDsToStrings(item)...)
		}
		return out
	}
	return nil
}

func (s *Server) closeWS(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
}

func (s *Server) createSession(userID string) *Session {
	id, err := util.RandomToken(16)
	if err != nil {
		id = util.SHA256Hex(userID + time.Now().String())[:32]
	}
	sess := &Session{ID: id, UserID: userID}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
	if s.byUser[userID] == nil {
		s.byUser[userID] = make(map[string]*Session)
	}
	s.byUser[userID][id] = sess
	return sess
}

func (s *Server) findSession(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

// MemberListSpec identifies one lazy member list: the channel's own
// (ChannelID empty = the guild-wide everyone list).
type MemberListSpec struct {
	GuildID   string
	ChannelID string
}

// RequestedMemberLists returns the union of op 14 subscriptions across
// all of the user's live sessions - the lists worth pushing occupancy
// updates for.
func (s *Server) RequestedMemberLists(userID string) []MemberListSpec {
	s.mu.RLock()
	sessions := make([]*Session, 0, len(s.byUser[userID]))
	for _, sess := range s.byUser[userID] {
		sessions = append(sessions, sess)
	}
	s.mu.RUnlock()
	seen := map[string]bool{}
	var out []MemberListSpec
	for _, sess := range sessions {
		for k := range sess.memberListSet() {
			if seen[k] {
				continue
			}
			seen[k] = true
			guild, channel, _ := strings.Cut(k, "\x00")
			out = append(out, MemberListSpec{GuildID: guild, ChannelID: channel})
		}
	}
	return out
}

func (s *Server) buildReady(sess *Session, user *storage.User) *ReadyData {
	guilds := []any{}
	if s.guildsForUser != nil {
		if raw, err := s.guildsForUser(user.ID); err == nil {
			guilds = append(guilds, raw...)
		}
	}
	// Debug bisect switch for client-compat work: VOIDBAR_READY_MINIMAL
	// strips READY down to the fields one by one until a picky client
	// (Android 126.21) accepts it, pinpointing the offending shape.
	if os.Getenv("VOIDBAR_READY_MINIMAL") != "" {
		s.log.Warn("ready_bisect: sending minimal READY (no guilds/users)")
		guilds = []any{}
		return &ReadyData{
			V:                9,
			User:             model.ToUser(user),
			Guilds:           guilds,
			SessionID:        sess.ID,
			ResumeURL:        s.cfg.GatewayWSURL(),
			ResumeGatewayURL: s.cfg.GatewayWSURL(),
			SessionType:      "normal",
		}
	}
	privateChannels := []any{}
	if s.dmChannelsForUser != nil {
		if dms := s.dmChannelsForUser(user.ID); dms != nil {
			privateChannels = dms
		}
	}
	return &ReadyData{
		V:                    9,
		User:                 model.ToUser(user),
		Guilds:               guilds,
		SessionID:            sess.ID,
		ResumeURL:            s.cfg.GatewayWSURL(),
		ResumeGatewayURL:     s.cfg.GatewayWSURL(),
		PrivateChannels:      privateChannels,
		Users:                []any{},
		Presences:            []any{},
		Relationships:        []any{},
		Sessions:             []any{},
		GeoOrderedRTCRegions: []any{},
		SessionType:      "normal",
		UserSettings:     s.userSettings(user.ID),
		Experiments:          []any{},
		GuildExperiments:     []any{},
		UserGuildSettings: &VersionedArray{
			Entries: []any{},
			Partial: false,
			Version: 0,
		},
		ReadState: &VersionedArray{
			Entries: []any{},
			Partial: false,
			Version: 0,
		},
		UserSettingsProto: "",
		ConnectedAccounts: []any{},
		GuildJoinRequests: []any{},
		Consents:          map[string]any{},
		AnalyticsToken:    "",
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
