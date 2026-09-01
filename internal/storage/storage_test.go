package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/util"
)

func openStore(t *testing.T) *Storage {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newUser(t *testing.T, s *Storage, name, email string) *User {
	t.Helper()
	u := &User{
		ID:        util.NewSnowflake(0, 0).New(),
		Username:  name,
		Email:     email,
		PassHash:  "x",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCreateAndGetUser(t *testing.T) {
	s := openStore(t)
	u := newUser(t, s, "doesnm", "doesnm@0ut0f.space")

	got, err := s.GetUserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "doesnm" || got.Email != "doesnm@0ut0f.space" {
		t.Fatalf("mismatch: %+v", got)
	}

	got, err = s.GetUserByUsername("doesnm")
	if err != nil || got.ID != u.ID {
		t.Fatalf("by username: %v %v", got, err)
	}

	got, err = s.GetUserByEmail("doesnm@0ut0f.space")
	if err != nil || got.ID != u.ID {
		t.Fatalf("by email: %v %v", got, err)
	}

	if _, err := s.GetUserByUsername("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCreateUserUnique(t *testing.T) {
	s := openStore(t)
	newUser(t, s, "doesnm", "a@x.io")
	err := s.CreateUser(&User{ID: "2", Username: "doesnm", Email: "b@x.io", CreatedAt: time.Now()})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("expected ErrUsernameTaken, got %v", err)
	}
	err = s.CreateUser(&User{ID: "3", Username: "other", Email: "a@x.io", CreatedAt: time.Now()})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := openStore(t)
	u := newUser(t, s, "doesnm", "a@x.io")
	sess := &Session{TokenHash: "abc", UserID: u.ID, CreatedAt: time.Now(), LastSeen: time.Now()}
	if err := s.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession("abc")
	if err != nil || got.UserID != u.ID {
		t.Fatalf("get: %v %v", got, err)
	}
	if err := s.TouchSession("abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession("abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListUsersSorted(t *testing.T) {
	s := openStore(t)
	sf := util.NewSnowflake(0, 0)
	for _, name := range []string{"b", "c", "a"} {
		u := &User{ID: sf.New(), Username: name, Email: name + "@x.io", CreatedAt: time.Now()}
		if err := s.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
	for i := 1; i < len(users); i++ {
		if users[i-1].ID > users[i].ID {
			t.Fatal("users not sorted by id")
		}
	}
}
