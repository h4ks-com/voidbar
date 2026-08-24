package rest

import (
	"net/http"
	"testing"
)

func TestStubExperiments(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/api/v9/experiments", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if assignments, ok := out["assignments"].([]any); !ok || len(assignments) != 0 {
		t.Fatalf("assignments: %v", out["assignments"])
	}
	if _, has := out["hash"]; !has {
		t.Fatal("missing hash")
	}
}

func TestStubNoContentEndpoints(t *testing.T) {
	h := newServer(t, "open")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v9/science"},
		{"POST", "/api/v9/flurgergson"},
		{"PUT", "/api/v9/fingerprint/whitelist"},
	} {
		rec, _ := do(t, h, tc.method, tc.path, "", []any{map[string]any{"event": "test"}})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s: %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestStubGatewayBot(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/api/v9/gateway/bot", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if out["url"] == nil || out["shards"].(float64) != 1 {
		t.Fatalf("body: %v", out)
	}
	ssl, ok := out["session_start_limit"].(map[string]any)
	if !ok || ssl["max_concurrency"].(float64) != 1 {
		t.Fatalf("session_start_limit: %v", out["session_start_limit"])
	}
}

func TestStubEmptyArrayEndpoints(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	rec, out := doAny(t, h, "GET", "/api/v9/applications/detectable", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detectable (public): %d", rec.Code)
	}
	if arr, ok := out.([]any); !ok || len(arr) != 0 {
		t.Fatalf("detectable: %v", out)
	}

	for _, path := range []string{
		"/api/v9/users/@me/connections",
		"/api/v9/users/@me/entitlements",
		"/api/v9/users/@me/billing/subscriptions",
	} {
		rec, out := doAny(t, h, "GET", path, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if arr, ok := out.([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s: %v", path, out)
		}
	}

	rec, _ = do(t, h, "GET", "/api/v9/users/@me/connections", "badtoken", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("connections without auth: %d", rec.Code)
	}
}

func TestStubAffinities(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)
	rec, out := do(t, h, "GET", "/api/v9/users/@me/affinities/users", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	for _, key := range []string{"user_affinities", "inverse_user_affinities"} {
		if arr, ok := out[key].([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s: %v", key, out[key])
		}
	}
}

func TestStubLocationMetadata(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/api/v9/auth/location-metadata", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if out["consent_required"] != false {
		t.Fatalf("consent_required: %v", out["consent_required"])
	}
	if _, has := out["country_code"]; !has {
		t.Fatal("missing country_code")
	}
}

func TestStubMetrics(t *testing.T) {
	h := newServer(t, "open")
	rec, _ := do(t, h, "POST", "/api/v9/metrics", "", []any{map[string]any{"x": 1}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("metrics: %d", rec.Code)
	}
}

func TestUnknownPathIs404JSON(t *testing.T) {
	h := newServer(t, "open")
	rec, out := do(t, h, "GET", "/api/v9/definitely/not/implemented", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rec.Code)
	}
	if out["code"].(float64) != 0 {
		t.Fatalf("body: %v", out)
	}
	rec, _ = do(t, h, "POST", "/api/v9/also/missing", "", map[string]string{"x": "y"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post status: %d", rec.Code)
	}
}
