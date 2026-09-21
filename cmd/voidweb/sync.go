package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	githubAPI = "https://api.github.com/repos/jaimeadf/discord-scraping"
	codeload  = "https://codeload.github.com/jaimeadf/discord-scraping/tar.gz/"
	rawBase   = "https://raw.githubusercontent.com/jaimeadf/discord-scraping"
)

// resolveCommit finds the scrape commits to consider, newest first:
// the branch head, or with at set, every commit on the channel branch
// made on or before that date - discord-scraping publishes one commit
// per detected build, so its git history is a catalog of every client
// version. Several candidates come back because individual scrapes
// are sometimes corrupt (empty capture files), and syncAssets walks
// back through the list until a clean one lands.
func resolveCommit(ctx context.Context, channel, at string) ([]string, error) {
	u := githubAPI + "/commits?sha=" + channel + "&per_page=20"
	if at != "" {
		day, err := time.Parse("2006-01-02", at)
		if err != nil {
			return nil, fmt.Errorf("bad -at date: %w", err)
		}
		u += "&until=" + day.Add(24*time.Hour-time.Second).UTC().Format(time.RFC3339)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github commits: %s", resp.Status)
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		if at != "" {
			return nil, errors.New("no scrape commit on or before " + at + " (repo history starts later)")
		}
		return nil, errors.New("empty commit list")
	}
	shas := make([]string, len(commits))
	for i, c := range commits {
		shas[i] = c.SHA
	}
	return shas, nil
}

// syncAssets materializes the client under cacheDir (assets/,
// index.html, manifest.json) from a codeload tarball. Scrapes
// occasionally contain zero-byte captures; those are left absent and
// healable - EXCEPT in boot-critical files (the scripts and styles
// the index loads eagerly): the runtime chunk embeds the per-build
// chunk table, so its name is unique to this build and no donor can
// ever exist for it. Scrapes with empty boot-critical captures are
// walked back; the client-version pin moves to an older build.
func syncAssets(ctx context.Context, logger *slog.Logger, cacheDir, channel, at string) (string, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	// Marker first: with a synced build on disk, startup can skip the
	// API entirely - unless the pin moved and this build no longer
	// matches it (resolveCommit returning the marker outside the
	// candidate list triggers a fresh sync).
	if markers, _ := filepath.Glob(filepath.Join(cacheDir, "synced-*")); len(markers) > 0 {
		sha := strings.TrimPrefix(filepath.Base(markers[0]), "synced-")
		if _, err := os.Stat(filepath.Join(cacheDir, "index.html")); err == nil {
			shas, err := resolveCommit(ctx, channel, at)
			if err == nil {
				pinned := false
				for _, s := range shas {
					if s == sha {
						pinned = true
						break
					}
				}
				if pinned {
					// First boot after this build landed: sweep the
					// neighbors once to heal gaps (empty captures,
					// uncaptured chunks). API hiccups just defer the
					// sweep - the cache serves on.
					if _, err := os.Stat(filepath.Join(cacheDir, "swept-"+sha)); err != nil {
						backfillMissing(ctx, logger, cacheDir, shas)
						_ = os.WriteFile(filepath.Join(cacheDir, "swept-"+sha), nil, 0o644)
					}
					return sha, nil
				}
			} else {
				// Cannot verify the pin (rate limit, outage): serve
				// the cache as-is rather than locking the local app
				// out.
				return sha, nil
			}
		}
	}
	shas, err := resolveCommit(ctx, channel, at)
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, sha := range shas {
		marker := filepath.Join(cacheDir, "synced-"+sha)
		if _, err := os.Stat(marker); err == nil {
			return sha, nil
		}
		// A different build was synced before: its content-hashed
		// files would linger forever, so start the asset tree from
		// scratch, and drop stale markers so switching back
		// re-downloads instead of trusting a marker with no files
		// behind it.
		if err := os.RemoveAll(filepath.Join(cacheDir, "assets")); err != nil {
			return "", err
		}
		if markers, _ := filepath.Glob(filepath.Join(cacheDir, "synced-*")); len(markers) > 0 {
			for _, m := range markers {
				_ = os.Remove(m)
			}
		}

		if _, err := extractTarball(ctx, logger, cacheDir, sha); err != nil {
			lastErr = err
			logger.Warn("scrape failed to extract, walking back", "commit", sha, "err", err)
			continue
		}
		if missing := bootCriticalMissing(cacheDir); len(missing) > 0 {
			lastErr = fmt.Errorf("scrape %s lost boot-critical captures: %v", sha[:8], missing)
			logger.Warn("unusable scrape (empty boot-critical captures), walking back", "commit", sha, "missing", missing)
			continue
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return "", err
		}
		backfillMissing(ctx, logger, cacheDir, shas)
		_ = os.WriteFile(filepath.Join(cacheDir, "swept-"+sha), nil, 0o644)
		return sha, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable scrape found")
	}
	return "", lastErr
}

