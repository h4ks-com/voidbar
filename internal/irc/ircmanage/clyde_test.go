package ircmanage

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// TestClydeDMForksMigrate verifies the global Clyde thread: per-network
// forks (left over from network recreations) collapse into one thread on
// the pseudo-network, keeping the most active one's history.
func TestClydeDMForksMigrate(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))

	sf := util.NewSnowflake(0, 0)
	// Two forks on different (long-gone) networks, the older one stale.
	old, err := store.EnsureDMChannel("u1", "net-old", "Clyde", sf.New)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.EnsureDMChannel("u1", "net-new", "Clyde", sf.New)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchDMChannel(fresh.ID); err != nil {
		t.Fatal(err)
	}

	dm := manager.clydeDM("u1")
	if dm == nil {
		t.Fatal("no clyde dm")
	}
	if dm.ID != fresh.ID {
		t.Fatalf("kept stale fork %s, want fresh %s", dm.ID, fresh.ID)
	}
	if dm.NetworkID != model.ClydeNetID {
		t.Fatalf("network = %q, want %q", dm.NetworkID, model.ClydeNetID)
	}
	dms, err := store.ListDMChannels("u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(dms) != 1 {
		t.Fatalf("forks survived: %+v", dms)
	}
	if dms[0].ID == old.ID {
		t.Fatal("old fork kept")
	}

	// Idempotent on repeat: same single thread, no churn.
	again := manager.clydeDM("u1")
	if again == nil || again.ID != dm.ID {
		t.Fatalf("repeat call forked: %+v", again)
	}
}

// TestLinkFailureClydeNotice verifies upstream failures surface as a Clyde
// system DM: dial-refused on a dead port yields exactly one notice for a
// run of identical retries (dedupe), re-armed only by a later change.
func TestLinkFailureClydeNotice(t *testing.T) {
	// Grab a port then close the listener: dial-refused, immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertNetwork(&storage.Network{
		ID: "net1", ConnID: "irc://127.0.0.1", Name: "Dead",
		Host: "127.0.0.1", Port: port, TLS: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{
		UserID: "u1", NetworkID: "net1", Nick: "n", JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })
	manager.EnsureConn("u1", "net1")

	notices := func() []string {
		var out []string
		dms, err := store.ListDMChannels("u1")
		if err != nil {
			t.Fatal(err)
		}
		for _, dm := range dms {
			if dm.Nick != "Clyde" {
				continue
			}
			for _, m := range store.ChannelMessages(dm.ID, "", "", 50) {
				if strings.Contains(m.Content, "Connection to ") {
					out = append(out, m.Content)
				}
			}
		}
		return out
	}

	// First notice lands quickly (dial-refused is immediate).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(notices()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no clyde notice for link failure")
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := notices()
	want := fmt.Sprintf("Connection to ircs://127.0.0.1:%d failed:", port)
	if !strings.HasPrefix(got[0], want) {
		t.Fatalf("notice = %q, want prefix %q", got[0], want)
	}

	// Several backoff cycles later: still exactly one - retries dedupe.
	time.Sleep(900 * time.Millisecond)
	if n := len(notices()); n != 1 {
		t.Fatalf("got %d notices after retries, want 1: %q", n, notices())
	}
}
