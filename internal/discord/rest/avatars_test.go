package rest

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// testAvatarPNG encodes a small distinct PNG (upload body).
func testAvatarPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestAvatarUploadFlow covers the account-wide and per-guild avatar
// paths: PATCH /users/@me and PATCH members/@me store the image, answer
// with the hash (never the data URI), and the CDN route serves the
// bytes without auth.
func TestAvatarUploadFlow(t *testing.T) {
	store, h := newServerWithStore(t, "open")
	token := registerAndLogin(t, h)
	rec, me := do(t, h, "GET", "/api/v9/users/@me", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	uid, _ := me["id"].(string)
	if me["avatar"] != nil {
		t.Fatalf("fresh account avatar: %v", me["avatar"])
	}

	dataURI := "data:image/png;base64," + b64(testAvatarPNG(t, color.RGBA{R: 255, A: 255}))
	rec, outAny := doAny(t, h, "PATCH", "/api/v9/users/@me", token, map[string]any{"avatar": dataURI})
	if rec.Code != http.StatusOK {
		t.Fatalf("global avatar: %d %v", rec.Code, outAny)
	}
	out, _ := outAny.(map[string]any)
	hash, _ := out["avatar"].(string)
	if hash == "" {
		t.Fatalf("avatar hash missing: %v", out)
	}
	// The me payload carries it too.
	rec, me = do(t, h, "GET", "/api/v9/users/@me", token, nil)
	if me["avatar"] != hash {
		t.Fatalf("me avatar: %v", me["avatar"])
	}

	// The CDN route serves the stored bytes unauthenticated.
	req := httptest.NewRequest("GET", "/avatars/"+uid+"/"+hash+".png", nil)
	srv := httptest.NewRecorder()
	h.ServeHTTP(srv, req)
	if srv.Code != http.StatusOK {
		t.Fatalf("avatar file: %d", srv.Code)
	}
	if got := srv.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("avatar content type: %q", got)
	}
	att, data, err := store.GetAvatar(hash)
	if err != nil || att == nil {
		t.Fatalf("stored avatar: %v", err)
	}
	if !bytes.Equal(srv.Body.Bytes(), data) {
		t.Fatal("served bytes differ from stored")
	}

	// Per-guild avatar: a second image under the same account.
	net := &storage.Network{ID: "800000000000000000", Name: "AvatarNet", ConnID: "irc.avatar.example"}
	if err := store.UpsertNetwork(net); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{NetworkID: net.ID, UserID: uid, Nick: "tester", AutoJoin: []string{"#test"}, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	guildURI := "data:image/png;base64," + b64(testAvatarPNG(t, color.RGBA{G: 255, A: 255}))
	rec, memAny := doAny(t, h, "PATCH", "/api/v9/guilds/"+net.ID+"/members/@me", token, map[string]any{"avatar": guildURI})
	if rec.Code != http.StatusOK {
		t.Fatalf("guild avatar: %d %v", rec.Code, memAny)
	}
	mem, _ := memAny.(map[string]any)
	guildHash, _ := mem["user"].(map[string]any)["avatar"].(string)
	if guildHash == "" || guildHash == hash {
		t.Fatalf("guild avatar hash: %q vs global %q", guildHash, hash)
	}

	// Clearing the global avatar leaves the guild override standing.
	rec, outAny = doAny(t, h, "PATCH", "/api/v9/users/@me", token, map[string]any{"avatar": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear avatar: %d %v", rec.Code, outAny)
	}
	out, _ = outAny.(map[string]any)
	if out["avatar"] != nil {
		t.Fatalf("cleared avatar: %v", out["avatar"])
	}
	rec, memAny = doAny(t, h, "PATCH", "/api/v9/guilds/"+net.ID+"/members/@me", token, map[string]any{"nick": "tester"})
	if rec.Code != http.StatusOK {
		t.Fatalf("member refresh: %d", rec.Code)
	}
	mem, _ = memAny.(map[string]any)
	if got, _ := mem["user"].(map[string]any)["avatar"].(string); got != guildHash {
		t.Fatalf("guild override lost: %v", mem["user"])
	}

	// Garbage is rejected, not stored.
	rec, _ = doAny(t, h, "PATCH", "/api/v9/users/@me", token, map[string]any{"avatar": "definitely-not-a-data-uri"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad avatar accepted: %d", rec.Code)
	}
}

func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
