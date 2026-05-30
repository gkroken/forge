package auth

import (
	"net/http"
	"strings"

	"forge/internal/repo"
)

// Enforcer applies the per-repo auth policy on every /repository/ request.
// When Store is nil, it uses AllowAll — every request is permitted, but a
// policy decision is still made on every route.
type Enforcer struct {
	store Store
	repos *repo.Manager
}

// NewEnforcer returns an Enforcer. If store is nil, AllowAll is used.
func NewEnforcer(store Store, repos *repo.Manager) *Enforcer {
	return &Enforcer{store: store, repos: repos}
}

type decision int

const (
	decisionAllow decision = iota
	decisionNeedAuth   // 401: no token or invalid/expired token
	decisionForbidden  // 403: valid token, insufficient role
)

// Middleware wraps next, enforcing the auth policy before each call.
// It must only be applied to /repository/ routes; probe and token routes
// are handled separately.
func (e *Enforcer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch e.decide(r, repoFromPath(r.URL.Path), actionFor(r.Method)) {
		case decisionNeedAuth:
			http.Error(w, "authentication required", http.StatusUnauthorized)
		case decisionForbidden:
			http.Error(w, "insufficient permissions", http.StatusForbidden)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// decide returns the policy decision for this request.
func (e *Enforcer) decide(r *http.Request, repoName string, action Action) decision {
	if e.store == nil {
		return decisionAllow // eval mode: AllowAll
	}

	rp, ok := e.repos.Get(repoName)
	if !ok {
		return decisionAllow // unknown repo → let handler return 404
	}

	secret := bearerToken(r)
	if secret == "" {
		if rp.AnonymousRead && action == ActionRead {
			return decisionAllow
		}
		return decisionNeedAuth
	}

	tok, err := e.store.Verify(secret)
	if err != nil || tok == nil {
		return decisionNeedAuth // invalid or expired token → re-authenticate
	}

	role := tok.RoleFor(repoName)
	switch action {
	case ActionRead:
		if role >= RoleRead {
			return decisionAllow
		}
	case ActionWrite:
		if role >= RoleWrite {
			return decisionAllow
		}
	}
	return decisionForbidden
}

// RequireAdmin checks for an admin token on token-management routes.
// Returns false and writes an HTTP error if the check fails.
func (e *Enforcer) RequireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if e.store == nil {
		return true // eval mode
	}
	secret := bearerToken(r)
	if secret == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	tok, err := e.store.Verify(secret)
	if err != nil || tok == nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return false
	}
	if tok.RoleFor("*") < RoleAdmin {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	return true
}

// --- helpers -----------------------------------------------------------------

func repoFromPath(path string) string {
	// /repository/{name}/...
	rest := strings.TrimPrefix(path, "/repository/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}

func actionFor(method string) Action {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ActionRead
	default:
		return ActionWrite
	}
}

func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(v, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	// npm sends _authToken as Basic auth with empty user
	if user, pass, ok := r.BasicAuth(); ok && user == "" {
		return pass
	}
	return ""
}