// bootCriticalMissing checks that the files the index loads EAGERLY
// (bootstrap scripts, the stylesheet, the icon) exist on disk: an
// empty capture of any of them starves the boot with no recovery,
// since their per-build names have no donors. Social-preview meta
// images and prefetch hints are deliberately ignored - the client
// boots without them.
func bootCriticalMissing(cacheDir string) []string {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "index.html"))
	if err != nil {
		return []string{"index.html"}
	}
	page := string(raw)
	var names []string
	for _, re := range []*regexp.Regexp{scriptSrc, sheetHref, iconHref} {
		for _, m := range re.FindAllStringSubmatch(page, -1) {
			if _, err := os.Stat(filepath.Join(cacheDir, "assets", m[1])); err != nil {
				names = append(names, m[1])
			}
		}
	}
	return names
}

var (
	scriptSrc = regexp.MustCompile(`<script[^>]+src="/assets/([0-9A-Za-z._-]+)"`)
	sheetHref = regexp.MustCompile(`<link[^>]*rel="stylesheet"[^>]*href="/assets/([0-9A-Za-z._-]+)"`)
	iconHref  = regexp.MustCompile(`<link[^>]*rel="icon"[^>]*href="/assets/([0-9A-Za-z._-]+)"`)
)

// backfillMissing heals partial scrapes: even a scrape with no empty
// files can miss chunks the scraper's session never loaded (the
// client discovers them lazily and dies with ChunkLoadError). The
// chunk universe is recoverable from the bundles themselves - the
// webpack runtime carries an id-to-content-hash table - and asset
// names are content hashes, so a same-named file in a NEIGHBORING
// scrape (a different build, but an unchanged chunk) is byte-exact.
// Neighbors are swept as codeload tarballs: no API quota, and one
// tarball covers every missing name it holds.
func backfillMissing(ctx context.Context, logger *slog.Logger, cacheDir string, shas []string) {
	assetsDir := filepath.Join(cacheDir, "assets")
	referenced := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(cacheDir, "index.html"))
	if err != nil {
		return
	}
	for _, m := range assetRef.FindAllStringSubmatch(string(raw), -1) {
		referenced[m[1]] = true
	}
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(assetsDir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range chunkHash.FindAllStringSubmatch(string(body), -1) {
			referenced[m[1]+".js"] = true
		}
	}
	missing := map[string]bool{}
	for name := range referenced {
		if _, err := os.Stat(filepath.Join(assetsDir, name)); err != nil {
			missing[name] = true
		}
	}
	if len(missing) == 0 {
		return
	}
	logger.Info("scrape is missing referenced assets, sweeping neighbors", "missing", len(missing))
	for _, sha := range shas {
		if len(missing) == 0 {
			break
		}
		if healed := sweepTarball(ctx, logger, cacheDir, sha, missing); healed > 0 {
			logger.Info("backfill from neighbor", "commit", sha[:8], "healed", healed, "still_missing", len(missing))
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		logger.Warn("assets unhealed - the lazy healer will retry them per request", "missing_count", len(names))
	}
}

// sweepTarball streams one neighbor scrape's tarball and extracts
// every currently-missing asset it carries (empty captures skipped).
func sweepTarball(ctx context.Context, logger *slog.Logger, cacheDir, sha string, missing map[string]bool) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeload+sha, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn("neighbor tarball", "commit", sha[:8], "err", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("neighbor tarball", "commit", sha[:8], "status", resp.Status)
		return 0
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0
	}
	defer gz.Close()
	prefix := "discord-scraping-" + sha + "/"
	healed := 0
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size == 0 {
			continue
		}
		name, ok := strings.CutPrefix(hdr.Name, prefix)
		if !ok {
			continue
		}
		asset, ok := strings.CutPrefix(name, "assets/")
		if !ok || !missing[asset] {
			continue
		}
		dst := filepath.Join(cacheDir, "assets", asset)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		out, err := os.Create(dst)
		if err != nil {
			continue
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err == nil && n > 0 {
			delete(missing, asset)
			healed++
		}
	}
	return healed
}

