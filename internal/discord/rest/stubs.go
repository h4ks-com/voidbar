package rest

import (
	"encoding/json"
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
	mux.HandleFunc("GET /api/v9/guild-recommendations", s.requireAuth(s.handleGuildRecommendations))
	mux.HandleFunc("GET /api/v9/discoverable-guilds", s.requireAuth(s.handleDiscoverableGuilds))
	mux.HandleFunc("GET /api/v9/discovery/categories", s.handleDiscoveryCategories)
	mux.HandleFunc("GET /api/v9/experiments", s.handleExperiments)
	mux.HandleFunc("POST /api/v9/science", s.handleNoContent)
	mux.HandleFunc("POST /api/v9/metrics", s.handleNoContent)
	mux.HandleFunc("POST /api/v9/flurgergson", s.handleNoContent)
	mux.HandleFunc("PUT /api/v9/fingerprint/whitelist", s.handleNoContent)
	mux.HandleFunc("GET /api/v9/auth/location-metadata", s.handleLocationMetadata)
	// The Android client POSTs this before every login (fraud check) and
	// treats any non-2xx as a network failure ("unknown network error"),
	// aborting the whole login flow. The value is opaque to us.
	mux.HandleFunc("POST /api/v9/auth/fingerprint", s.handleAuthFingerprint)
	mux.HandleFunc("PATCH /api/v9/users/@me/settings-proto/1", s.requireAuth(s.handleSettingsProtoPatch))
	mux.HandleFunc("GET /api/v9/users/@me/settings-proto/1", s.requireAuth(s.handleSettingsProtoGet))
	// The legacy (non-proto) settings store: the client PATCHes e.g. the
	// last opened channel on navigation and dismissed promos. Persisted so
	// onboarding popovers ("account switcher" promo, guide) stay dismissed
	// across sessions instead of re-appearing over the guild rail.
	// Billing: the client probes payment sources/country after joining a
	// guild (gift/boost upsells). No billing on a bouncer.
	mux.HandleFunc("GET /api/v9/users/@me/billing/payment-sources", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/billing/country-code", s.requireAuth(func(w http.ResponseWriter, r *http.Request, u *storage.User) {
		writeJSON(w, http.StatusOK, map[string]any{"country_code": "US"})
	}))
	mux.HandleFunc("PATCH /api/v9/users/@me/settings", s.requireAuth(s.handleSettingsPatch))
	mux.HandleFunc("GET /api/v9/users/@me/affinities/guilds", s.requireAuth(s.handleGuildAffinities))
	mux.HandleFunc("GET /api/v9/users/@me/library", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/billing/user-trial-offer", s.requireAuth(s.handleNull))
	mux.HandleFunc("GET /api/v2/incidents/unresolved.json", s.handleStatuspageJSON)
	mux.HandleFunc("GET /api/v2/scheduled-maintenances/active.json", s.handleStatuspageJSON)
	mux.HandleFunc("GET /api/v2/scheduled-maintenances/upcoming.json", s.handleStatuspageJSON)
	mux.HandleFunc("GET /api/v9/gateway/bot", s.handleGatewayBot)
	mux.HandleFunc("GET /api/v9/applications/detectable", s.handleEmptyArrayPublic)
	mux.HandleFunc("GET /api/v9/users/@me/connections", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/entitlements", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/billing/subscriptions", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/relationships", s.requireAuth(s.handleEmptyArray))
	mux.HandleFunc("GET /api/v9/users/@me/notes", s.requireAuth(s.handleEmptyMap))
	mux.HandleFunc("GET /api/v9/users/@me/affinities/users", s.requireAuth(s.handleAffinities))
	// Android client probes after login; 404s leave stores in retry loops
	// (and the profile screen spins). All are "nothing to show" shapes.
	mux.HandleFunc("GET /api/v9/users/{id}/profile", s.requireAuth(s.handleUserProfile))
	mux.HandleFunc("GET /api/v9/users/@me/survey", s.requireAuth(s.handleNull))
	mux.HandleFunc("POST /api/v9/users/@me/devices", s.requireAuth(s.handleNoContentAuthed))
}

// handleAuthFingerprint answers the Android client's pre-login fingerprint
// probe. Format per userdocs (legacy, unauthenticated): a snowflake and a
// hashed value, "123456789012345678.ABCdef...". Persisting experiments
// across the auth flow is not implemented; a fresh valid-looking value per
// call is enough for the client to proceed with login.
func (s *Server) handleAuthFingerprint(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	writeJSON(w, http.StatusOK, map[string]any{
		"fingerprint": s.auth.Fingerprint(),
	})
}

// handleUserProfile answers the Android profile probe. The client renders
// the user sheet from this; a minimal profile with a connected_account-free
// body keeps it calm.
func (s *Server) handleUserProfile(w http.ResponseWriter, r *http.Request, u *storage.User) {
	id := r.PathValue("id")
	username := "user"
	if id == u.ID {
		username = u.Username
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                  id,
		"username":            username,
		"discriminator":       "0",
		"avatar":              nil,
		"bot":                 false,
		"public_flags":        0,
		"flags":               0,
		"premium_type":        0,
		"bio":                 "",
		"connected_accounts":  []any{},
		"mutual_guilds":       []any{},
		"mutual_friends_count": 0,
		"user_profile": map[string]any{
			"bio": "",
		},
	})
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

// handleNoContentAuthed acknowledges client-driven writes (settings sync
// etc.) without storing anything.
func (s *Server) handleNoContentAuthed(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusNoContent)
}

