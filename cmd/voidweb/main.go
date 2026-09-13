// Command voidweb serves the scraped Discord web client against a
// voidbar bouncer. Assets come from a local cache synced out of the
// discord-scraping archive (git history of that repo holds every
// build ever published, so the client can be pinned to an old,
// bouncer-friendly version), /api is reverse-proxied to the bouncer,
// and the boot config (window.GLOBAL_ENV in index.html) is rewritten
// to point at the bouncer. No client code is patched, so the bundles'
// SRI integrity hashes stay valid.
//
// The client connects to the gateway directly (wss to the bouncer
// host) and only REST flows through this server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

func main() {
	bouncer := flag.String("bouncer", "https://vb.doesnmlab.xyz", "bouncer base URL")
	listen := flag.String("listen", "127.0.0.1:8090", "listen address")
	channel := flag.String("channel", "stable", "discord-scraping release channel branch")
	at := flag.String("at", "2021-12-01", "pin the client build to the last scrape commit on or before this date (YYYY-MM-DD; empty tracks the branch head)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	base, err := url.Parse(*bouncer)
	if err != nil || base.Scheme == "" || base.Host == "" {
		logger.Error("bad bouncer URL", "bouncer", *bouncer)
		os.Exit(1)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		logger.Error("cache dir", "err", err)
		os.Exit(1)
	}
	cacheDir := filepath.Join(cache, "voidweb")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sha, err := syncAssets(ctx, logger, cacheDir, *channel, *at)
	if err != nil {
		logger.Error("asset sync", "err", err)
		os.Exit(1)
	}
	build, hash := readManifest(cacheDir)
	logger.Info("client synced", "commit", sha, "build", build, "version_hash", hash)

	index := rewriteIndex(cacheDir, base.Host)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = base.Host
			pr.SetXForwarded()
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/assets/", &assetHandler{dir: filepath.Join(cacheDir, "assets")})
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pinned client may speak an older API version (v8) while
		// the bouncer only routes v9; the shapes it needs are the same.
		r.URL.Path = apiVersion.ReplaceAllString(r.URL.Path, "/api/v9/")
		proxy.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/cdn-cgi/", func(w http.ResponseWriter, r *http.Request) {
		// The scraped index still carries Discord's Cloudflare probe.
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(index)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("voidweb listening", "addr", *listen, "bouncer", base.Host)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}

var apiVersion = regexp.MustCompile(`^/api/v[0-9]+/`)

// logRequests mirrors the bouncer's http access log format so client
// debugging flows the same way on both sides.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"dur", time.Since(start).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// rewriteIndex rewrites the GLOBAL_ENV boot config inside the scraped
// index.html: API same-origin (proxied), gateway and CDN straight at
// the bouncer host (the bouncer serves /gateway and its own /avatars
// CDN surface), webapp/assets same-origin. Keyed by config key so the
// exact original values (which drifted across Discord builds) do not
// matter.
func rewriteIndex(cacheDir, bouncerHost string) []byte {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "index.html"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "voidweb: read index.html: %v\n", err)
		os.Exit(1)
	}
	replacements := []struct{ key, value string }{
		{"API_ENDPOINT", "/api"},
		{"GATEWAY_ENDPOINT", "wss://" + bouncerHost + "/gateway"},
		{"WEBAPP_ENDPOINT", ""},
		{"CDN_HOST", bouncerHost},
		{"ASSET_ENDPOINT", ""},
		{"WIDGET_ENDPOINT", ""},
		{"DEVELOPERS_ENDPOINT", ""},
		{"MARKETING_ENDPOINT", ""},
		{"REMOTE_AUTH_ENDPOINT", ""},
		{"NETWORKING_ENDPOINT", ""},
	}
	page := string(raw)
	for _, rep := range replacements {
		pattern := regexp.MustCompile(`(` + rep.key + `:\s*)('[^']*'|"[^"]*")`)
		if !pattern.MatchString(page) {
			continue
		}
		page = pattern.ReplaceAllString(page, `${1}'`+rep.value+`'`)
	}
	return []byte(page)
}

// readManifest pulls the build number and version hash out of the
// scrape manifest for startup logging.
func readManifest(cacheDir string) (build, hash string) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "manifest.json"))
	if err != nil {
		return "?", "?"
	}
	var m struct {
		Build int    `json:"build"`
		Hash  string `json:"versionHash"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "?", "?"
	}
	return strconv.Itoa(m.Build), m.Hash
}
