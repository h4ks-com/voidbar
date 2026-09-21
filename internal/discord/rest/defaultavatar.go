package rest

import (
	"net/http"
	"strconv"
	"strings"
)

// Numeric default avatars: 2023+ web clients fetch /embed/avatars/{0..5}.png
// (index = discriminator % 6) from the CDN host, which resolves to THIS
// bouncer. A 404 renders as a blank hole in every default-avatar slot.
// The palette and renderer are the hash-based ones oldcord-lineage
// clients already use (see avatars.go); indices cycle through them.
var defaultAvatarIndexHashes = []string{
	"6debd47ed13483642cf09e832ed0bc1b", // blue
	"322c936a8c8be1b803cd94861bdfa868", // gray
	"dd4dbc0016779df1378e7812eabaa04d", // green
	"0e291f67c9274a1abdddeb3fd919cbaa", // yellow
	"1cbd08c76f8af6dddce02c5138971129", // red
	"6debd47ed13483642cf09e832ed0bc1b", // blue again for 5
}

// handleDefaultAvatar serves GET/HEAD /embed/avatars/{n}[.png]. No auth:
// discovery-CDN semantics like /avatars.
func (s *Server) handleDefaultAvatar(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("n"), ".png")
	idx, err := strconv.Atoi(name)
	if err != nil || idx < 0 {
		http.NotFound(w, r)
		return
	}
	body, ok := defaultAvatarPNG(defaultAvatarIndexHashes[idx%len(defaultAvatarIndexHashes)] + ".png")
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	_, _ = w.Write(body)
}
