package rest

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// TestCreateGuildAndGuildDetail exercises the connection-string-as-invite
// flow end to end on the REST surface. The IRC manager is wired but no real
// connection is attempted (no server configured for the host; EnsureConn
// spawns a goroutine that fails to connect and is harmless in tests).
func TestCreateGuildAndGuildDetail(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	// Create a network from a connection string (Discord's "Create server"
	// form posts {name: ...}).
	rec, out := do(t, h, "POST", "/api/v9/guilds", token, map[string]string{
		"name": "ircs://irc.libera.chat:6697/#go,#rust?name=Libera",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create guild: %d %v", rec.Code, out)
	}
	guildID, _ := out["id"].(string)
	if guildID == "" {
		t.Fatalf("guild id missing: %v", out)
	}

	// Duplicate join is idempotent: same connection string -> same guild.
	rec, out = do(t, h, "POST", "/api/v9/guilds", token, map[string]string{
		"name": "ircs://irc.libera.chat:6697/#other",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("rejoin: %d %v", rec.Code, out)
	}
	if out["id"] != guildID {
		t.Fatalf("rejoin returned different guild: %v vs %v", out["id"], guildID)
	}

	// Bad string -> 400.
	rec, out = do(t, h, "POST", "/api/v9/guilds", token, map[string]string{"name": "ftp://host"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad string: %d %v", rec.Code, out)
	}

	// Guild detail shows auto-join channels as Discord channels.
	rec, out = do(t, h, "GET", "/api/v9/guilds/"+guildID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("guild detail: %d %v", rec.Code, out)
	}
	channels, _ := out["channels"].([]any)
	if len(channels) != 2 {
		t.Fatalf("channels: %v", channels)
	}
	if name := channels[0].(map[string]any)["name"]; name != "go" {
		t.Fatalf("channel[0] name: %v", name)
	}
}

// TestGatewayGuildCreateFlow verifies that after a network is joined, the
// gateway READY carries unavailable stubs and GUILD_CREATE fills the rail.
func TestGatewayGuildCreateFlow(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager)
	gw.SetGuildProviders(
		func(u string) ([]any, error) { return netSvc.ReadyGuildPayloads(u), nil },
		netSvc.GuildCreateForUser,
	)

	// Join a network first (no real IRC connection happens - the host has no
	// listener; EnsureConn just spawns a goroutine that fails to connect).
	if _, err := netSvc.Join(user.ID, "ircs://irc.libera.chat:6697/#go,#rust?name=Libera"); err != nil {
		t.Fatal(err)
	}

	h := New(svc, cfg, logger, gw, netSvc, manager)
	srv := httptest.NewServer(h)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway/?v=9&encoding=json"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	recvJSON := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	_ = recvJSON() // HELLO
	if err := conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}

	rdy := recvJSON()
	if rdy["t"] != "READY" {
		t.Fatalf("expected READY, got %v", rdy)
	}
	readyGuilds := rdy["d"].(map[string]any)["guilds"].([]any)
	if len(readyGuilds) != 1 {
		t.Fatalf("expected 1 ready guild, got %v", readyGuilds)
	}
	rg := readyGuilds[0].(map[string]any)
	if u := rg["unavailable"]; u != false {
		t.Fatalf("ready guild should be available: %v", rg)
	}
	// Wire format: channels is a flat array (Gateway Guild Object spec);
	// the client hydrates it into its internal versioned structure itself.
	if _, ok := rg["channels"].([]any); !ok {
		t.Fatalf("ready guild channels must be a flat array: %v", rg["channels"])
	}
	if _, ok := rg["guild_hashes"].(map[string]any); !ok {
		t.Fatalf("ready guild must carry guild_hashes: %v", rg)
	}

	create := recvJSON()
	if create["t"] != "GUILD_CREATE" {
		t.Fatalf("expected GUILD_CREATE after READY, got %v", create)
	}
	d := create["d"].(map[string]any)
	if d["name"] != "Libera" {
		t.Fatalf("guild name: %v", d["name"])
	}
	chans := d["channels"].([]any)
	if len(chans) != 2 {
		t.Fatalf("channels: %v", chans)
	}
	members := d["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("members: %v", members)
	}
}
func TestJoinInvite(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	rec, out := do(t, h, "POST", "/api/v9/invites/ircs%3A%2F%2Firc.libera.chat%3A6697%2F%23go", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("join invite: %d %v", rec.Code, out)
	}
	guild, _ := out["guild"].(map[string]any)
	if guild == nil || guild["id"] == "" {
		t.Fatalf("guild: %v", out)
	}
}
