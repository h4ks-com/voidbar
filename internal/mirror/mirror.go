// Package mirror downloads a frozen Discord web client build from an
// upstream mirror (e.g. the Wayback Machine) into a local directory, so it
// can be uploaded once to a CORS-enabled mirror such as an archive.org item.
//
// The downloader follows the same discovery rules as the browser loader:
//   - stylesheets, scripts and prefetch links referenced by the entry HTML;
//   - webpack chunk candidates ({id}.{hash}.js, {hash}.js, {hash}.css)
//     extracted from downloaded scripts;
//   - assets referenced from CSS/JS via url(/assets/...).
//
// It is resumable: files already present on disk are reused.
package mirror

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	reStylesheet = regexp.MustCompile(`<link[^>]+rel="[^"]*stylesheet[^"]*"[^>]*>`)
	reHref       = regexp.MustCompile(`(?:href|src)="([^"]+)"`)
	reScriptSrc  = regexp.MustCompile(`<script[^>]+src="([^"]+)"[^>]*>`)
	rePrefetch   = regexp.MustCompile(`<link[^>]+as="script"[^>]*>`)
	reChunkMap   = regexp.MustCompile(`[{,]\s*(?:"|')?(\d+)(?:"|')?\s*:\s*(?:"|')([0-9a-zA-Z._-]{10,})(?:"|')`)
	reCSSURL     = regexp.MustCompile(`url\(['"]?(/assets/[^)'"]+)['"]?\)`)
)

// ExtractEntryResources returns the asset paths referenced by the entry HTML.
func ExtractEntryResources(html string) (styles, scripts []string) {
	seen := map[string]bool{}
	add := func(list []string, v string) []string {
		if v == "" || !strings.HasPrefix(v, "/") || seen[v] {
			return list
		}
		seen[v] = true
		return append(list, v)
	}
	for _, tag := range reStylesheet.FindAllString(html, -1) {
		for _, m := range reHref.FindAllStringSubmatch(tag, -1) {
			styles = add(styles, m[1])
		}
	}
	for _, m := range reScriptSrc.FindAllStringSubmatch(html, -1) {
		scripts = add(scripts, m[1])
	}
	for _, tag := range rePrefetch.FindAllString(html, -1) {
		for _, m := range reHref.FindAllStringSubmatch(tag, -1) {
			scripts = add(scripts, m[1])
		}
	}
	return styles, scripts
}

// ExtractChunkVariants maps webpack chunk hashes to candidate paths,
// tried in order.
func ExtractChunkVariants(js string) map[string][]string {
	out := map[string][]string{}
	for _, m := range reChunkMap.FindAllStringSubmatch(js, -1) {
		id, hash := m[1], m[2]
		if _, ok := out[hash]; ok {
			continue
		}
		out[hash] = []string{
			"/assets/" + id + "." + hash + ".js",
			"/assets/" + hash + ".js",
			"/assets/" + hash + ".css",
		}
	}
	return out
}

