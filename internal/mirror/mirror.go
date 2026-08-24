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
	"bytes"
	"compress/gzip"
	"context"
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

	"github.com/andybalholm/brotli"
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

// textAsset reports whether the path is expected to hold textual content.
// Fonts/images are legitimately binary and must never be "fixed".
func textAsset(p string) bool {
	switch filepath.Ext(p) {
	case ".js", ".css", ".html", ".json", ".map", ".txt", ".svg":
		return true
	}
	return false
}

// asciiHead reports whether the first bytes are printable source: webpack
// chunk wrappers and license banners are always plain ASCII, while
// compressed bodies start with arbitrary binary within bytes.
func asciiHead(b []byte) bool {
	head := b
	if len(head) > 64 {
		head = head[:64]
	}
	for _, c := range head {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
		if c > 0x7f {
			return false
		}
	}
	return true
}

// looksTextual validates that b is already source and needs no decoding:
// ASCII head plus a high-byte fraction small enough that b cannot be a
// compressed stream (those are ~50% high bytes).
func looksTextual(b []byte) bool {
	if !asciiHead(b) {
		return false
	}
	sample := b
	if len(sample) > 64<<10 {
		sample = sample[:64<<10]
	}
	high := 0
	for _, c := range sample {
		if c > 0x7f {
			high++
		}
	}
	return high*20 < len(sample) // <5% high bytes
}

// DecodeCompressed repairs bodies saved in a compressed transfer encoding:
// Wayback replays the ARCHIVED Content-Encoding header (frequently brotli
// for discord.com assets), and Go's transport only transparently inflates
// gzip it requested itself - brotli always arrives raw. For textual assets
// that do not look textual, gzip (magic) and then brotli are attempted.
// A successful DECODE is validated by asciiHead only (localization chunks
// legitimately carry tens of percent of high bytes, so the strict
// looksTextual ratio would reject valid output); a decode that errors out
// or yields a binary head is refused. ok is true when the content is
// usable as-is (already textual or successfully decoded).
func DecodeCompressed(p string, b []byte) (out []byte, ok bool) {
	if !textAsset(p) || looksTextual(b) {
		return b, true
	}
	// gzip magic
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err == nil {
			if dec, err := io.ReadAll(zr); err == nil && asciiHead(dec) {
				return dec, true
			}
		}
	}
	// brotli has no magic number: try decoding; a wrong input errors out
	// with high probability (CRC + Huffman structure), and the asciiHead
	// check catches accidental decodes of garbage.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, brotli.NewReader(bytes.NewReader(b))); err == nil && asciiHead(buf.Bytes()) {
		return buf.Bytes(), true
	}
	return b, false
}

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

	// First, repair everything already on disk: older runs may have stored
	// bodies in their archived transfer encoding (Wayback replays the
	// original Content-Encoding, frequently brotli, which the downloader
	// used to save verbatim). This pass needs no network and makes resumed
	// runs self-healing regardless of what discovery reaches.
	revalidated := d.revalidateExisting()
	if revalidated > 0 {
		opts.Log("revalidated %d compressed files from a previous run", revalidated)
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

	// When fetching a script (local or remote), always parse it so a resumed
	// run still discovers chunk groups from previously downloaded files.
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

// revalidateExisting walks the output tree and repairs any text asset
// stored in a compressed transfer encoding, returning the repair count.
func (d *downloader) revalidateExisting() int {
	n := 0
	_ = filepath.WalkDir(d.opts.Out, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(d.opts.Out, p)
		if rerr != nil {
			return nil
		}
		webPath := "/" + filepath.ToSlash(rel)
		if !textAsset(webPath) {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		dec, ok := DecodeCompressed(webPath, b)
		if !ok || bytes.Equal(dec, b) {
			return nil
		}
		if werr := os.WriteFile(p, dec, 0o644); werr == nil {
			n++
		}
		return nil
	})
	return n
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
			if b, err := os.ReadFile(d.localPath(cand)); err == nil {
				// Revalidate: an earlier run may have saved the chunk still
				// compressed; repair it in place instead of trusting it.
				if dec, ok := DecodeCompressed(cand, b); ok {
					if !bytes.Equal(dec, b) {
						_ = d.save(cand, dec)
					}
					return // already have it from a previous run
				}
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
// copy (resume support). Wayback bases additionally fall back to the CDX API
// when the snapshot is missing the file entirely: lazy webpack chunks are
// frequently absent from one capture but present in another, and chunk files
// are content-addressed so any snapshot serves.
func (d *downloader) fetchLocalOrRemote(p string) ([]byte, error) {
	if b, err := os.ReadFile(d.localPath(p)); err == nil {
		// Resume support, with revalidation: a previous run may have stored
		// the body still compressed (Wayback replayed the archived
		// Content-Encoding). Repair it in place so the mirror self-heals on
		// the next `voidbar mirror` pass without any network traffic.
		if dec, ok := DecodeCompressed(p, b); ok {
			if !bytes.Equal(dec, b) {
				_ = d.save(p, dec)
			}
			return dec, nil
		}
		// Unusable as-is (undecodable): fall through and re-fetch.
	}
	b, err := d.fetchWithRetry(d.opts.Base + p)
	if errors.Is(err, errNotFound) && strings.HasPrefix(p, "/assets/") {
		alt := strings.TrimPrefix(p, "/assets/")
		b, err = d.fetchWithRetry(d.opts.Base + "/" + alt)
	}
	if err == nil {
		b, _ = DecodeCompressed(p, b)
	}
	if errors.Is(err, errNotFound) {
		if b2, ok := d.fetchViaCDX(p); ok {
			if dec, ok := DecodeCompressed(p, b2); ok {
				return dec, nil
			}
			return b2, nil
		}
	}
	return b, err
}

// fetchViaCDX tries to recover a Wayback-missing file from any other
// snapshot via the CDX API. It is best-effort: failures are swallowed and
// reported through the logger only.
func (d *downloader) fetchViaCDX(p string) ([]byte, bool) {
	original, ok := SplitWayback(d.opts.Base)
	if !ok {
		return nil, false
	}
	ctx := context.Background()
	target, err := CDXLookup(ctx, d.client, original+p)
	if err != nil {
		d.opts.Log("cdx: no snapshot for %s: %v", p, err)
		return nil, false
	}
	b, err := d.fetchWithRetry(target)
	if err != nil {
		d.opts.Log("cdx: fetch failed for %s: %v", p, err)
		return nil, false
	}
	d.opts.Log("cdx: rescued %s from %s", p, target)
	return b, true
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
