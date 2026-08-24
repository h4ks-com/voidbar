package rest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
)

const Version = "0.1.0"

type Server struct {
	auth *auth.Service
	cfg  *config.Config
	log  *slog.Logger
	net  *network.Service
	irc  *ircmanage.Manager
	gw   *gateway.Server
}

type ctxKey struct{ name string }

var userCtxKey = ctxKey{"user"}

func New(a *auth.Service, cfg *config.Config, log *slog.Logger, gatewayWS *gateway.Server, net *network.Service, irc *ircmanage.Manager) http.Handler {
	s := &Server{auth: a, cfg: cfg, log: log, net: net, irc: irc, gw: gatewayWS}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/v9/gateway", s.handleGateway)
	if gatewayWS != nil {
		mux.Handle("GET /gateway", gatewayWS)
		// Clients connect to GATEWAY_ENDPOINT+"/?v=..&encoding=json",
		// which lands on the trailing-slash path.
		mux.Handle("GET /gateway/", gatewayWS)
		mux.Handle("GET /remote-auth", remoteAuthStub())
		mux.Handle("GET /remote-auth/", remoteAuthStub())
	}
	mux.HandleFunc("POST /api/v9/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v9/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v9/auth/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/v9/users/@me", s.requireAuth(s.handleMe))
	mux.HandleFunc("GET /api/v9/users/@me/settings", s.requireAuth(s.handleUserSettings))
	mux.HandleFunc("GET /api/v9/users/@me/guilds", s.requireAuth(s.handleUserGuilds))
	mux.HandleFunc("GET /api/v9/users/@me/channels", s.requireAuth(s.handleUserChannels))
	mux.HandleFunc("POST /api/v9/guilds", s.requireAuth(s.handleCreateGuild))
	mux.HandleFunc("GET /api/v9/invites/{code}", s.requireAuth(s.handleGetInvite))
	mux.HandleFunc("POST /api/v9/invites/{code}", s.requireAuth(s.handleJoinInvite))
	mux.HandleFunc("GET /api/v9/channels/{channel}/messages", s.requireAuth(s.handleGetMessages))
	mux.HandleFunc("POST /api/v9/channels/{channel}/messages", s.requireAuth(s.handleSendMessage))
	mux.HandleFunc("GET /api/v9/channels/{channel}/pins", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("POST /api/v9/channels/{channel}/messages/{message}/ack", s.requireAuth(s.handleNoContentAuthed))
	if net != nil {
		mux.HandleFunc("GET /api/v9/guilds/{guild}", s.requireAuth(s.handleGuildDetail))
	}
	s.registerStubs(mux)
	s.registerUnknown(mux)
	return s.withLogging(withCORS(mux))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Debug-Options, X-Super-Properties, X-Fingerprint, X-Context-Properties, X-Discord-Locale")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "dur", time.Since(start).String())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not implement http.Hijacker")
	}
	w.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requireAuth(next func(http.ResponseWriter, *http.Request, *storage.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			s.log.Warn("auth_fail", "path", r.URL.Path, "reason", "no/unknown auth header", "header_prefix", authHeaderPrefix(r))
			unauthorized(w)
			return
		}
		u, err := s.auth.ValidateToken(token)
		if err != nil {
			s.log.Warn("auth_fail", "path", r.URL.Path, "reason", "invalid token", "token_len", len(token))
			unauthorized(w)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)), u)
	}
}

// authHeaderPrefix reports the scheme part of the Authorization header for
// diagnostics (never logs the credential itself).
func authHeaderPrefix(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if i := strings.IndexByte(h, ' '); i > 0 {
		return h[:i]
	}
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 {
		switch parts[0] {
		case "Bearer", "Bot":
			return strings.TrimSpace(parts[1])
		}
	}
	// Discord user clients send the raw token with no auth scheme.
	return strings.TrimSpace(h)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_, _ = w.Write([]byte(`{"message":"encoding error"}`))
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func unauthorized(w http.ResponseWriter) {
	jsonError(w, http.StatusUnauthorized, "401: Unauthorized")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
}

func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"url": s.cfg.GatewayWSURL()})
}

type registerRequest struct {
	Username   string `json:"username"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code"`
}

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

type tokenResponse struct {
	Token string `json:"token"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	_, token, err := s.auth.Register(req.Username, req.Email, req.Password, req.InviteCode)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRegistrationClosed):
			jsonError(w, http.StatusForbidden, "Registration is closed")
		case errors.Is(err, auth.ErrInvalidInvite):
			jsonError(w, http.StatusForbidden, "Invalid invite code")
		case errors.Is(err, auth.ErrInvalidUsername),
			errors.Is(err, auth.ErrInvalidEmail),
			errors.Is(err, auth.ErrInvalidPassword):
			jsonError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, storage.ErrUsernameTaken):
			jsonError(w, http.StatusConflict, "Username is already taken")
		case errors.Is(err, storage.ErrEmailTaken):
			jsonError(w, http.StatusConflict, "Email is already registered")
		default:
			jsonError(w, http.StatusInternalServerError, "Internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{Token: token})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	_, token, err := s.auth.Login(req.Login, req.Password)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{Token: token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, u *storage.User) {
	token := bearerToken(r)
	if err := s.auth.Logout(token); err != nil {
		jsonError(w, http.StatusInternalServerError, "Logout failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u *storage.User) {
	writeJSON(w, http.StatusOK, model.ToUser(u))
}

func (s *Server) handleUserSettings(w http.ResponseWriter, r *http.Request, u *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleUserGuilds(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net != nil {
		guilds, err := s.net.GuildsForUser(u.ID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to list guilds")
			return
		}
		writeJSON(w, http.StatusOK, guilds)
		return
	}
	writeJSON(w, http.StatusOK, []any{})
}

func (s *Server) handleUserChannels(w http.ResponseWriter, r *http.Request, u *storage.User) {
	writeJSON(w, http.StatusOK, []any{})
}
