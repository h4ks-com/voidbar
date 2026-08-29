package ircmanage

import (
	"strings"
)

// imageExtensions: URL paths ending in these get an image embed, the
// same way Discord's server-side link unfurler works (the client only
// renders what the server put into embeds[]).
var imageExtensions = map[string]bool{
	".png": true, ".apng": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".bmp": true,
}

// imageEmbeds synthesizes Discord image embeds for direct image URLs
// found in an IRC message. Dimensions are not fetched (the client sizes
// the container from the download); urls are deduped, capped at 4 like
// a sane bot would.
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
		embeds = append(embeds, map[string]any{
			"type":        "image",
			"url":         u,
			"image":       map[string]any{"url": u, "proxy_url": u},
			"thumbnail":   nil,
			"video":       nil,
			"description": "",
		})
		if len(embeds) >= 4 {
			break
		}
	}
	return embeds
}
