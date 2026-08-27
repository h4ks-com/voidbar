package rest

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"strings"
	"sync"
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
