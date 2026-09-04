package auth

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

var (
	ErrRegistrationClosed = errors.New("registration is closed")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidUsername    = errors.New("username must be 2-32 chars: a-z 0-9 . _")
	ErrInvalidEmail       = errors.New("invalid email")
	ErrInvalidPassword    = errors.New("password must be 8-128 chars")
)

var (
	usernameRe = regexp.MustCompile(`^[a-z0-9._]{2,32}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

type Service struct {
	store        *storage.Storage
	sf           *util.Snowflake
	registration string
	adminKey     []byte
}

func New(store *storage.Storage, sf *util.Snowflake, registration string) *Service {
	return &Service{store: store, sf: sf, registration: registration}
}

// SetAdminKey arms the master-key admin API (see the rest package's
// /api/v9/admin/users endpoints). The key is the instance master.key;
// without it those endpoints refuse everything.
func (s *Service) SetAdminKey(key []byte) { s.adminKey = []byte(hex.EncodeToString(key)) }

// ListUsers exposes the user list to the admin API (the rest server has
// no storage handle of its own).
func (s *Service) ListUsers() ([]*storage.User, error) { return s.store.ListUsers() }

// AdminAuthorized checks an X-Master-Key value in constant time. Empty
// (unset) keys authorize nobody.
func (s *Service) AdminAuthorized(headerKey string) bool {
	if len(s.adminKey) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(headerKey), s.adminKey) == 1
}

func (s *Service) Registration() string { return s.registration }

// CreateUserAdmin creates a user bypassing the registration gate (the
// CLI and the master-key admin API both land here). Mirrors the CLI
// semantics: the first user on a fresh instance becomes an admin.
func (s *Service) CreateUserAdmin(username, email, password string, forceAdmin bool) (*storage.User, error) {
	username = strings.TrimSpace(strings.ToLower(username))
	email = strings.ToLower(strings.TrimSpace(email))
	if !usernameRe.MatchString(username) {
		return nil, ErrInvalidUsername
	}
	if !emailRe.MatchString(email) {
		return nil, ErrInvalidEmail
	}
	if len(password) < 8 || len(password) > 128 {
		return nil, ErrInvalidPassword
	}
	if _, err := s.store.GetUserByUsername(username); err == nil {
		return nil, storage.ErrUsernameTaken
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if _, err := s.store.GetUserByEmail(email); err == nil {
		return nil, storage.ErrEmailTaken
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	hash, err := util.HashPassword(password)
	if err != nil {
		return nil, err
	}
	count, err := s.store.UserCount()
	if err != nil {
		return nil, err
	}
	u := &storage.User{
		ID:        s.sf.New(),
		Username:  username,
		Email:     email,
		PassHash:  hash,
		IsAdmin:   forceAdmin || count == 0,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// Fingerprint returns a fresh legacy auth fingerprint ("snowflake.hash" per
// the userdocs /auth/fingerprint contract). It is opaque to Voidbar and only
// needs the right shape for clients to carry through the login flow.
func (s *Service) Fingerprint() string {
	hash, err := util.RandomToken(27)
	if err != nil {
		hash = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return s.sf.New() + "." + hash
}

func (s *Service) Register(username, email, password string) (*storage.User, string, error) {
	if s.registration == "closed" {
		return nil, "", ErrRegistrationClosed
	}
	username = strings.TrimSpace(username)
	email = strings.ToLower(strings.TrimSpace(email))
	if !usernameRe.MatchString(username) {
		return nil, "", ErrInvalidUsername
	}
	if !emailRe.MatchString(email) {
		return nil, "", ErrInvalidEmail
	}
	if len(password) < 8 || len(password) > 128 {
		return nil, "", ErrInvalidPassword
	}
	if _, err := s.store.GetUserByUsername(username); err == nil {
		return nil, "", storage.ErrUsernameTaken
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, "", err
	}
	if _, err := s.store.GetUserByEmail(email); err == nil {
		return nil, "", storage.ErrEmailTaken
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, "", err
	}
	hash, err := util.HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	u := &storage.User{
		ID:        s.sf.New(),
		Username:  username,
		Email:     email,
		PassHash:  hash,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateUser(u); err != nil {
		return nil, "", err
	}
	token, err := s.newSession(u.ID)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *Service) Login(login, password string) (*storage.User, string, error) {
	login = strings.TrimSpace(login)
	u, err := s.store.GetUserByEmail(login)
	if errors.Is(err, storage.ErrNotFound) {
		u, err = s.store.GetUserByUsername(login)
	}
	if err != nil {
		return nil, "", ErrInvalidCredentials
	}
	ok, err := util.VerifyPassword(password, u.PassHash)
	if err != nil || !ok {
		return nil, "", ErrInvalidCredentials
	}
	token, err := s.newSession(u.ID)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *Service) Logout(token string) error {
	return s.store.DeleteSession(util.SHA256Hex(token))
}

func (s *Service) ValidateToken(token string) (*storage.User, error) {
	hash := util.SHA256Hex(token)
	sess, err := s.store.GetSession(hash)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	u, err := s.store.GetUserByID(sess.UserID)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := s.store.TouchSession(hash); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Service) newSession(userID string) (string, error) {
	token, err := util.RandomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	now := time.Now().UTC()
	sess := &storage.Session{
		TokenHash: util.SHA256Hex(token),
		UserID:    userID,
		CreatedAt: now,
		LastSeen:  now,
	}
	if err := s.store.CreateSession(sess); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}
