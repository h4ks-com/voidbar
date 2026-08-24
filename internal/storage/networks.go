package storage

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// Network is the shared reflection of an IRC connection string. ID is the
// opaque Discord-facing guild id (a snowflake); ConnID is the canonical
// connection-string identity (host:port + TLS) used to dedupe joins.
type Network struct {
	ID        string    `json:"id"`      // guild id, what Discord sees
	ConnID    string    `json:"conn_id"` // canonical connection string id
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	TLS       bool      `json:"tls"`
	Password  string    `json:"password,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Membership ties a user to a network with their per-connection identity.
// Upstream connections live on (user, network): each member connects with
// their own nick, never sharing a socket.
type Membership struct {
	UserID    string    `json:"user_id"`
	NetworkID string    `json:"network_id"`
	Nick      string    `json:"nick"`
	Username  string    `json:"username"`
	Realname  string    `json:"realname"`
	AutoJoin  []string  `json:"auto_join,omitempty"`
	JoinedAt  time.Time `json:"joined_at"`
}

func networkKey(id string) []byte     { return []byte("network/" + id) }
func netConnKey(connID string) []byte { return []byte("netconn/" + connID) }
func memberKey(netID, userID string) []byte {
	return []byte("member/" + netID + "/" + userID)
}

// UpsertNetwork writes the network record and its connection-string index.
func (s *Storage) UpsertNetwork(n *Network) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(networkKey(n.ID), b); err != nil {
			return err
		}
		return txn.Set(netConnKey(n.ConnID), []byte(n.ID))
	})
}

// NetworkByConnID resolves a network from its canonical connection-string id.
func (s *Storage) NetworkByConnID(connID string) (*Network, error) {
	var guildID []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(netConnKey(connID))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { guildID = append(guildID[:0], val...); return nil })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetNetwork(string(guildID))
}

func (s *Storage) GetNetwork(id string) (*Network, error) {
	var n Network
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(networkKey(id))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &n) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return &n, err
}

func (s *Storage) ListNetworks() ([]*Network, error) {
	var nets []*Network
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("network/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var n Network
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &n) }); err != nil {
				return err
			}
			nets = append(nets, &n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i].ID < nets[j].ID })
	return nets, nil
}

func (s *Storage) UpsertMembership(m *Membership) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(memberKey(m.NetworkID, m.UserID), b)
	})
}

func (s *Storage) GetMembership(netID, userID string) (*Membership, error) {
	var m Membership
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(memberKey(netID, userID))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &m) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return &m, err
}

func (s *Storage) ListMemberships(netID string) ([]*Membership, error) {
	var ms []*Membership
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("member/" + netID + "/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var m Membership
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
				return err
			}
			ms = append(ms, &m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].UserID < ms[j].UserID })
	return ms, nil
}

// ListMembershipsForUser returns every membership of a user across networks,
// for READY/guild list assembly.
func (s *Storage) ListMembershipsForUser(userID string) ([]*Membership, error) {
	var ms []*Membership
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("member/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var m Membership
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
				return err
			}
			if m.UserID == userID {
				ms = append(ms, &m)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].NetworkID < ms[j].NetworkID })
	return ms, nil
}
