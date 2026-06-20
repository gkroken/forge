package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"forge/internal/auth"
)

// handleUsers serves the user management API:
//
//	GET    /api/v1/users              list users
//	POST   /api/v1/users              create user
//	PUT    /api/v1/users/{username}   update role or disabled
//	DELETE /api/v1/users/{username}   delete user
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Users == nil {
		http.Error(w, `{"error":"user management not enabled"}`, http.StatusNotImplemented)
		return
	}
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/users")
	sub = strings.TrimPrefix(sub, "/")

	switch {
	case r.Method == http.MethodGet && sub == "":
		s.apiListUsers(w, r)
	case r.Method == http.MethodPost && sub == "":
		s.apiCreateUser(w, r)
	case r.Method == http.MethodPut && sub != "":
		s.apiUpdateUser(w, r, sub)
	case r.Method == http.MethodDelete && sub != "":
		s.apiDeleteUser(w, r, sub)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

func (s *Server) apiListUsers(w http.ResponseWriter, _ *http.Request) {
	users, err := s.Users.List()
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if users == nil {
		users = []auth.User{}
	}
	json.NewEncoder(w).Encode(users)
}

type createUserRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role"`
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		jsonError(w, "username and password required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "Reader"
	}
	u, err := s.Users.Create(req.Username, req.Password, req.Role)
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(u)
}

type updateUserRequest struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request, username string) {
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Role != nil {
		if err := s.Users.SetRole(username, *req.Role); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
	}
	if req.Disabled != nil {
		if err := s.Users.SetDisabled(username, *req.Disabled); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
	}
	u, ok, err := s.Users.Get(username)
	if err != nil || !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(u)
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, _ *http.Request, username string) {
	if err := s.Users.Delete(username); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRoles serves the role management API:
//
//	GET    /api/v1/roles         list roles (predefined + custom)
//	POST   /api/v1/roles         create custom role
//	DELETE /api/v1/roles/{name}  delete custom role
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Roles == nil {
		http.Error(w, `{"error":"role management not enabled"}`, http.StatusNotImplemented)
		return
	}
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/roles")
	sub = strings.TrimPrefix(sub, "/")

	switch {
	case r.Method == http.MethodGet && sub == "":
		s.apiListRoles(w, r)
	case r.Method == http.MethodPost && sub == "":
		s.apiCreateRole(w, r)
	case r.Method == http.MethodDelete && sub != "":
		s.apiDeleteRole(w, r, sub)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

type rolesResponse struct {
	Predefined []auth.CustomRole `json:"predefined"`
	Custom     []auth.CustomRole `json:"custom"`
}

func (s *Server) apiListRoles(w http.ResponseWriter, _ *http.Request) {
	custom, err := s.Roles.List()
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if custom == nil {
		custom = []auth.CustomRole{}
	}
	json.NewEncoder(w).Encode(rolesResponse{
		Predefined: auth.PredefinedRoles,
		Custom:     custom,
	})
}

func (s *Server) apiCreateRole(w http.ResponseWriter, r *http.Request) {
	var role auth.CustomRole
	if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Roles.Create(role); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(role)
}

func (s *Server) apiDeleteRole(w http.ResponseWriter, _ *http.Request, name string) {
	if err := s.Roles.Delete(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
