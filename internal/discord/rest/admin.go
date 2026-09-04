package rest

import (
	"encoding/json"
	"net/http"

	"github.com/h4ks-com/voidbar/internal/storage"
)

type adminUserPayload struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"admin"`
	ID       string `json:"id"`
}

func adminPayload(u *storage.User) adminUserPayload {
	return adminUserPayload{Username: u.Username, Email: u.Email, IsAdmin: u.IsAdmin, ID: u.ID}
}

// handleAdminCreateUser implements POST /api/v9/admin/users: user
// provisioning while the server holds the storage lock (docker exec,
// remote management). The registration gate does not apply; the master
// key replaces it.
func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.auth.AdminAuthorized(r.Header.Get("X-Master-Key")) {
		jsonError(w, http.StatusUnauthorized, "invalid master key")
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	u, err := s.auth.CreateUserAdmin(req.Username, req.Email, req.Password, req.Admin)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, adminPayload(u))
}

// handleAdminListUsers implements GET /api/v9/admin/users.
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.auth.AdminAuthorized(r.Header.Get("X-Master-Key")) {
		jsonError(w, http.StatusUnauthorized, "invalid master key")
		return
	}
	users, err := s.auth.ListUsers()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	out := make([]adminUserPayload, 0, len(users))
	for _, u := range users {
		out = append(out, adminPayload(u))
	}
	writeJSON(w, http.StatusOK, out)
}