var (
	assetRef  = regexp.MustCompile(`(?:/assets/|"assets/)([0-9A-Za-z._-]+\.(?:js|css|png|svg|woff2|ico|webm|mp3|json))`)
	chunkHash = regexp.MustCompile(`"([0-9a-f]{20})"`)
)

// extractTarball downloads the scrape tarball for one commit into
// cacheDir and reports how many captured files came out empty.
func extractTarball(ctx context.Context, logger *slog.Logger, cacheDir, sha string) (int, error) {
	logger.Info("downloading client", "commit", sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeload+sha, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("codeload: %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, err
	}
	defer gz.Close()

	prefix := "discord-scraping-" + sha + "/"
	files, bytes, empty := 0, 0, 0
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		name, ok := strings.CutPrefix(hdr.Name, prefix)
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case strings.HasPrefix(name, "assets/"), name == "index.html", name == "manifest.json":
		default:
			continue
		}
		dst := filepath.Join(cacheDir, filepath.FromSlash(name))
		if hdr.Size == 0 {
			// Zero-byte capture: leave it absent so the healers can
			// source a real donor for the name.
			empty++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		out, err := os.Create(dst)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err != nil {
			return 0, err
		}
		files++
		bytes += int(n)
	}
	if files == 0 {
		return 0, errors.New("tarball contained no client files")
	}
	logger.Info("client extracted", "commit", sha[:8], "files", files, "mb", bytes/1024/1024, "empty_captures", empty)
	return empty, nil
}

// assetHandler serves the cached client bundles. Asset names are
// content hashes, so hits are immutable. A miss triggers lazy
// healing: the archive's git history is searched (path-filtered
// commit lookup on the channel branch) for any scrape that captured
// the same content-hashed file, and the bytes are pulled from that
// commit. Chunks the client loads conditionally exist only in some
// scrapes; a few - one-time modals the scraping account had already
// dismissed - exist nowhere and are answered with a synthetic empty
// webpack chunk keyed by the id from the runtime's chunk table: the
// loader resolves, its chunk group completes, and the no-op modal
// costs an unhandled rejection instead of the whole boot. Discord's
// live asset host is no alternative - discord.com is blocked at the
// ISP here, such fetches would just hang.
type assetHandler struct {
	dir     string
	channel string
	logger  *slog.Logger
	mu      sync.Mutex
	donors  map[string]string // asset name -> donor sha, "-" = nowhere in the archive
	ids     map[string]string // asset base name -> chunk id (from the runtime chunk table)
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	dst := filepath.Join(h.dir, filepath.FromSlash(name))
	if _, err := os.Stat(dst); err != nil {
		if !h.heal(r.Context(), name, dst) {
			h.serveStub(w, name)
			return
		}
	}
	if strings.HasSuffix(name, ".js") {
		h.servePatchedJS(w, r, name, dst)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, dst)
}

// assetLoaderMiss matches webpack's async require.context miss path. The
// archive's asset tables lack entries whose request strings the client
// builds by concatenation (font weight/style variants), and the
// MODULE_NOT_FOUND throw rejects the boot Promise.all into errorPage.
// The replacement resolves to stub module 950001 (a data-URL string,
// seeded with the ghost modules) instead of throwing.
var assetLoaderMiss = regexp.MustCompile(`if \(!n\.o\(r, e\)\) return Promise\.resolve\(\)\.then\(\(\(\) => \{\s*var t = new Error\("Cannot find module '" \+ e \+ "'"\);\s*t\.code = "MODULE_NOT_FOUND";\s*throw t\s*\}\)\);`)

const assetLoaderMissFix = `if (!n.o(r, e)) return Promise.resolve().then((() => n.t(950001, 23)));`

// patchedJS memoizes rewritten bundle bytes (they are tens of MB; the
// rewrite is byte-stable so it pays to do it once).
var patchedJS sync.Map