// handleSettingsPatch merges the legacy settings PATCH body into storage
// and acknowledges with 204 (Discord's contract for this endpoint).
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request, u *storage.User) {
	patch := map[string]any{}
	// UseNumber: the Android client PATCHes guild_folders snowflakes as
	// JSON numbers; float64 parsing rounds them past 2^53 and the exact
	// ids become unrecoverable, desyncing the client's settings store.
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&patch); err != nil {
		// Unreadable body: still acknowledge; the client treats 204 as
		// "synced" and won't retry-spam.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.net != nil {
		if err := s.net.MergeUserSettings(u.ID, patch); err != nil {
			s.log.Warn("settings persist failed", "err", err, "user", u.ID)
		}
	}
	// Presence picker: the status lands here among the settings keys.
	// Relay to every IRC connection (online/idle/dnd map onto AWAY;
	// invisible is silently dropped upstream - see Manager.applyStatus).
	if status, ok := patch["status"].(string); ok && s.irc != nil {
		s.irc.SetStatus(u.ID, status)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGuildRecommendations feeds the recommendations store. The client's
// store comparator shallow-compares recommendationGuilds on every guild
// store change - a 404 here leaves it undefined and crashes Object.keys.
func (s *Server) handleGuildRecommendations(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"recommended_guilds": []any{},
		"load_id":            "voidbar",
	})
}

// handleDiscoverableGuilds answers the discovery listing with an empty page.
func (s *Server) handleDiscoverableGuilds(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"guilds":     []any{},
		"total":      0,
		"offset":     0,
		"limit":      0,
		"categories": []any{},
		"keywords":   []any{},
		"load_id":    "voidbar",
	})
}

// handleDiscoveryCategories answers the discovery categories probe.
func (s *Server) handleDiscoveryCategories(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": []any{},
		"load_id":    "voidbar",
	})
}

// handleStatuspageJSON answers statuspage.io-shaped endpoints. The client
// destructures the named array fields, so both keys must always be present
// (a bare [] crashes its destructuring).
func (s *Server) handleStatuspageJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"incidents":              []any{},
		"scheduled_maintenances": []any{},
	})
}

// handleSettingsProtoPatch acknowledges settings persistence. The client
// reads body.out_of_date and body.settings from the response - a bare 204
// made it parse null and crash.
func (s *Server) handleSettingsProtoPatch(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	_, _ = io.Copy(io.Discard, r.Body)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":    "",
		"out_of_date": false,
	})
}

// handleSettingsProtoGet answers the client's loadIfNecessary probe with an
// empty serialized PreloadedUserSettings (all defaults).
func (s *Server) handleSettingsProtoGet(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":              "",
		"required_data_version": nil,
	})
}

// handleGuildAffinities mirrors the user-affinities shape for guilds.
func (s *Server) handleGuildAffinities(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"guild_affinities":         []any{},
		"inverse_guild_affinities": []any{},
	})
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
