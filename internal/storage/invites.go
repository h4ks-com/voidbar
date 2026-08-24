package storage

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

type RegInvite struct {
	Code      string    `json:"code"`
	CreatedBy string    `json:"created_by"`
	MaxUses   int       `json:"max_uses"`
	Uses      int       `json:"uses"`
	CreatedAt time.Time `json:"created_at"`
}

func regInviteKey(code string) []byte { return []byte("reginvite/" + code) }

func (s *Storage) CreateRegInvite(inv *RegInvite) error {
	b, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(regInviteKey(inv.Code), b)
	})
}

func (s *Storage) GetRegInvite(code string) (*RegInvite, error) {
	var inv RegInvite
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(regInviteKey(code))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &inv) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (s *Storage) ListRegInvites() ([]*RegInvite, error) {
	var invites []*RegInvite
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("reginvite/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var inv RegInvite
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &inv) }); err != nil {
				return err
			}
			invites = append(invites, &inv)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(invites, func(i, j int) bool { return invites[i].CreatedAt.Before(invites[j].CreatedAt) })
	return invites, nil
}

func (s *Storage) DeleteRegInvite(code string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(regInviteKey(code))
	})
}

func (s *Storage) ConsumeRegInvite(code string) error {
	err := s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(regInviteKey(code))
		if err != nil {
			return err
		}
		var inv RegInvite
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &inv) }); err != nil {
			return err
		}
		if inv.MaxUses > 0 && inv.Uses >= inv.MaxUses {
			return ErrInviteExhausted
		}
		inv.Uses++
		b, err := json.Marshal(inv)
		if err != nil {
			return err
		}
		return txn.Set(regInviteKey(code), b)
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}
