package rest

import (
	"io"
	"net/http"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// registerStubs adds no-op endpoints that Discord clients hit during boot but
// that carry no meaning for Voidbar. They exist so a frozen client build can
// finish loading; anything not covered here lands in handleUnknown and gets
// logged for contract-by-client implementation.
func (s *Server) registerStubs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v9/experiments", s.handleExperiments)
	mux.HandleFunc("POST /api/v9/science", s.handleNoContent)
	mux.HandleFunc("POST /api/v9/flurgergson", s.handleNoContent)
	mux.HandleFunc("PUT /api/v9/fingerprint/whitelist", s.handleNoContent)
	mux.HandleFunc("GET /api/v9/gateway/bot", s.handleGatewayBot)
	mux.HandleFunc("GET /api/v9/applications/detectable", s.handleEmptyArrayPublic)
	mux.HandleFunc("GET /api/v9/users/@me/connections", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/entitlements", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/billing/subscriptions", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/affinities/users", s.requireAuth(s.handleAffinities))
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

func (s *Server) handleEmptyArray(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, []any{})
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
