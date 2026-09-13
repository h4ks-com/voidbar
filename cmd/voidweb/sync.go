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
	"strings"
	"time"
)

const (
	githubAPI = "https://api.github.com/repos/jaimeadf/discord-scraping"
	codeload  = "https://codeload.github.com/jaimeadf/discord-scraping/tar.gz/"
)

// resolveCommit finds the scrape commit to serve: the branch head, or
// with at set, the last commit on the channel branch made on or
// before that date - discord-scraping publishes one commit per
// detected build, so its git history is a catalog of every client
// version.
func resolveCommit(ctx context.Context, channel, at string) (string, error) {
	u := githubAPI + "/commits?sha=" + channel + "&per_page=1"
	if at != "" {
		day, err := time.Parse("2006-01-02", at)
		if err != nil {
			return "", fmt.Errorf("bad -at date: %w", err)
		}
		u += "&until=" + day.Add(24*time.Hour-time.Second).UTC().Format(time.RFC3339)
	}
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
		if at != "" {
			return "", errors.New("no scrape commit on or before " + at + " (repo history starts later)")
		}
		return "", errors.New("empty commit list")
	}
	return commits[0].SHA, nil
}

// syncAssets materializes the client for the given commit under
// cacheDir (assets/, index.html, manifest.json) from the codeload
// tarball, skipping the download when the commit is already on disk.
func syncAssets(ctx context.Context, logger *slog.Logger, cacheDir, channel, at string) (string, error) {
	sha, err := resolveCommit(ctx, channel, at)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(cacheDir, "synced-"+sha)
	if _, err := os.Stat(marker); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	// A different build was synced before: its content-hashed files
	// would linger forever, so start the asset tree from scratch, and
	// drop stale markers so switching back re-downloads instead of
	// trusting a marker with no files behind it.
	if err := os.RemoveAll(filepath.Join(cacheDir, "assets")); err != nil {
		return "", err
	}
	if markers, _ := filepath.Glob(filepath.Join(cacheDir, "synced-*")); len(markers) > 0 {
		for _, m := range markers {
			_ = os.Remove(m)
		}
	}

	logger.Info("downloading client", "commit", sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codeload+sha, nil)
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
		return "", fmt.Errorf("codeload: %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	prefix := "discord-scraping-" + sha + "/"
	files, bytes := 0, 0
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
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
			return "", err
		}
		out, err := os.Create(dst)
		if err != nil {
			return "", err
		}
		n, err := io.Copy(out, tr)
		out.Close()
		if err != nil {
			return "", err
		}
		files++
		bytes += int(n)
	}
	if files == 0 {
		return "", errors.New("tarball contained no client files")
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return "", err
	}
	logger.Info("client extracted", "files", files, "mb", bytes/1024/1024)
	return sha, nil
}

// assetHandler serves the cached client bundles. Asset names are
// content hashes, so hits are immutable. A miss is a plain 404: the
// scrape set is complete for pinned builds, and fetching from
// Discord's live asset host is a non-starter where discord.com is
// blocked (the hanging fetch would stall every miss).
type assetHandler struct {
	dir string
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(h.dir, filepath.FromSlash(name)))
}
