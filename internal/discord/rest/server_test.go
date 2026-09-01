package rest

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func newServer(t *testing.T, registration string) http.Handler {
	t.Helper()
	_, h := newServerWithStore(t, registration)
	return h
}

// newServerWithStore also returns the backing store so tests can seed
// replay-buffer state directly.
func newServerWithStore(t *testing.T, registration string) (*storage.Storage, http.Handler) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), registration)
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	gw.SetGuildProviders(
		func(u string) ([]any, error) { return netSvc.ReadyGuildPayloads(u), nil },
		netSvc.GuildCreateForUser,
	)
	return store, New(svc, cfg, logger, gw, netSvc, manager)
}

func do(t *testing.T, h http.Handler, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, out := doAny(t, h, method, path, token, body)
	if out == nil {
		return rec, map[string]any{}
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected object response, got %T", out)
	}
	return rec, m
}

func doAny(t *testing.T, h http.Handler, method, path, token string, body any) (*httptest.ResponseRecorder, any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.Len() == 0 {
		return rec, nil
	}
	var out any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("non-json response: %q", rec.Body.String())
	}
	return rec, out
}

func registerAndLogin(t *testing.T, h http.Handler) string {
	t.Helper()
	rec, out := do(t, h, "POST", "/api/v9/auth/register", "", map[string]string{
		"username": "doesnm",
		"email":    "doesnm@0ut0f.space",
		"password": "hunter2hunter2",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %v", rec.Code, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatal("no token in register response")
	}
	return token
}

func TestHealth(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/health", "", nil)
	if rec.Code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("health: %d %v", rec.Code, out)
	}
}

func TestGatewayURL(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/api/v9/gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway: %d", rec.Code)
	}
	url, _ := out["url"].(string)
	if !strings.HasPrefix(url, "ws://") || !strings.HasSuffix(url, "/gateway") {
		t.Fatalf("gateway url: %q", url)
	}
}