// ExtractAssetURLs returns url(/assets/...) references from CSS or JS.
func ExtractAssetURLs(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reCSSURL.FindAllStringSubmatch(content, -1) {
		p := m[1]
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

type Options struct {
	Base        string // upstream prefix without trailing slash
	HTML        string // entry path relative to Base, e.g. "app"
	Out         string // output directory
	Concurrency int
	Retries     int
	Log         func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.Retries <= 0 {
		o.Retries = 5
	}
	if o.HTML == "" {
		o.HTML = "app"
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

var errNotFound = errors.New("404")

type downloader struct {
	opts    Options
	client  *http.Client
	tasks   chan func()
	wg      sync.WaitGroup
	mu      sync.Mutex
	missing map[string]bool
	saved   int
}

// Run downloads the build into opts.Out.
func Run(opts Options) error {
	opts.defaults()
	if !strings.HasPrefix(opts.Base, "http://") && !strings.HasPrefix(opts.Base, "https://") {
		return fmt.Errorf("--from must be an http(s) URL")
	}
	if opts.Out == "" {
		return fmt.Errorf("--out is required")
	}
	opts.Base = strings.TrimSuffix(opts.Base, "/")
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return err
	}

	d := &downloader{
		opts:    opts,
		client:  &http.Client{Timeout: 120 * time.Second},
		tasks:   make(chan func(), 64),
		missing: map[string]bool{},
	}

	html, err := d.fetchLocalOrRemote("/" + strings.TrimPrefix(opts.HTML, "/"))
	if err != nil {
		return fmt.Errorf("entry html: %w", err)
	}
	if err := d.save("/"+strings.TrimPrefix(opts.HTML, "/"), html); err != nil {
		return err
	}

	styles, scripts := ExtractEntryResources(string(html))
	opts.Log("entry html references %d styles, %d scripts", len(styles), len(scripts))

	enqueueAsset := func(p, ftype string) {
		d.enqueue(func() {
			content, err := d.fetchLocalOrRemote(p)
			if err != nil {
				if errors.Is(err, errNotFound) {
					d.markMissing(p)
					return
				}
				d.markMissing(p + " (" + err.Error() + ")")
				return
			}
			if err := d.save(p, content); err != nil {
				opts.Log("save %s: %v", p, err)
				return
			}
			if ftype == "script" {
				for hash, candidates := range ExtractChunkVariants(string(content)) {
					d.enqueueChunk(hash, candidates)
				}
			}
			for _, asset := range ExtractAssetURLs(string(content)) {
				assetPath := asset
				d.enqueue(func() {
					b, err := d.fetchLocalOrRemote(assetPath)
					if err != nil {
						if errors.Is(err, errNotFound) {
							d.markMissing(assetPath)
						}
						return
					}
					_ = d.save(assetPath, b)
				})
			}
		})
	}
	for _, p := range styles {
		enqueueAsset(p, "style")
	}
	for _, p := range scripts {
		enqueueAsset(p, "script")
	}

	go func() {
		d.wg.Wait()
		close(d.tasks)
	}()
	for i := 0; i < opts.Concurrency; i++ {
		go func() {
			for task := range d.tasks {
				task()
			}
		}()
	}
	// Drain in the main goroutine as well so Run returns only when done.
	for task := range d.tasks {
		task()
	}

	d.mu.Lock()
	saved, missing := d.saved, len(d.missing)
	d.mu.Unlock()
	opts.Log("done: %d files saved, %d missing", saved, missing)
	return nil
}

func (d *downloader) enqueue(task func()) {
	d.wg.Add(1)
	go func() {
		d.tasks <- func() {
			defer d.wg.Done()
			task()
		}
	}()
}

func (d *downloader) enqueueChunk(hash string, candidates []string) {
	key := "chunk:" + hash
	d.mu.Lock()
	if d.missing[key] { // reused as "seen" set for chunks
		d.mu.Unlock()
		return
	}
	d.missing[key] = true
	d.mu.Unlock()
	d.enqueue(func() {
		for _, cand := range candidates {
			if _, err := os.Stat(d.localPath(cand)); err == nil {
				return // already have it from a previous run
			}
			content, err := d.fetchLocalOrRemote(cand)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				continue
			}
			if err := d.save(cand, content); err != nil {
				return
			}
			if strings.HasSuffix(cand, ".js") {
				for h, cs := range ExtractChunkVariants(string(content)) {
					d.enqueueChunk(h, cs)
				}
			}
			return
		}
		// All candidates 404: the mirror never archived this lazy chunk.
		// Harmless for core boot, but record it so gaps are visible.
		d.markMissing("chunk " + hash + " (" + candidates[0] + ")")
	})
}

func (d *downloader) localPath(p string) string {
	clean := path.Clean(p)
	return filepath.Join(d.opts.Out, filepath.FromSlash(clean))
}

func (d *downloader) save(p string, content []byte) error {
	cp := d.localPath(p)
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cp, content, 0o644); err != nil {
		return err
	}
	d.mu.Lock()
	d.saved++
	d.mu.Unlock()
	return nil
}

func (d *downloader) markMissing(p string) {
	d.mu.Lock()
	d.missing[p] = true
	d.mu.Unlock()
	d.opts.Log("missing: %s", p)
}

// fetchLocalOrRemote returns the file content, preferring an existing local
// copy (resume support).
func (d *downloader) fetchLocalOrRemote(p string) ([]byte, error) {
	if b, err := os.ReadFile(d.localPath(p)); err == nil {
		return b, nil
	}
	b, err := d.fetchWithRetry(d.opts.Base + p)
	if errors.Is(err, errNotFound) && strings.HasPrefix(p, "/assets/") {
		alt := strings.TrimPrefix(p, "/assets/")
		b, err = d.fetchWithRetry(d.opts.Base + "/" + alt)
	}
	return b, err
}

func (d *downloader) fetchWithRetry(url string) ([]byte, error) {
	backoff := time.Second
	var lastErr error
	for attempt := 0; attempt <= d.opts.Retries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff + time.Duration(rand.Intn(500))*time.Millisecond)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "voidbar-mirror/0.1 (build downloader)")
		res, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case res.StatusCode == http.StatusOK:
			b, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
			res.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}
			return b, nil
		case res.StatusCode == http.StatusNotFound:
			res.Body.Close()
			return nil, errNotFound
		case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
			wait := backoff
			if ra := res.Header.Get("Retry-After"); ra != "" {
				var secs int
				if _, err := fmt.Sscanf(ra, "%d", &secs); err == nil {
					if d := time.Duration(secs) * time.Second; d > wait && d <= 60*time.Second {
						wait = d
					}
				}
			}
			res.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", res.StatusCode)
			time.Sleep(wait)
			continue
		default:
			res.Body.Close()
			return nil, fmt.Errorf("HTTP %d", res.StatusCode)
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", d.opts.Retries+1, lastErr)
}
