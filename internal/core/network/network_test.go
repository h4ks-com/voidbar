package network

import (
	"testing"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
	"io"
	"log/slog"
)

func TestJoinCreatesMembershipAndGuilds(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Minimal user record so Join can read the username.
	u := &storage.User{ID: "user1", Username: "doesnm"}
	if err := store.CreateUser(u); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger)
	svc := NewService(store, gw, util.NewSnowflake(0, 0), manager)

	net, err := svc.Join("user1", "ircs://irc.libera.chat:6697/#go,#rust?name=Libera")
	if err != nil {
		t.Fatal(err)
	}
	if net.ID == "" || net.ConnID != "ircs://irc.libera.chat:6697" {
		t.Fatalf("net: %+v", net)
	}

	mem, err := svc.MembershipFor("user1", net.ID)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	if mem.Nick != "doesnm" {
		t.Fatalf("nick: %q", mem.Nick)
	}
	if len(mem.AutoJoin) != 2 {
		t.Fatalf("autojoin: %v", mem.AutoJoin)
	}

	guilds, err := svc.GuildsForUser("user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 1 {
		t.Fatalf("guilds: %v", guilds)
	}

	creates := svc.GuildCreateForUser("user1")
	if len(creates) != 1 {
		t.Fatalf("guild create payloads: %d", len(creates))
	}
}

func TestJoinIdempotentSameGuild(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	u := &storage.User{ID: "user1", Username: "doesnm"}
	if err := store.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger)
	svc := NewService(store, gw, util.NewSnowflake(0, 0), manager)

	a, _ := svc.Join("user1", "ircs://irc.libera.chat:6697/#go?name=Libera")
	b, _ := svc.Join("user1", "ircs://irc.libera.chat:6697/#rust")
	if a.ID != b.ID {
		t.Fatalf("same conn string must yield same guild: %s vs %s", a.ID, b.ID)
	}
}