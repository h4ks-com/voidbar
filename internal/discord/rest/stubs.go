package rest

import (
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// registerStubs adds no-op endpoints that Discord clients hit during boot but
// that carry no meaning for Voidbar. They exist so a frozen client build can
// finish loading; anything not covered here lands in handleUnknown and gets
// logged for contract-by-client implementation.
func (s *Server) registerStubs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v9/experiments", s.handleExperiments)
	mux.HandleFunc("POST /api/v9/science", s.handleNoContent)
	mux.HandleFunc("POST /api/v9/metrics", s.handleNoContent)
	mux.HandleFunc("POST /api/v9/flurgergson", s.handleNoContent)
	mux.HandleFunc("PUT /api/v9/fingerprint/whitelist", s.handleNoContent)
	mux.HandleFunc("GET /api/v9/auth/location-metadata", s.handleLocationMetadata)
	mux.HandleFunc("GET /api/v9/users/@me/billing/user-trial-offer", s.requireAuth(s.handleNull))
	mux.HandleFunc("GET /api/v2/incidents/unresolved.json", s.handleEmptyArrayPublic)
	mux.HandleFunc("GET /api/v2/scheduled-maintenances/active.json", s.handleEmptyArrayPublic)
	mux.HandleFunc("GET /api/v9/gateway/bot", s.handleGatewayBot)
	mux.HandleFunc("GET /api/v9/applications/detectable", s.handleEmptyArrayPublic)
	mux.HandleFunc("GET /api/v9/users/@me/connections", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/entitlements", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/billing/subscriptions", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/relationships", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/notes", s.requireAuth(s.handleEmptyMap))
	mux.HandleFunc("GET /api/v9/users/@me/affinities/users", s.requireAuth(s.handleAffinities))
}

// handleLocationMetadata answers the login screen's geo/consent probe with
// safe defaults: no GDPR consent wall, no promotions.
func (s *Server) handleLocationMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"consent_required":        false,
		"country_code":            "US",
		"promotional_body":        nil,
		"promo_flags":             0,
		"samsung_client_id":       nil,
		"samsung_sdk_initialized": false,
	})
}

// registerUnknown installs the catch-all handler last so every unmatched
// request is logged with a greppable marker instead of a silent 404.
func (s *Server) registerUnknown(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleUnknown)
}

func (s *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"guild_id":    nil,
		"hash":        "voidbar",
		"assignments": []any{},
	})
}

func (s *Server) handleNoContent(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGatewayBot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"url":    s.cfg.GatewayWSURL(),
		"shards": 1,
		"session_start_limit": map[string]any{
			"total":           1000,
			"remaining":       1000,
			"reset_after":     0,
			"max_concurrency": 1,
		},
	})
}

func (s *Server) handleEmptyArrayPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// handleNull answers 200 with JSON null - Discord's "no trial offer" reply.
func (s *Server) handleNull(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, nil)
}

// remoteAuthStub accepts the QR-login websocket and closes it immediately.
// Voidbar has no QR login; a clean close stops the client's reconnect loop
// and the login screen falls back to the email/password form.
func remoteAuthStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "remote auth not supported"),
			time.Now().Add(5*time.Second),
		)
		_ = conn.Close()
	})
}

func (s *Server) handleEmptyArray(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, []any{})
}

func (s *Server) handleEmptyMap(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleAffinities(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user_affinities":         []any{},
		"inverse_user_affinities": []any{},
	})
}

func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	hasAuth := r.Header.Get("Authorization") != ""
	s.log.Warn("unknown_path", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery, "auth", hasAuth)
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "404: Not Found", "code": 0})
}
