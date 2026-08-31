package ircmanage

import (
	"io"
	"log/slog"
	"testing"

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
