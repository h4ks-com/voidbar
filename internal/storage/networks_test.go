package storage

import (
	"errors"
	"testing"
	"time"
)

func TestNetworkUpsertGet(t *testing.T) {
	s := openStore(t)
	n := &Network{
		ID:        "123456789",
		ConnID:    "ircs://irc.libera.chat:6697",
		Name:      "Libera",
		Host:      "irc.libera.chat",
		Port:      6697,
		TLS:       true,
		CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.UpsertNetwork(n); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNetwork(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Libera" || got.Port != 6697 {
		t.Fatalf("got: %+v", got)
	}
	if _, err := s.GetNetwork("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Resolution by canonical connection string id.
	byConn, err := s.NetworkByConnID(n.ConnID)
	if err != nil || byConn.ID != n.ID {
		t.Fatalf("by conn id: %+v err=%v", byConn, err)
	}
	if _, err := s.NetworkByConnID("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for conn id, got %v", err)
	}
}

func TestMembershipPerUser(t *testing.T) {
	s := openStore(t)
	netID := "ircs://irc.libera.chat:6697"
	for _, u := range []struct{ uid, nick string }{
		{"u1", "doesnm"},
		{"u2", "mattf"},
	} {
		m := &Membership{
			UserID:    u.uid,
			NetworkID: netID,
			Nick:      u.nick,
			AutoJoin:  []string{"#go"},
			JoinedAt:  time.Now().UTC(),
		}
		if err := s.UpsertMembership(m); err != nil {
			t.Fatal(err)
		}
	}

	ms, err := s.ListMemberships(netID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("expected 2 memberships, got %d", len(ms))
	}

	all, err := s.ListMembershipsForUser("u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Nick != "doesnm" {
		t.Fatalf("u1 memberships: %+v", all)
	}

	m, err := s.GetMembership(netID, "u2")
	if err != nil || m.Nick != "mattf" {
		t.Fatalf("u2: %+v err=%v", m, err)
	}
	if _, err := s.GetMembership(netID, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListNetworksSorted(t *testing.T) {
	s := openStore(t)
	for _, id := range []string{"b", "a", "c"} {
		if err := s.UpsertNetwork(&Network{ID: id, ConnID: "conn/" + id, Host: id}); err != nil {
			t.Fatal(err)
		}
	}
	nets, err := s.ListNetworks()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(nets); i++ {
		if nets[i-1].ID > nets[i].ID {
			t.Fatal("networks not sorted")
		}
	}
}
