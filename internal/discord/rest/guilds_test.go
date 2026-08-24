package rest

import (
	"net/http"
	"testing"
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

// TestJoinInvite exercises the paste-an-invite (connection string) flow.
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
