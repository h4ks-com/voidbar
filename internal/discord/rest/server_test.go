package rest

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func newServer(t *testing.T, registration string) http.Handler {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), registration)
	gw := gateway.New(svc, cfg, logger, nil)
	manager := ircmanage.New(store, gw, logger)
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager)
	gw = gateway.New(svc, cfg, logger, netSvc.GuildsForUser)
	return New(svc, cfg, logger, gw, netSvc, manager)
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

func TestRegisterInviteMode(t *testing.T) {
	h := newServer(t, "invite")
	rec, out := do(t, h, "POST", "/api/v9/auth/register", "", map[string]string{
		"username":    "doesnm",
		"email":       "a@x.io",
		"password":    "hunter2hunter2",
		"invite_code": "nope",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d %v", rec.Code, out)
	}
}