func (h *assetHandler) servePatchedJS(w http.ResponseWriter, r *http.Request, name, dst string) {
	if v, ok := patchedJS.Load(name); ok {
		h.writeJS(w, v.([]byte))
		return
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body = assetLoaderMiss.ReplaceAll(body, []byte(assetLoaderMissFix))
	patchedJS.Store(name, body)
	h.writeJS(w, body)
}

func (h *assetHandler) writeJS(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(body)
}

// serveStub answers an unhealable asset: CSS and images as empty
// bytes, JS chunks as a push of their chunk id with no modules - the
// webpack loader treats the chunk as loaded and moves on. The id is
// looked up in the runtime's chunk table (id -> content-hash pairs
// scanned out of the cached bundles).
func (h *assetHandler) serveStub(w http.ResponseWriter, name string) {
	body := []byte{}
	ct := "application/octet-stream"
	if strings.HasSuffix(name, ".js") {
		base := strings.TrimSuffix(name, ".js")
		id, ok := h.ids[base]
		if !ok {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		body = []byte("(self.webpackChunkdiscord_app=self.webpackChunkdiscord_app||[]).push([[" + id + "],{}])")
		ct = "application/javascript"
	} else if strings.HasSuffix(name, ".css") {
		ct = "text/css"
	} else if strings.HasSuffix(name, ".png") {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(body)
}

// buildChunkIDs scans the cached bundles for the runtime's chunk
// table entries (id: "hash") so stub chunks can push under the id
// the loader expects.
func (h *assetHandler) buildChunkIDs() {
	h.ids = map[string]string{}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	pair := regexp.MustCompile(`(\d{1,7}):\s*"([0-9a-f]{20})"`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(h.dir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range pair.FindAllStringSubmatch(string(body), -1) {
			if _, exists := h.ids[m[2]]; !exists {
				h.ids[m[2]] = m[1]
			}
		}
	}
	h.logger.Info("chunk table mapped", "ids", len(h.ids))
}

// heal finds a donor commit for one missing asset and materializes
// the file. Donor answers are memoized: a hit avoids the API lookup
// forever after, a miss ("-") avoids re-probing names the archive
// never captured anywhere.
func (h *assetHandler) heal(ctx context.Context, name, dst string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return true
	}
	if sha, known := h.donors[name]; known {
		if sha == "-" {
			return false
		}
		if fetchRaw(ctx, sha, name, dst) {
			return true
		}
		return false
	}
	sha, err := findDonor(ctx, h.channel, name)
	if err != nil {
		// Lookup failed (rate limit, network): do not memoize - the
		// next request retries.
		h.logger.Warn("donor lookup failed", "asset", name, "err", err)
		return false
	}
	if sha == "" {
		h.donors[name] = "-"
		h.logger.Warn("asset nowhere in the archive", "asset", name)
		h.persistDonors()
		return false
	}
	h.donors[name] = sha
	h.persistDonors()
	if fetchRaw(ctx, sha, name, dst) {
		h.logger.Info("healed asset from archive", "asset", name, "donor", sha[:8])
		return true
	}
	return false
}

// findDonor asks the archive's history for any commit that captured
// the asset: commits touching the path appear when a scrape added or
// removed it, and the newest such scrape is a byte-exact donor (the
// name is a content hash). The lookup MUST scope to the channel
// branch - scrapes never land on the default branch.
func findDonor(ctx context.Context, channel, name string) (string, error) {
	u := githubAPI + "/commits?sha=" + channel + "&path=assets/" + name + "&per_page=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github commits: %s", resp.Status)
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", nil
	}
	return commits[0].SHA, nil
}

func fetchRaw(ctx context.Context, sha, name, dst string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawBase+"/"+sha+"/assets/"+name, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil || len(body) == 0 {
		return false
	}
	return os.WriteFile(dst, body, 0o644) == nil
}

// persistDonors spills the donor memo next to the asset cache so
// restarts keep their API budget.
func (h *assetHandler) persistDonors() {
	raw, err := json.Marshal(h.donors)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(filepath.Dir(h.dir), "donors.json"), raw, 0o644)
}

// loadDonors reads the memo written by earlier runs.
func loadDonors(cacheDir string) map[string]string {
	donors := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(cacheDir, "donors.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &donors)
	}
	return donors
}
