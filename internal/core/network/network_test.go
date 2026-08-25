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
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
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
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	svc := NewService(store, gw, util.NewSnowflake(0, 0), manager)

	a, _ := svc.Join("user1", "ircs://irc.libera.chat:6697/#go?name=Libera")
	b, _ := svc.Join("user1", "ircs://irc.libera.chat:6697/#rust")
	if a.ID != b.ID {
		t.Fatalf("same conn string must yield same guild: %s vs %s", a.ID, b.ID)
	}
}

func TestLeaveRemovesMembershipAndGCsNetwork(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	u2 := &storage.User{ID: "user2", Username: "other"}
	if err := store.CreateUser(u2); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	svc := NewService(store, gw, util.NewSnowflake(0, 0), manager)

	net, err := svc.Join("user2", "ircs://irc.libera.chat:6697/#go?name=Libera")
	if err != nil {
		t.Fatal(err)
	}
	chans, err := svc.ChannelsFor(net.ID, []string{"#go"})
	if err != nil || len(chans) != 1 {
		t.Fatalf("channels: %v %v", chans, err)
	}
	if err := store.AppendMessage(storage.BufferedMessage{ID: "1", ChannelID: chans[0].ID, Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Leave("user2", net.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MembershipFor("user2", net.ID); err != storage.ErrNotFound {
		t.Fatalf("membership must be gone, got %v", err)
	}
	// Last member left: the whole network (channels + buffers) is GC'd.
	if _, err := svc.Network(net.ID); err != storage.ErrNotFound {
		t.Fatalf("network must be GC'd, got %v", err)
	}
	if _, err := store.GetChannel(chans[0].ID); err != storage.ErrNotFound {
		t.Fatalf("channel must be GC'd, got %v", err)
	}
	if msgs := store.ChannelMessages(chans[0].ID, "", "", 50); len(msgs) != 0 {
		t.Fatalf("buffer must be GC'd, got %d", len(msgs))
	}

	// Leaving a network you're not in is a 404-shaped error.
	if err := svc.Leave("user2", net.ID); err != storage.ErrNotFound {
		t.Fatalf("second leave: %v", err)
	}
}

func TestLeaveKeepsNetworkWhileOtherMembersRemain(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, u := range []*storage.User{
		{ID: "user1", Username: "doesnm", Email: "doesnm@example.com"},
		{ID: "user2", Username: "other", Email: "other@example.com"},
	} {
		if err := store.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	svc := NewService(store, gw, util.NewSnowflake(0, 0), manager)

	net, err := svc.Join("user1", "ircs://irc.libera.chat:6697/#go?name=Libera")
	if err != nil {
		t.Fatal(err)
	}
	// Same connection string: user2 joins the same network.
	if _, err := svc.Join("user2", "ircs://irc.libera.chat:6697/#go"); err != nil {
		t.Fatal(err)
	}

	if err := svc.Leave("user1", net.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Network(net.ID); err != nil {
		t.Fatalf("network must survive while user2 is a member: %v", err)
	}
	if _, err := svc.MembershipFor("user2", net.ID); err != nil {
		t.Fatalf("user2 membership must survive: %v", err)
	}
}
