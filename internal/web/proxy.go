package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/mirror"
)

// cdnProxy serves client assets from the configured upstream mirror through
// this instance's own origin. This is an OPT-IN mode (client.proxy_cdn):
// it removes browser CORS issues, but the instance then distributes
// Discord-owned assets itself, so it must only run on instances that are not
// reachable from the public internet.
//
// Successful responses are cached on disk indefinitely; a build is immutable.
// For Wayback upstreams, files missing from the primary snapshot are
// recovered from any other snapshot via the CDX API.
type cdnProxy struct {
	base     string
	cacheDir string
	client   *http.Client
	log      *slog.Logger
}

func newCDNProxy(cfg *config.Config, log *slog.Logger) *cdnProxy {
	sum := sha256.Sum256([]byte(cfg.Client.CdnBase))
	return &cdnProxy{
		base:     strings.TrimSuffix(cfg.Client.CdnBase, "/"),
		cacheDir: filepath.Join(cfg.Storage.Path, "cdn-cache", hex.EncodeToString(sum[:6])),
		client:   &http.Client{Timeout: 120 * time.Second},
		log:      log,
	}
}

var contentTypes = map[string]string{
	".js":    "application/javascript",
	".css":   "text/css",
	".html":  "text/html",
	".htm":   "text/html",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".svg":   "image/svg+xml",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".json":  "application/json",
	".map":   "application/json",
	".txt":   "text/plain; charset=utf-8",
}

func (p *cdnProxy) cachePath(rel string) (string, error) {
	clean := path.Clean("/" + rel)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid path")
	}
	return filepath.Join(p.cacheDir, filepath.FromSlash(clean)), nil
}

func (p *cdnProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	cp, err := p.cachePath(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if b, err := os.ReadFile(cp); err == nil {
		p.serve(w, r, rel, b)
		return
	}

	body, status := p.fetchUpstream(r.Context(), rel)
	if status != 0 {
		// Pass 404/429/5xx through: the loader has its own retry and
		// variant logic keyed on these statuses.
		http.Error(w, fmt.Sprintf("upstream %d", status), status)
		return
	}
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err == nil {
		_ = os.WriteFile(cp, body, 0o644)
	}
	p.serve(w, r, rel, body)
}

// fetchUpstream tries, in order: the plain path, the path without the
// /assets/ prefix (mirror layout differences), and — for Wayback upstreams —
// any other snapshot via the CDX API. A non-zero status means give up.
func (p *cdnProxy) fetchUpstream(ctx context.Context, rel string) ([]byte, int) {
	urls := []string{p.base + "/" + strings.TrimPrefix(rel, "/")}
	if strings.HasPrefix(rel, "/assets/") || strings.HasPrefix(rel, "assets/") {
		urls = append(urls, p.base+"/"+strings.TrimPrefix(rel, "/assets/"))
	}
	var lastStatus int
	for _, u := range urls {
		body, status, err := p.get(ctx, u)
		if err == nil && status == 0 {
			return body, 0
		}
		if err == nil {
			lastStatus = status
		}
		if status != http.StatusNotFound {
			// Network error or rate limit: no point trying more variants.
			if status != 0 {
				return nil, status
			}
			continue
		}
	}

	if original, ok := mirror.SplitWayback(p.base); ok {
		if target, err := mirror.CDXLookup(ctx, p.client, original+"/"+strings.TrimPrefix(rel, "/")); err == nil {
			if body, status, err := p.get(ctx, target); err == nil && status == 0 {
				p.log.Info("cdn proxy: rescued via cdx", "path", rel)
				return body, 0
			}
		}
	}
	return nil, lastStatus
}

// get returns (body, 0, nil) on 200; (nil, status, nil) for other HTTP
// statuses; (nil, 0, err) on transport errors.
func (p *cdnProxy) get(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "voidbar-loader/0.1")
	res, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			return nil, res.StatusCode, nil
		}
		return nil, res.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, 0, nil
}

func (p *cdnProxy) serve(w http.ResponseWriter, r *http.Request, rel string, body []byte) {
	ct, ok := contentTypes[strings.ToLower(path.Ext(rel))]
	if !ok {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