func TestRegisterLoginMeFlow(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	rec, out := do(t, h, "GET", "/api/v9/users/@me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("@me: %d %v", rec.Code, out)
	}
	if out["username"] != "doesnm" || out["id"] == nil {
		t.Fatalf("@me body: %v", out)
	}
	if _, has := out["discriminator"]; !has {
		t.Fatal("missing discriminator")
	}

	rec, _ = do(t, h, "GET", "/api/v9/users/@me", "badtoken", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	rec, out = do(t, h, "POST", "/api/v9/auth/login", "", map[string]string{
		"login":    "doesnm",
		"password": "hunter2hunter2",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %v", rec.Code, out)
	}
	token2, _ := out["token"].(string)

	rec, _ = do(t, h, "POST", "/api/v9/auth/login", "", map[string]string{
		"login":    "doesnm",
		"password": "wrong-password",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", rec.Code)
	}

	rec, _ = do(t, h, "POST", "/api/v9/auth/logout", token2, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	rec, _ = do(t, h, "GET", "/api/v9/users/@me", token2, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token should be dead after logout: %d", rec.Code)
	}
}

func TestRawTokenAuthorization(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	// Discord user clients send the raw token with no auth scheme.
	req := httptest.NewRequest("GET", "/api/v9/users/@me", nil)
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw token: %d", rec.Code)
	}

	// Prefixed forms keep working.
	for _, form := range []string{"Bearer " + token, "Bot " + token} {
		req := httptest.NewRequest("GET", "/api/v9/users/@me", nil)
		req.Header.Set("Authorization", form)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: %d", form, rec.Code)
		}
	}
}

func TestUserMetadataEndpoints(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)
	for _, tc := range []struct {
		path    string
		isArray bool
	}{
		{"/api/v9/users/@me/settings", false},
		{"/api/v9/users/@me/guilds", true},
		{"/api/v9/users/@me/channels", true},
	} {
		rec, _ := doAny(t, h, "GET", tc.path, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", tc.path, rec.Code)
		}
		trimmed := strings.TrimSpace(rec.Body.String())
		if tc.isArray && !strings.HasPrefix(trimmed, "[") {
			t.Fatalf("%s: expected array, got %q", tc.path, trimmed)
		}
		if !tc.isArray && !strings.HasPrefix(trimmed, "{") {
			t.Fatalf("%s: expected object, got %q", tc.path, trimmed)
		}
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newServer(t, "open")
	rec, _ := do(t, h, "OPTIONS", "/api/v9/users/@me", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing CORS header")
	}
}

func TestRegisterClosedMode(t *testing.T) {
	h := newServer(t, "closed")
	rec, out := do(t, h, "POST", "/api/v9/auth/register", "", map[string]string{
		"username": "doesnm",
		"email":    "a@x.io",
		"password": "hunter2hunter2",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d %v", rec.Code, out)
	}
}

func TestUserNotes(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)
	target := "5998428274284326925"

	rec, _ := do(t, h, "PUT", "/api/v9/users/@me/notes/"+target, token, map[string]string{"note": "knows IRC"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("put note: %d", rec.Code)
	}
	rec, out := do(t, h, "GET", "/api/v9/users/@me/notes/"+target, token, nil)
	if rec.Code != http.StatusOK || out["note"] != "knows IRC" {
		t.Fatalf("get note: %d %v", rec.Code, out)
	}
	if out["note_user_id"] != target || out["user_id"] == "" {
		t.Fatalf("note ids wrong: %v", out)
	}
	rec, list := do(t, h, "GET", "/api/v9/users/@me/notes", token, nil)
	if rec.Code != http.StatusOK || list[target] != "knows IRC" {
		t.Fatalf("list notes: %d %v", rec.Code, list)
	}

	// Oversized note is rejected (documented 256-char limit).
	rec, _ = do(t, h, "PUT", "/api/v9/users/@me/notes/"+target, token, map[string]string{"note": strings.Repeat("x", 257)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized note: %d", rec.Code)
	}

	// An empty note clears it.
	rec, _ = do(t, h, "PUT", "/api/v9/users/@me/notes/"+target, token, map[string]string{"note": ""})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear note: %d", rec.Code)
	}
	rec, out = do(t, h, "GET", "/api/v9/users/@me/notes/"+target, token, nil)
	if rec.Code != http.StatusOK || out["note"] != "" {
		t.Fatalf("cleared note should read empty: %d %v", rec.Code, out)
	}
	rec, list = do(t, h, "GET", "/api/v9/users/@me/notes", token, nil)
	if _, still := list[target]; still {
		t.Fatalf("cleared note still listed: %v", list)
	}
}

func TestRenameGuild(t *testing.T) {
	store, h := newServerWithStore(t, "open")
	token := registerAndLogin(t, h)
	rec, me := do(t, h, "GET", "/api/v9/users/@me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	uid, _ := me["id"].(string)
	// Seed a network the user is a member of.
	net := &storage.Network{ID: "800000000000000000", Name: "OldName", ConnID: "irc.old.example"}
	if err := store.UpsertNetwork(net); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{NetworkID: net.ID, UserID: uid, Nick: "tester", AutoJoin: []string{"#test"}, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rec, out := do(t, h, "PATCH", "/api/v9/guilds/"+net.ID, token, map[string]string{"name": "NewName"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %v", rec.Code, out)
	}
	if out["name"] != "NewName" {
		t.Fatalf("renamed guild payload: %v", out)
	}
	// The rename persists.
	got, err := store.GetNetwork(net.ID)
	if err != nil || got.Name != "NewName" {
		t.Fatalf("persisted name: %v %v", got, err)
	}
	// Empty names are rejected.
	rec, _ = do(t, h, "PATCH", "/api/v9/guilds/"+net.ID, token, map[string]string{"name": "   "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank rename: %d", rec.Code)
	}
	// Unknown guild 404s.
	rec, _ = do(t, h, "PATCH", "/api/v9/guilds/42", token, map[string]string{"name": "X"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown guild rename: %d", rec.Code)
	}
}

func TestReactionReactors(t *testing.T) {
	store, h := newServerWithStore(t, "open")
	token := registerAndLogin(t, h)
	rec, me := do(t, h, "GET", "/api/v9/users/@me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	uid, _ := me["id"].(string)
	channelID := "710000000000000000"

	// Seed a message with a reaction from us and an IRC peer hash.
	peerID := model.IrcAuthorID("irc:bob")
	if err := store.AppendMessage(storage.BufferedMessage{
		ID: "910000000000000000", ChannelID: channelID, Content: "react to me",
		AuthorID: "irc:bob", AuthorName: "bob",
		Timestamp: "2026-01-01T10:00:00Z", Type: 0,
		Reactions: map[string][]string{"👍": {uid, peerID}},
	}); err != nil {
		t.Fatal(err)
	}

	path := "/api/v9/channels/" + channelID + "/messages/910000000000000000/reactions/" + url.PathEscape("👍")
	rec, listAny := doAny(t, h, "GET", path, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reactors: %d", rec.Code)
	}
	list, ok := listAny.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 reactors, got %v", listAny)
	}
	names := map[string]bool{}
	for _, rowAny := range list {
		row, _ := rowAny.(map[string]any)
		if name, _ := row["username"].(string); name != "" {
			names[name] = true
		}
	}
	// The own member resolves to the real username.
	if !names["doesnm"] {
		t.Fatalf("own reactor missing: %v", list)
	}

	// Unknown message 404s.
	rec, _ = do(t, h, "GET", "/api/v9/channels/"+channelID+"/messages/42/reactions/"+url.PathEscape("👍"), token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown message reactors: %d", rec.Code)
	}
}

func TestChannelPins(t *testing.T) {
	store, h := newServerWithStore(t, "open")
	token := registerAndLogin(t, h)
	channelID := "700000000000000000"

	seed := func(id, content string) {
		t.Helper()
		if err := store.AppendMessage(storage.BufferedMessage{
			ID: id, ChannelID: channelID, Content: content,
			AuthorID: "irc:bob", AuthorName: "bob",
			Timestamp: "2026-01-01T10:00:00Z", Type: 0,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("900000000000000000", "pin me")
	seed("900000000000000001", "second")

	pin := func(id string) int {
		rec, _ := do(t, h, "PUT", "/api/v9/channels/"+channelID+"/pins/"+id, token, nil)
		return rec.Code
	}
	if code := pin("900000000000000000"); code != http.StatusNoContent {
		t.Fatalf("pin: %d", code)
	}
	if code := pin("900000000000000001"); code != http.StatusNoContent {
		t.Fatalf("pin second: %d", code)
	}
	// Pinning again is idempotent.
	if code := pin("900000000000000000"); code != http.StatusNoContent {
		t.Fatalf("re-pin: %d", code)
	}
	// Unknown message 404s.
	if code := pin("42"); code != http.StatusNotFound {
		t.Fatalf("pin unknown: %d", code)
	}

	rec, listAny := doAny(t, h, "GET", "/api/v9/channels/"+channelID+"/pins", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pins: %d", rec.Code)
	}
	list, ok := listAny.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 pins, got %v", listAny)
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != "900000000000000000" || first["pinned"] != true || first["content"] != "pin me" {
		t.Fatalf("pin payload wrong: %v", first)
	}

	// The pin drops a type-6 system row into the channel history.
	rec, histAny := doAny(t, h, "GET", "/api/v9/channels/"+channelID+"/messages?limit=50", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history: %d", rec.Code)
	}
	hist, _ := histAny.([]any)
	sysRows := 0
	for _, rowAny := range hist {
		row, _ := rowAny.(map[string]any)
		if row["type"] == float64(6) && row["content"] == "" {
			sysRows++
		}
	}
	if sysRows != 2 { // one per pinned message
		t.Fatalf("expected 2 pin system rows, got %d in %v", sysRows, hist)
	}

	// Unpin one; the list shrinks.
	rec, _ = do(t, h, "DELETE", "/api/v9/channels/"+channelID+"/pins/900000000000000000", token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unpin: %d", rec.Code)
	}
	rec, listAny = doAny(t, h, "GET", "/api/v9/channels/"+channelID+"/pins", token, nil)
	list, _ = listAny.([]any)
	if rec.Code != http.StatusOK || len(list) != 1 {
		t.Fatalf("expected 1 pin after unpin: %d %v", rec.Code, listAny)
	}
}
