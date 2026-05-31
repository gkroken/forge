package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"forge/internal/auth"
)

// handleTokens serves the token management API:
//
//	POST   /api/v1/tokens        create (bootstrap or admin-authenticated)
//	GET    /api/v1/tokens        list   (admin)
//	DELETE /api/v1/tokens/{id}   revoke (admin)
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Token API is unavailable when auth is not configured.
	if s.Auth == nil {
		http.Error(w, `{"error":"auth not enabled; start forge with -auth"}`,
			http.StatusNotImplemented)
		return
	}

	// Route by method and sub-path.
	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/tokens")
	sub = strings.TrimPrefix(sub, "/")

	switch {
	case r.Method == http.MethodPost && sub == "":
		s.createToken(w, r)
	case r.Method == http.MethodGet && sub == "":
		s.listTokens(w, r)
	case r.Method == http.MethodDelete && sub != "":
		s.revokeToken(w, r, sub)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

type createTokenRequest struct {
	Description string      `json:"description"`
	Grants      []auth.Grant `json:"grants"`
	ExpiresAt   *time.Time  `json:"expires_at,omitempty"`
}

type createTokenResponse struct {
	auth.Token
	Secret string `json:"secret"` // shown once
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	n, err := s.Auth.Count()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Bootstrap: first token may be created without authentication.
	// After that, admin role is required.
	if n > 0 && !s.Enforcer.RequireAdmin(w, r) {
		return
	}

	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Description == "" {
		req.Description = "unnamed token"
	}

	tok, secret, err := s.Auth.Create(req.Description, req.Grants, req.ExpiresAt)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("audit", "audit", true, "event", "token.create",
		"token_id", tok.ID, "description", tok.Description)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createTokenResponse{Token: tok, Secret: secret})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	tokens, err := s.Auth.List()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(tokens)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, id string) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if err := s.Auth.Revoke(id); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("audit", "audit", true, "event", "token.revoke", "token_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
