package storage

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
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

// Channel is a Discord-facing channel backed by an IRC channel or query.
// IDs are snowflakes: they end up in URLs (/channels/<guild>/<channel>), so
// the IRC name itself (with its # and friends) must never appear there.
type Channel struct {
	ID        string    `json:"id"`
	NetworkID string    `json:"network_id"`
	IRCName   string    `json:"irc_name"` // "#go" or a bare nick for DMs
	Name      string    `json:"name"`     // "go" / the nick
	CreatedAt time.Time `json:"created_at"`
}

func chanKey(id string) []byte { return []byte("chan/" + id) }
func chanNetKey(netID, ircName string) []byte {
	return []byte("channet/" + netID + "/" + ircName)
}

// DMChannel is a Discord-facing DM channel backed by an IRC query with one
// upstream user. Per (owner, network, nick) - each bouncer user carries
// their own query threads, mirroring per-connection IRC state.
type DMChannel struct {
	ID        string    `json:"id"`
	NetworkID string    `json:"network_id"`
	OwnerID   string    `json:"owner_id"`
	Nick      string    `json:"nick"`
	LastMsgAt time.Time `json:"last_msg_at"`
	CreatedAt time.Time `json:"created_at"`
}

func dmKey(id string) []byte { return []byte("dm/" + id) }
func dmOwnerKey(owner, netID, nick string) []byte {
	return []byte("dmown/" + owner + "/" + netID + "/" + nick)
}
func dmOwnerPrefix(owner string) []byte { return []byte("dmown/" + owner + "/") }

// EnsureChannel registers the network's IRC channel and returns its stable,
// URL-safe snowflake id. Idempotent by (network, irc name).
func (s *Storage) EnsureChannel(netID, ircName string, newID func() string) (*Channel, error) {
	var ch Channel
	err := s.db.Update(func(txn *badger.Txn) error {
		idx, err := txn.Get(chanNetKey(netID, ircName))
		if err == nil {
			return idx.Value(func(id []byte) error {
				item, err := txn.Get(chanKey(string(id)))
				if err != nil {
					return err
				}
				return item.Value(func(val []byte) error { return json.Unmarshal(val, &ch) })
			})
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		ch = Channel{
			ID:        newID(),
			NetworkID: netID,
			IRCName:   ircName,
			Name:      strings.TrimPrefix(ircName, "#"),
			CreatedAt: time.Now().UTC(),
		}
		b, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		if err := txn.Set(chanKey(ch.ID), b); err != nil {
			return err
		}
		return txn.Set(chanNetKey(netID, ircName), []byte(ch.ID))
	})
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

func (s *Storage) GetChannel(id string) (*Channel, error) {
	var ch Channel
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(chanKey(id))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &ch) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// ChannelsByIRC resolves channel ids for a network's IRC channel names,
// registering any that are missing. Returns channels in the input order.
func (s *Storage) ChannelsByIRC(netID string, ircNames []string, newID func() string) ([]*Channel, error) {
	out := make([]*Channel, 0, len(ircNames))
	for _, name := range ircNames {
		ch, err := s.EnsureChannel(netID, name, newID)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

// EnsureDMChannel returns (creating if needed) the owner's DM thread with
// an upstream nick on a network. Idempotent by (owner, network, nick).
func (s *Storage) EnsureDMChannel(owner, netID, nick string, newID func() string) (*DMChannel, error) {
	var dm DMChannel
	err := s.db.Update(func(txn *badger.Txn) error {
		idx, err := txn.Get(dmOwnerKey(owner, netID, nick))
		if err == nil {
			return idx.Value(func(id []byte) error {
				item, err := txn.Get(dmKey(string(id)))
				if err != nil {
					return err
				}
				return item.Value(func(val []byte) error { return json.Unmarshal(val, &dm) })
			})
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		dm = DMChannel{
			ID:        newID(),
			NetworkID: netID,
			OwnerID:   owner,
			Nick:      nick,
			CreatedAt: time.Now().UTC(),
		}
		b, err := json.Marshal(dm)
		if err != nil {
			return err
		}
		if err := txn.Set(dmKey(dm.ID), b); err != nil {
			return err
		}
		return txn.Set(dmOwnerKey(owner, netID, nick), []byte(dm.ID))
	})
	if err != nil {
		return nil, err
	}
	return &dm, nil
}

// GetDMChannel loads a DM channel by id.
func (s *Storage) GetDMChannel(id string) (*DMChannel, error) {
	var dm DMChannel
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(dmKey(id))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &dm) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &dm, nil
}

// ListDMChannels returns every DM thread a user owns, newest activity
// first (the order the Direct Messages list renders in).
func (s *Storage) ListDMChannels(owner string) ([]*DMChannel, error) {
	var out []*DMChannel
	// Resolve via the owner index, then load each record; LastMsgAt lives
	// only in the record, so sorting happens after the load.
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := dmOwnerPrefix(owner)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var dm DMChannel
			ok := item.Value(func(id []byte) error {
				rec, err := txn.Get(dmKey(string(id)))
				if err != nil {
					return nil // orphan index entry: skip, don't abort
				}
				return rec.Value(func(val []byte) error { return json.Unmarshal(val, &dm) })
			}) == nil
			if ok {
				out = append(out, &dm)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMsgAt.After(out[j].LastMsgAt) })
	return out, nil
}

// TouchDMChannel bumps a DM thread's activity timestamp (called on every
// relayed message so ListDMChannels ordering tracks the conversation).
func (s *Storage) TouchDMChannel(id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(dmKey(id))
		if err != nil {
			return err
		}
		var dm DMChannel
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &dm) }); err != nil {
			return err
		}
		dm.LastMsgAt = time.Now().UTC()
		b, err := json.Marshal(dm)
		if err != nil {
			return err
		}
		return txn.Set(dmKey(id), b)
	})
}

