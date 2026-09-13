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
// occasionally contain zero-byte captures; an empty bootstrap script
// wedges webpack mid-boot with no error anywhere, so candidates are
// walked newest-to-older until one extracts without empty files.
func syncAssets(ctx context.Context, logger *slog.Logger, cacheDir, channel, at string) (string, error) {
	shas, err := resolveCommit(ctx, channel, at)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
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

		empty, err := extractTarball(ctx, logger, cacheDir, sha)
		if err != nil {
			lastErr = err
			logger.Warn("scrape failed to extract, walking back", "commit", sha, "err", err)
			continue
		}
		if empty > 0 {
			lastErr = fmt.Errorf("scrape %s has %d empty capture(s)", sha[:8], empty)
			logger.Warn("corrupt scrape, walking back", "commit", sha, "empty_files", empty)
			continue
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return "", err
		}
		backfillMissing(ctx, logger, cacheDir, shas)
		return sha, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no clean scrape found")
	}
	return "", lastErr
}

// backfillMissing heals partial scrapes: even a scrape with no empty
// files can miss chunks the scraper's session never loaded (the
// client discovers them lazily and dies with ChunkLoadError). The
// chunk universe is recoverable from the bundles themselves - the
// webpack runtime carries an id-to-content-hash table - and asset
// names are content hashes, so a same-named file in a NEIGHBORING
// scrape (a different build, but an unchanged chunk) is byte-exact.
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
	logger.Info("scrape is missing referenced assets, healing from neighbors", "missing", len(missing))
	for _, sha := range shas {
		if len(missing) == 0 {
			break
		}
		if healed := fetchAssetsFromCommit(ctx, cacheDir, sha, missing); healed > 0 {
			logger.Info("backfill from neighbor", "commit", sha[:8], "healed", healed, "still_missing", len(missing))
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		logger.Warn("assets unhealed - client may hit ChunkLoadError on these", "missing", names)
	}
}

// fetchAssetsFromCommit downloads the still-missing assets found in
// one neighbor commit's tree, deleting each healed name from the
// missing set. Returns how many files actually landed on disk.
func fetchAssetsFromCommit(ctx context.Context, cacheDir, sha string, missing map[string]bool) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/git/trees/"+sha+"?recursive=1", nil)
	req.Header.Set("User-Agent", "voidweb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return 0
	}
	healed := 0
	for _, e := range tree.Tree {
		name, ok := strings.CutPrefix(e.Path, "assets/")
		if !ok || e.Size == 0 || !missing[name] {
			continue
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawBase+"/"+sha+"/"+e.Path, nil)
		r.Header.Set("User-Agent", "voidweb")
		file, err := http.DefaultClient.Do(r)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(file.Body, 64<<20))
		file.Body.Close()
		if err != nil || file.StatusCode != http.StatusOK || len(body) == 0 {
			continue
		}
		dst := filepath.Join(cacheDir, "assets", name)
		if os.WriteFile(dst, body, 0o644) == nil {
			delete(missing, name)
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
		if n == 0 {
			empty++
		}
	}
	if files == 0 {
		return 0, errors.New("tarball contained no client files")
	}
	logger.Info("client extracted", "commit", sha[:8], "files", files, "mb", bytes/1024/1024, "empty", empty)
	return empty, nil
}

// assetHandler serves the cached client bundles. Asset names are
// content hashes, so hits are immutable. A miss triggers lazy
// healing: the scraping archive's git history is searched (one
// path-filtered commit lookup) for any scrape that captured the same
// content-hashed file, and the bytes are pulled from that commit.
// Chunks the client loads conditionally exist only in some scrapes,
// so the pinned one alone is never quite complete. Discord's live
// asset host is no alternative - discord.com is blocked at the ISP
// here, such fetches would just hang.
type assetHandler struct {
	dir    string
	logger *slog.Logger
	mu     sync.Mutex
	donors map[string]string // asset name -> donor sha, "-" = nowhere in the archive
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
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, dst)
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
	sha, err := findDonor(ctx, name)
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
// name is a content hash).
func findDonor(ctx context.Context, name string) (string, error) {
	u := githubAPI + "/commits?path=assets/" + name + "&per_page=1"
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
