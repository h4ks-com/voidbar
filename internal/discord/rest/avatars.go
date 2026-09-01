package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

// defaultAvatarColors maps the five historical default-avatar CDN hashes
// (as requested by oldcord-lineage web clients) to disc colors in
// Discord's palette: blue, gray, green, yellow, red.
var defaultAvatarColors = map[string][3]byte{
	"6debd47ed13483642cf09e832ed0bc1b": {59, 130, 246},
	"322c936a8c8be1b803cd94861bdfa868": {148, 155, 164},
	"dd4dbc0016779df1378e7812eabaa04d": {35, 165, 90},
	"0e291f67c9274a1abdddeb3fd919cbaa": {240, 178, 50},
	"1cbd08c76f8af6dddce02c5138971129": {242, 63, 67},
}

var defaultAvatarCache sync.Map // hash -> []byte (encoded PNG)

// defaultAvatarPNG renders (and caches) a 128x128 avatar disc for a
// "<hash>.png" style asset name; ok is false for unknown names.
func defaultAvatarPNG(name string) ([]byte, bool) {
	hash := strings.TrimSuffix(name, ".png")
	c, ok := defaultAvatarColors[hash]
	if !ok {
		return nil, false
	}
	if v, ok := defaultAvatarCache.Load(hash); ok {
		return v.([]byte), true
	}
	const size = 256
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx, cy, radius := float64(size)/2, float64(size)/2, float64(size)/2-8
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := math.Sqrt(dx*dx + dy*dy)
			// one-pixel smooth edge via distance falloff
			a := uint8(255)
			if d > radius {
				continue
			} else if d > radius-1 {
				a = uint8(255 * (radius - d))
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c[0], c[1], c[2], a
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, false
	}
	b := buf.Bytes()
	defaultAvatarCache.Store(hash, b)
	return b, true
}

// handleUpdateMe serves PATCH /users/@me: the account-wide avatar upload
// (settings sheet sends a base64 data URI). The stored hash replaces the
// data URI in the response, so the client's next render goes through the
// CDN route instead of hauling the data URI around.
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request, u *storage.User) {
	var req map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if raw, ok := req["avatar"]; ok {
		// null removes the avatar; a data URI replaces it; the field's
		// mere absence leaves it alone.
		dataURI := ""
		var av *string
		if err := json.Unmarshal(raw, &av); err == nil && av != nil {
			dataURI = strings.TrimSpace(*av)
		}
		updated, err := s.net.SetGlobalAvatar(u.ID, dataURI)
		if err != nil {
			switch {
			case errors.Is(err, network.ErrBadAvatarDataURI),
				errors.Is(err, storage.ErrAvatarTooLarge),
				errors.Is(err, storage.ErrAvatarType):
				jsonError(w, http.StatusBadRequest, "invalid avatar")
			default:
				jsonError(w, http.StatusInternalServerError, "avatar store failed")
			}
			return
		}
		writeJSON(w, http.StatusOK, model.ToUser(updated))
		return
	}
	writeJSON(w, http.StatusOK, model.ToUser(u))
}

// handleAvatarFile serves GET/HEAD /avatars/{uid}/{hash}.png: the stored
// avatar bytes (uploads and mirrored peer avatars share the store; the
// uid segment is decorative - hashes are unique).
func (s *Server) handleAvatarFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("hash")
	hash := strings.TrimSuffix(name, ".png")
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	att, data, err := s.net.GetAvatar(hash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := att.ContentType
	if ct == "" {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
