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
//	POST   /api/v1/tokens        create (bootstrap, admin, or self-service)
//	GET    /api/v1/tokens        list   (admin sees all; a user sees their own)
//	DELETE /api/v1/tokens/{id}   revoke (admin any; a user their own)
//
// Self-service exists because the alternative is worse. When only admins could
// mint tokens, every developer wanting to run `npm install` against a private
// registry had to ask one — so people shared tokens, which is precisely what
// per-user credentials are for.
//
// The rule that keeps it safe: a caller may only mint a token whose grants
// their own credential already carries, so a token can never widen anyone's
// authority. A write user cannot mint an admin token, and a token scoped to one
// repository cannot mint one covering another.
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
	Description string       `json:"description"`
	Grants      []auth.Grant `json:"grants"`
	ExpiresAt   *time.Time   `json:"expires_at,omitempty"`
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
	// Bootstrap: the very first token may be created without authentication.
	var caller *auth.Token
	if n > 0 {
		caller = s.Enforcer.Caller(r)
		if caller == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}

	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Description == "" {
		req.Description = "unnamed token"
	}
	if err := auth.ValidateGrants(req.Grants); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A non-admin may mint only within their own authority, and only if their
	// credential is tied to a person — otherwise tokens would beget ownerless
	// tokens nobody can find or revoke.
	owner := ""
	if caller != nil && !caller.GlobalAdmin() {
		if caller.Owner == "" {
			jsonError(w, "this credential cannot create tokens; sign in, or ask an admin",
				http.StatusForbidden)
			return
		}
		if bad := exceedsCaller(caller, req.Grants); bad != "" {
			jsonError(w, "a token cannot grant more than you have: "+bad, http.StatusForbidden)
			return
		}
		owner = caller.Owner
	}

	tok, secret, err := s.Auth.Create(req.Description, req.Grants, req.ExpiresAt, owner)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("audit", "audit", true, "event", "token.create",
		"token_id", tok.ID, "description", tok.Description, "owner", tok.Owner)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createTokenResponse{Token: tok, Secret: secret}) // #nosec G117 -- intentional: one-time secret returned only at token creation
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	caller := s.Enforcer.Caller(r)
	if caller == nil {
		jsonError(w, "authentication required", http.StatusUnauthorized)
		return
	}
	tokens, err := s.Auth.List()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// An admin sees every token, which is what makes self-service auditable.
	// Anyone else sees only their own.
	if !caller.GlobalAdmin() {
		mine := make([]auth.Token, 0, len(tokens))
		for _, t := range tokens {
			if t.Owner != "" && t.Owner == caller.Owner {
				mine = append(mine, t)
			}
		}
		tokens = mine
	}
	json.NewEncoder(w).Encode(tokens)
}

// exceedsCaller reports the first requested grant the caller cannot already
// exercise, or "" when every one is within their authority.
//
// The check is deliberately strict about selectors: a caller whose own grant is
// path-scoped fails Allows with an empty path, so they cannot delegate at all
// rather than delegating something subtly wider than they hold.
func exceedsCaller(caller *auth.Token, grants []auth.Grant) string {
	for _, g := range grants {
		for _, a := range g.Actions {
			if !caller.Allows(g.Repo, "", a) {
				return string(a) + " on " + g.Repo
			}
		}
	}
	return ""
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, id string) {
	caller := s.Enforcer.Caller(r)
	if caller == nil {
		jsonError(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !caller.GlobalAdmin() {
		// Revoking someone else's token would be a denial of service, so a
		// non-admin may only revoke tokens they own. A token that does not
		// exist is reported the same way, so this cannot be used to enumerate.
		tokens, err := s.Auth.List()
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		owned := false
		for _, t := range tokens {
			if t.ID == id && t.Owner != "" && t.Owner == caller.Owner {
				owned = true
				break
			}
		}
		if !owned {
			jsonError(w, "token not found", http.StatusNotFound)
			return
		}
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
