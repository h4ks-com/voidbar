package storage

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username already taken")
	ErrEmailTaken    = errors.New("email already taken")
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	PassHash  string    `json:"pass_hash"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

func userKey(id string) []byte         { return []byte("user/id/" + id) }
func userNameKey(name string) []byte   { return []byte("user/name/" + strings.ToLower(name)) }
func userEmailKey(email string) []byte { return []byte("user/email/" + strings.ToLower(email)) }

func (s *Storage) CreateUser(u *User) error {
	return s.db.Update(func(txn *badger.Txn) error {
		if _, err := txn.Get(userNameKey(u.Username)); err == nil {
			return ErrUsernameTaken
		} else if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		if _, err := txn.Get(userEmailKey(u.Email)); err == nil {
			return ErrEmailTaken
		} else if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		if err := txn.Set(userNameKey(u.Username), []byte(u.ID)); err != nil {
			return err
		}
		if err := txn.Set(userEmailKey(u.Email), []byte(u.ID)); err != nil {
			return err
		}
		b, err := json.Marshal(u)
		if err != nil {
			return err
		}
		return txn.Set(userKey(u.ID), b)
	})
}

func (s *Storage) getUser(txn *badger.Txn, key []byte) (*User, error) {
	item, err := txn.Get(key)
	if err != nil {
		return nil, err
	}
	var u User
	if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &u) }); err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Storage) GetUserByID(id string) (*User, error) {
	var u *User
	err := s.db.View(func(txn *badger.Txn) error {
		var err error
		u, err = s.getUser(txn, userKey(id))
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Storage) GetUserByUsername(name string) (*User, error) {
	var u *User
	err := s.db.View(func(txn *badger.Txn) error {
		id, err := txn.Get(userNameKey(name))
		if err != nil {
			return err
		}
		return id.Value(func(val []byte) error {
			var gerr error
			u, gerr = s.getUser(txn, userKey(string(val)))
			return gerr
		})
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Storage) GetUserByEmail(email string) (*User, error) {
	var u *User
	err := s.db.View(func(txn *badger.Txn) error {
		id, err := txn.Get(userEmailKey(email))
		if err != nil {
			return err
		}
		return id.Value(func(val []byte) error {
			var gerr error
			u, gerr = s.getUser(txn, userKey(string(val)))
			return gerr
		})
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Storage) ListUsers() ([]*User, error) {
	var users []*User
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("user/id/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var u User
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &u) }); err != nil {
				return err
			}
			users = append(users, &u)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	return users, nil
}

func (s *Storage) UserCount() (int, error) {
	users, err := s.ListUsers()
	return len(users), err
}
