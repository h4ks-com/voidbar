package ircmanage

import (
	"image"
	"io"
	"net/http"
	"strings"
	"time"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// imageExtensions: URL paths ending in these get an image embed, the
// same way Discord's server-side link unfurler works (the client only
// renders what the server put into embeds[]).
var imageExtensions = map[string]bool{
	".png": true, ".apng": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".bmp": true,
}

// dimClient fetches just enough of a remote image to decode its header.
var dimClient = &http.Client{Timeout: 5 * time.Second}

// fetchImageDims downloads the image header to learn its size - the
// client sizes embed containers from width/height and renders an empty
// box without them.
func fetchImageDims(u string) (int, int) {
	resp, err := dimClient.Get(u)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0
	}
	// Headers live in the first kilobytes; a hard read cap keeps a
	// hostile "image" from streaming forever into the relay path.
	cfg, _, err := image.DecodeConfig(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// imageEmbeds synthesizes Discord image embeds for direct image URLs
// found in an IRC message. Dimensions are fetched when the remote
// answers (missing dims render as an empty box on the client); urls are
// deduped, capped at 4 like a sane bot would.
func imageEmbeds(content string) []any {
	var embeds []any
	seen := map[string]bool{}
	for _, tok := range strings.Fields(content) {
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		u := strings.Trim(tok, "<>()[]\"',")
		path := u
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			path = u[:i]
		}
		lower := strings.ToLower(path)
		dot := strings.LastIndex(lower, ".")
		if dot < 0 || !imageExtensions[lower[dot:]] {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		media := map[string]any{"url": u, "proxy_url": u}
		if w, h := fetchImageDims(u); w > 0 && h > 0 {
			media["width"] = w
			media["height"] = h
		}
		embeds = append(embeds, map[string]any{
			"type":  "image",
			"url":   u,
			"image": media,
		})
		if len(embeds) >= 4 {
			break
		}
	}
	return embeds
}
