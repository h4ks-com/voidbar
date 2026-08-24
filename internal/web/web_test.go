package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
)

func TestConfigEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = "https://archive.org/download/voidbar-client/"
	cfg.Client.Build = "january_15_2022"
	cfg.Server.PublicURL = "https://voidbar.example.com"

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/voidbar/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out clientConfig
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Gateway != "wss://voidbar.example.com/gateway" {
		t.Fatalf("gateway: %q", out.Gateway)
	}
	if out.CdnBase != "https://archive.org/download/voidbar-client" {
		t.Fatalf("cdn_base: %q", out.CdnBase)
	}
	if out.ProxyBase != "" {
		t.Fatalf("proxy_base should be empty when disabled, got %q", out.ProxyBase)
	}
	if out.Build != "january_15_2022" {
		t.Fatalf("build: %q", out.Build)
	}
}

func TestConfigEndpointDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = false

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/voidbar/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", res.StatusCode)
	}
}

func TestStaticFiles(t *testing.T) {
	cfg := config.Default()
	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	cases := []struct {
		path, contentType, marker string
	}{
		{"/", "text/html", "voidbar-loading"},
		{"/loader.js", "application/javascript", "GLOBAL_ENV"},
		{"/loading.css", "text/css", "voidbar-loading-bar"},
	}
	for _, tc := range cases {
		res, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, res.StatusCode)
		}
		if got := res.Header.Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%s: content-type %q", tc.path, got)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), tc.marker) {
			t.Fatalf("%s: marker %q not found", tc.path, tc.marker)
		}
	}
}

func TestConfigEndpointWithProxy(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = "https://web.archive.org/web/20220601000000id_/https://discord.com"
	cfg.Client.ProxyCDN = true

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/voidbar/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out clientConfig
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ProxyBase != ProxyBasePath {
		t.Fatalf("proxy_base: %q", out.ProxyBase)
	}
}

func TestProxyPrefersMirrorDir(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("from-upstream"))
	}))
	defer upstream.Close()

	mirror := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mirror, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Wayback archives chunks under bare hash names; the client asks for
	// id-prefixed names.
	if err := os.WriteFile(filepath.Join(mirror, "assets", "app.js"), []byte("from-mirror"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "assets", "070bd796afd556fd6d8e.js"), []byte("chunk-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = upstream.URL
	cfg.Client.ProxyCDN = true
	cfg.Client.MirrorDir = mirror
	cfg.Storage.Path = t.TempDir()

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	for _, path := range []string{ProxyBasePath + "/assets/app.js", "/assets/app.js"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
		if string(body) != "from-mirror" {
			t.Fatalf("%s: body %q", path, body)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hit %d times, expected 0 (mirror_dir must win)", hits.Load())
	}

	// Id-prefixed request resolves to the bare-hash file from the mirror.
	res, err := http.Get(srv.URL + ProxyBasePath + "/assets/906.070bd796afd556fd6d8e.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "chunk-content" {
		t.Fatalf("id-strip fallback: %d %q", res.StatusCode, body)
	}

	// A file missing from the mirror 404s fast: with a local mirror_dir the
	// proxy never falls back to the network (the mirror already encodes
	// whatever the discovery process could find).
	res, err = http.Get(srv.URL + ProxyBasePath + "/assets/other.js")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("mirror miss should 404, got %d", res.StatusCode)
	}
}

func TestProxyServesAndCaches(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/assets/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte("console.log(1)"))
		case "/assets/missing.js":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = upstream.URL
	cfg.Client.ProxyCDN = true
	cfg.Storage.Path = t.TempDir()

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	get := func(path string) *http.Response {
		res, err := http.Get(srv.URL + ProxyBasePath + path)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := get("/assets/app.js")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("app.js status: %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/javascript" {
		t.Fatalf("content-type: %q", ct)
	}
	if string(body) != "console.log(1)" {
		t.Fatalf("body: %q", body)
	}
	res = get("/assets/app.js")
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cached app.js status: %d", res.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected upstream to be hit exactly once, got %d", hits.Load())
	}

	// Cache must live on disk.
	var cached int
	filepath.Walk(cfg.Storage.Path, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".js") {
			cached++
		}
		return nil
	})
	if cached != 1 {
		t.Fatalf("expected 1 cached file, found %d", cached)
	}

	res = get("/assets/missing.js")
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing.js status: %d", res.StatusCode)
	}
}

func TestProxyDisabledByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = "https://example.invalid"

	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	res, err := http.Get(srv.URL + ProxyBasePath + "/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Without proxy enabled the catch-all serves the loader page instead.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "voidbar-loading") {
		t.Fatalf("expected loader html, got %q", string(body)[:min(80, len(body))])
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
