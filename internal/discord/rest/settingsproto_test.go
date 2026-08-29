package rest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// b64enc is a tiny helper for readable test blobs.
func b64enc(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// protoBlob builds a length-delimited top-level field: tag = num<<3|2,
// varint length, payload.
func protoBlob(num int, payload string) []byte {
	out := []byte{byte(num)<<3 | 2, byte(len(payload))}
	return append(out, payload...)
}

func TestSettingsProtoMergeFlow(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	_, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	srv := httptest.NewServer(New(svc, cfg, logger, gw, netSvc, manager))
	defer srv.Close()

	patch := func(kind, settingsB64 string) map[string]any {
		body, _ := json.Marshal(map[string]any{"settings": settingsB64})
		req, _ := http.NewRequest("PATCH", srv.URL+"/api/v9/users/@me/settings-proto/"+kind, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("PATCH status %d", resp.StatusCode)
		}
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	get := func(kind string) map[string]any {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v9/users/@me/settings-proto/"+kind, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// Initial save: two top-level fields (appearance=5, localization=12).
	v1 := append(protoBlob(5, "appearance-v1"), protoBlob(12, "locale-v1")...)
	res := patch("1", b64enc(v1))
	if res["out_of_date"] != false {
		t.Fatalf("out_of_date = %v", res["out_of_date"])
	}

	// Later save replaces only appearance; localization must survive.
	v2 := protoBlob(5, "appearance-v2")
	res = patch("1", b64enc(v2))
	b64, _ := res["settings"].(string)
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blob, []byte("appearance-v2")) {
		t.Error("patched field missing from merge echo")
	}
	if !bytes.Contains(blob, []byte("locale-v1")) {
		t.Error("unchanged top-level field lost in merge")
	}

	// GET serves the same merged blob.
	got := get("1")
	if got["settings"] != res["settings"] {
		t.Errorf("GET blob %v != PATCH echo", got["settings"])
	}

	// Kinds are independent stores.
	if other := get("2"); other["settings"] != "" {
		t.Errorf("kind 2 leaked: %v", other["settings"])
	}

	// Unparseable body: stored state echoed, no crash.
	bad, _ := http.NewRequest("PATCH", srv.URL+"/api/v9/users/@me/settings-proto/1", strings.NewReader("not json"))
	bad.Header.Set("Authorization", token)
	badResp, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != 200 {
		t.Fatalf("garbage PATCH status %d", badResp.StatusCode)
	}
	out := map[string]any{}
	_ = json.NewDecoder(badResp.Body).Decode(&out)
	if out["settings"] != got["settings"] {
		t.Errorf("garbage PATCH disturbed store: %v", out["settings"])
	}
}

func TestSplitProtoFields(t *testing.T) {
	blob := []byte{}
	blob = append(blob, protoBlob(1, "v")...)
	blob = append(blob, 0x08, 0x2A) // field 1 varint 42
	fields, err := splitProtoFields(blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields", len(fields))
	}
	if fields[0].num != 1 || fields[1].num != 1 {
		t.Fatalf("field numbers: %d %d", fields[0].num, fields[1].num)
	}

	if _, err := splitProtoFields([]byte{0x0B}); err == nil { // group wire type
		t.Error("group wire type accepted")
	}
	if _, err := splitProtoFields([]byte{0x0A}); err == nil { // truncated length
		t.Error("truncated blob accepted")
	}
}
