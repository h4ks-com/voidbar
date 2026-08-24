package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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

	mu       sync.RWMutex
	sessions map[string]*Session
	byUser   map[string]map[string]*Session
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
			if _, err := old.dispatch("RESUMED", nil, false); err != nil {
				s.log.Error("resumed failed", "err", err)
				return
			}
			sess = old
		case OpHeartbeat:
			ack, _ := json.Marshal(opFrame{Op: OpHeartbeatACK})
			ch <- ack
		case OpPresenceUpdate, OpVoiceStateUpdate, OpRequestGuildMembers:
			if sess == nil {
				s.closeWS(conn, CloseNotAuthenticated, "not authenticated")
				return
			}
		default:
			s.closeWS(conn, CloseUnknownOpcode, "unknown opcode")
			return
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

func (s *Server) buildReady(sess *Session, user *storage.User) *ReadyData {
	guilds := []guildUnavailable{}
	if s.guildsForUser != nil {
		if raw, err := s.guildsForUser(user.ID); err == nil {
			for _, g := range raw {
				if m, ok := g.(map[string]any); ok {
					// READY always carries guilds as unavailable stubs; the
					// real data arrives via GUILD_CREATE after READY.
					guilds = append(guilds, guildUnavailable{ID: fmt.Sprintf("%v", m["id"]), Unavailable: true})
				}
			}
		}
	}
return &ReadyData{
		V:                   9,
		User:                model.ToUser(user),
		Guilds:              guilds,
		SessionID:           sess.ID,
		ResumeURL:           s.cfg.GatewayWSURL(),
		ResumeGatewayURL:    s.cfg.GatewayWSURL(),
		PrivateChannels:     []any{},
		Users:               []any{},
		Presences:           []any{},
		Relationships:       []any{},
		Sessions:            []any{},
		GeoOrderedRTCRegions: []any{},
		SessionType:         "normal",
		UserSettings:        map[string]any{},
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}
