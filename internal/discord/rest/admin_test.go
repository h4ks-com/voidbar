package rest

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func newAdminTestServer(t *testing.T, key string) (*httptest.Server, *auth.Service) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	svc := auth.New(store, util.NewSnowflake(0, 0), "closed")
	svc.SetAdminKey([]byte(key))
	cfg := &config.Config{}
	cfg.Server.PublicURL = "http://test"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(svc, cfg, logger, nil, nil, nil)
	return httptest.NewServer(h), svc
}

func TestAdminCreateUser(t *testing.T) {
	srv, _ := newAdminTestServer(t, "k1")
	defer srv.Close()

	body := func() *bytes.Reader {
		b, _ := json.Marshal(map[string]any{"username": "wildcat", "email": "wild@cat.io", "password": "hunter2hunter2"})
		return bytes.NewReader(b)
	}

	// no key -> 401
	resp, err := http.Post(srv.URL+"/api/v9/admin/users", "application/json", body())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: got %d want 401", resp.StatusCode)
	}

	// wrong key -> 401
	req, _ := http.NewRequest("POST", srv.URL+"/api/v9/admin/users", body())
	req.Header.Set("X-Master-Key", "wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d want 401", resp.StatusCode)
	}

	// correct key -> 201; the instance is empty so the user becomes admin
	req, _ = http.NewRequest("POST", srv.URL+"/api/v9/admin/users", body())
	req.Header.Set("X-Master-Key", "k1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		ID      string `json:"id"`
		IsAdmin bool   `json:"admin"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	if resp.StatusCode != http.StatusCreated || created.ID == "" || !created.IsAdmin {
		t.Fatalf("create: status=%d id=%q admin=%v", resp.StatusCode, created.ID, created.IsAdmin)
	}

	// list requires the key and sees the user
	req, _ = http.NewRequest("GET", srv.URL+"/api/v9/admin/users", nil)
	req.Header.Set("X-Master-Key", "k1")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var users []map[string]any
	json.NewDecoder(resp2.Body).Decode(&users)
	if resp2.StatusCode != http.StatusOK || len(users) != 1 {
		t.Fatalf("list: status=%d users=%d", resp2.StatusCode, len(users))
	}
}