// DeleteDMChannel removes a DM thread (record + owner index). History in
// the replay buffer is left in place: a new query from the same peer
// recreates the thread with the old snowflake and the conversation
// continues where it left off.
func (s *Storage) DeleteDMChannel(id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(dmKey(id))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		var dm DMChannel
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &dm) }); err != nil {
			return err
		}
		if err := txn.Delete(dmKey(id)); err != nil {
			return err
		}
		return txn.Delete(dmOwnerKey(dm.OwnerID, dm.NetworkID, dm.Nick))
	})
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

// DeleteMembership removes a single user's membership on a network
// (Discord "leave guild"). Idempotent.
func (s *Storage) DeleteMembership(netID, userID string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(memberKey(netID, userID))
	})
}

// MembershipAddChannel merges an IRC channel into the member's auto-join
// list (case-insensitive dedup). Returns the updated membership.
func (s *Storage) MembershipAddChannel(netID, userID, ircName string) (*Membership, error) {
	var m Membership
	err := s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(memberKey(netID, userID))
		if err != nil {
			return err
		}
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
			return err
		}
		for _, ch := range m.AutoJoin {
			if strings.EqualFold(ch, ircName) {
				return nil
			}
		}
		m.AutoJoin = append(m.AutoJoin, ircName)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return txn.Set(memberKey(netID, userID), b)
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// MembershipRemoveChannel drops an IRC channel from the member's auto-join
// list (channel delete / failed-join rollback). Idempotent.
func (s *Storage) MembershipRemoveChannel(netID, userID, ircName string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(memberKey(netID, userID))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		var m Membership
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
			return err
		}
		out := m.AutoJoin[:0]
		for _, ch := range m.AutoJoin {
			if !strings.EqualFold(ch, ircName) {
				out = append(out, ch)
			}
		}
		m.AutoJoin = out
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return txn.Set(memberKey(netID, userID), b)
	})
}

// GetChannelByIRC resolves a channel by its (network, irc name) without
// creating one (unlike EnsureChannel). Used by join-failure rollback.
func (s *Storage) GetChannelByIRC(netID, ircName string) (*Channel, error) {
	var ch Channel
	err := s.db.View(func(txn *badger.Txn) error {
		idx, err := txn.Get(chanNetKey(netID, ircName))
		if err != nil {
			return err
		}
		return idx.Value(func(id []byte) error {
			item, err := txn.Get(chanKey(string(id)))
			if err != nil {
				return err
			}
			return item.Value(func(val []byte) error { return json.Unmarshal(val, &ch) })
		})
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// DeleteNetworkCascade garbage-collects a network nobody is a member of
// anymore: the network record, every channel registry entry (both the
// chan/<id> record and the channet/<net>/<irc> index) and each channel's
// replay buffer. Callers only invoke it after the last membership is gone.
func (s *Storage) DeleteNetworkCascade(netID string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		if err := txn.Delete(networkKey(netID)); err != nil {
			return err
		}
		// Collect first, delete after: mutating while iterating a badger
		// txn skips entries.
		var memberKeys, chanIdxKeys, chanIDs, prefillKeys [][]byte
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		mp := []byte("member/" + netID + "/")
		for it.Seek(mp); it.ValidForPrefix(mp); it.Next() {
			memberKeys = append(memberKeys, it.Item().KeyCopy(nil))
		}
		pp := []byte("chatprefill:" + netID + ":")
		for it.Seek(pp); it.ValidForPrefix(pp); it.Next() {
			prefillKeys = append(prefillKeys, it.Item().KeyCopy(nil))
		}
		cp := []byte("channet/" + netID + "/")
		for it.Seek(cp); it.ValidForPrefix(cp); it.Next() {
			item := it.Item()
			chanIdxKeys = append(chanIdxKeys, item.KeyCopy(nil))
			_ = item.Value(func(id []byte) error {
				chanIDs = append(chanIDs, append([]byte(nil), id...))
				return nil
			})
		}
		it.Close()
		for _, k := range memberKeys {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		for _, k := range prefillKeys {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		for _, k := range chanIdxKeys {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		for _, id := range chanIDs {
			if err := txn.Delete(chanKey(string(id))); err != nil {
				return err
			}
			var msgKeys [][]byte
			prefix := msgPrefix(string(id))
			it2 := txn.NewIterator(badger.DefaultIteratorOptions)
			for it2.Seek(prefix); it2.ValidForPrefix(prefix); it2.Next() {
				msgKeys = append(msgKeys, it2.Item().KeyCopy(nil))
			}
			it2.Close()
			for _, k := range msgKeys {
				if err := txn.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
