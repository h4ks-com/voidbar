package storage

import (
	"encoding/json"
	"errors"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

type Session struct {
	TokenHash string    `json:"token_hash"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

func sessionKey(hash string) []byte { return []byte("session/" + hash) }

func (s *Storage) CreateSession(sess *Session) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(sessionKey(sess.TokenHash), b)
	})
}

func (s *Storage) GetSession(tokenHash string) (*Session, error) {
	var sess Session
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(sessionKey(tokenHash))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error { return json.Unmarshal(val, &sess) })
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *Storage) TouchSession(tokenHash string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(sessionKey(tokenHash))
		if err != nil {
			return err
		}
		var sess Session
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &sess) }); err != nil {
			return err
		}
		sess.LastSeen = time.Now().UTC()
		b, err := json.Marshal(sess)
		if err != nil {
			return err
		}
		return txn.Set(sessionKey(tokenHash), b)
	})
}

func (s *Storage) DeleteSession(tokenHash string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		if err := txn.Delete(sessionKey(tokenHash)); err != nil {
			return err
		}
		return nil
	})
}
