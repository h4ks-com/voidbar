package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func newService(t *testing.T, registration string) *Service {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, util.NewSnowflake(0, 0), registration)
}

func TestRegisterOpen(t *testing.T) {
	svc := newService(t, "open")
	u, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == "" || token == "" {
		t.Fatalf("empty id or token: %+v", u)
	}
	if got, err := svc.ValidateToken(token); err != nil || got.ID != u.ID {
		t.Fatalf("validate token: %v %v", got, err)
	}
}

func TestRegisterClosed(t *testing.T) {
	svc := newService(t, "closed")
	_, _, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("expected ErrRegistrationClosed, got %v", err)
	}
}

func TestRegisterInvite(t *testing.T) {
	svc := newService(t, "invite")
	inv := &storage.RegInvite{Code: "code1", MaxUses: 1, CreatedAt: time.Now()}
	if err := svc.store.CreateRegInvite(inv); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "wrong"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expected ErrInvalidInvite, got %v", err)
	}
	if _, _, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "code1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register("mattf", "mattf@x.io", "hunter2hunter2", "code1"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expected exhausted invite, got %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	svc := newService(t, "open")
	cases := []struct {
		name, user, email, pass string
		want                    error
	}{
		{"bad username upper", "Doesnm", "a@x.io", "hunter2hunter2", ErrInvalidUsername},
		{"bad username short", "d", "a@x.io", "hunter2hunter2", ErrInvalidUsername},
		{"bad username chars", "does nm", "a@x.io", "hunter2hunter2", ErrInvalidUsername},
		{"bad email", "doesnm", "nope", "hunter2hunter2", ErrInvalidEmail},
		{"bad password", "doesnm", "a@x.io", "short", ErrInvalidPassword},
	}
	for _, tc := range cases {
		if _, _, err := svc.Register(tc.user, tc.email, tc.pass, ""); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestRegisterDuplicate(t *testing.T) {
	svc := newService(t, "open")
	if _, _, err := svc.Register("doesnm", "a@x.io", "hunter2hunter2", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register("doesnm", "b@x.io", "hunter2hunter2", ""); !errors.Is(err, storage.ErrUsernameTaken) {
		t.Fatalf("expected ErrUsernameTaken, got %v", err)
	}
	if _, _, err := svc.Register("mattf", "a@x.io", "hunter2hunter2", ""); !errors.Is(err, storage.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestLogin(t *testing.T) {
	svc := newService(t, "open")
	if _, _, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", ""); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"doesnm", "doesnm@0ut0f.space"} {
		u, token, err := svc.Login(login, "hunter2hunter2")
		if err != nil {
			t.Fatalf("login %q: %v", login, err)
		}
		if u.Username != "doesnm" || token == "" {
			t.Fatalf("bad login result: %+v", u)
		}
	}
	if _, _, err := svc.Login("doesnm", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if _, _, err := svc.Login("ghost", "hunter2hunter2"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLogout(t *testing.T) {
	svc := newService(t, "open")
	_, token, err := svc.Register("doesnm", "a@x.io", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateToken(token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials after logout, got %v", err)
	}
}
