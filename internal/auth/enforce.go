package auth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"forge/internal/repo"
)

// UISessionCookie is the name of the HttpOnly session cookie set by the login page.
const UISessionCookie = "forge_token"

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
	decisionAllow     decision = iota
	decisionNeedAuth           // 401: no token or invalid/expired token
	decisionForbidden          // 403: valid token, insufficient role
)

// Middleware wraps next, enforcing the auth policy before each call.
// It must only be applied to /repository/ routes; probe and token routes
// are handled separately.
func (e *Enforcer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoName, sub := repoFromPath(r.URL.Path)
		switch e.decide(r, repoName, sub, actionFor(r.Method)) {
		case decisionNeedAuth:
			http.Error(w, "authentication required", http.StatusUnauthorized)
		case decisionForbidden:
			http.Error(w, "insufficient permissions", http.StatusForbidden)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// decide returns the policy decision for this request. sub is the
// repo-relative request path, consulted only by selector-scoped grants.
func (e *Enforcer) decide(r *http.Request, repoName, sub string, action Action) decision {
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

	if tok.Allows(repoName, sub, action) {
		return decisionAllow
	}
	return decisionForbidden
}

// MiddlewareOCI is like Middleware but extracts the repo name from a
// /v2/{repo}/... path instead of /repository/{repo}/...  It also sets the
// WWW-Authenticate header that OCI clients need for auth discovery.
func (e *Enforcer) MiddlewareOCI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoName, sub := ociRepoFromPath(r.URL.Path)
		switch e.decide(r, repoName, sub, actionFor(r.Method)) {
		case decisionNeedAuth:
			w.Header().Set("WWW-Authenticate", `Bearer realm="forge"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"errors": []map[string]any{{"code": "UNAUTHORIZED", "message": "authentication required"}},
			})
		case decisionForbidden:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{
				"errors": []map[string]any{{"code": "DENIED", "message": "insufficient permissions"}},
			})
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// RequireAdmin checks for a global-admin token on system-level API routes
// (tokens, users, webhooks, global policies, repo creation).
// Returns false and writes an HTTP error if the check fails.
func (e *Enforcer) RequireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if e.store == nil {
		return true // eval mode
	}
	tok := e.apiToken(w, r)
	if tok == nil {
		return false
	}
	if !tok.GlobalAdmin() {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	return true
}

// RequireRepoAdmin checks for a token carrying the admin action on the named
// repository (a repo-scoped admin grant, or global admin via "*"). Used by
// per-repository admin API routes: settings, cleanup, cache, scan, policy
// assignment. Returns false and writes an HTTP error if the check fails.
func (e *Enforcer) RequireRepoAdmin(w http.ResponseWriter, r *http.Request, repoName string) bool {
	if e.store == nil {
		return true // eval mode
	}
	tok := e.apiToken(w, r)
	if tok == nil {
		return false
	}
	if !tok.Allows(repoName, "", ActionAdmin) {
		http.Error(w, "admin role on repository "+repoName+" required", http.StatusForbidden)
		return false
	}
	return true
}

// RequireRepoRead gates a read of a repository's contents — the browse and
// component-listing endpoints, which are not artifact downloads but do disclose
// what a repository holds.
//
// It applies exactly the policy an artifact read gets, so anonymousRead=false
// means what it says. These endpoints previously had no check at all, on the
// reasoning that they were "not admin-only": an unauthenticated caller could
// list every package name and version in a private repository. Those names are
// the target list for a dependency-confusion attack, which is the thing forge's
// group shadowing exists to defeat.
//
// Both credential shapes are accepted: a Bearer token (API clients) and the UI
// session cookie (a signed-in browser).
func (e *Enforcer) RequireRepoRead(w http.ResponseWriter, r *http.Request, repoName string) bool {
	if e.store == nil {
		return true // eval mode: AllowAll
	}
	switch e.decideWithSession(r, repoName, ActionRead) {
	case decisionAllow:
		return true
	case decisionForbidden:
		http.Error(w, "insufficient permissions", http.StatusForbidden)
	default:
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	return false
}

// decideWithSession is decide() plus the UI session cookie, for endpoints the
// browser calls directly.
func (e *Enforcer) decideWithSession(r *http.Request, repoName string, action Action) decision {
	if bearerToken(r) == "" {
		if c, err := r.Cookie(UISessionCookie); err == nil && c.Value != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+c.Value)
		}
	}
	return e.decide(r, repoName, "", action)
}

// Caller returns the verified identity behind a request, from either credential
// shape: a Bearer token (API clients) or the UI session cookie (a signed-in
// browser). Returns nil when the request carries no valid credential.
//
// A session cookie holds a token, so both shapes resolve to the same type —
// which is what lets a signed-in user mint a token scoped by their own grants.
func (e *Enforcer) Caller(r *http.Request) *Token {
	if e.store == nil {
		return nil
	}
	secret := bearerToken(r)
	if secret == "" {
		if c, err := r.Cookie(UISessionCookie); err == nil {
			secret = c.Value
		}
	}
	if secret == "" {
		return nil
	}
	tok, err := e.store.Verify(secret)
	if err != nil {
		return nil
	}
	return tok
}

// apiToken resolves and verifies the request's token for API routes, writing
// a 401 and returning nil when absent or invalid.
func (e *Enforcer) apiToken(w http.ResponseWriter, r *http.Request) *Token {
	secret := bearerToken(r)
	if secret == "" {
		// The admin UI's own fetch/htmx calls hit these API routes with only
		// the HttpOnly forge_token session cookie (JS can't read it to set a
		// Bearer header). The cookie is SameSite=Strict, so honouring it here
		// is CSRF-safe. Unlike RequireAdminUI this still returns 401 rather
		// than redirecting, which is what XHR/fetch callers expect.
		if c, err := r.Cookie(UISessionCookie); err == nil {
			secret = c.Value
		}
	}
	if secret == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return nil
	}
	tok, err := e.store.Verify(secret)
	if err != nil || tok == nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return nil
	}
	return tok
}

// RequireAdminUI is like RequireAdmin but intended for browser UI handlers.
// It accepts the token from an Authorization header OR from the forge_token
// session cookie set by the login page.  On failure it redirects to the login
// page (303) rather than returning 401, which is appropriate for browser flows.
// Returns true if the caller may proceed.
func (e *Enforcer) RequireAdminUI(w http.ResponseWriter, r *http.Request) bool {
	if e.store == nil {
		return true // eval mode: AllowAll
	}
	secret := bearerToken(r)
	if secret == "" {
		if c, err := r.Cookie(UISessionCookie); err == nil {
			secret = c.Value
		}
	}
	next := url.QueryEscape(r.URL.RequestURI())
	if secret == "" {
		http.Redirect(w, r, "/ui/login?next="+next, http.StatusSeeOther)
		return false
	}
	tok, err := e.store.Verify(secret)
	if err != nil || tok == nil {
		http.Redirect(w, r, "/ui/login?error=invalid&next="+next, http.StatusSeeOther)
		return false
	}
	if !tok.GlobalAdmin() {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	return true
}

// --- helpers -----------------------------------------------------------------

func repoFromPath(path string) (name, sub string) {
	// /repository/{name}/{sub...}
	rest := strings.TrimPrefix(path, "/repository/")
	name, sub, _ = strings.Cut(rest, "/")
	return name, sub
}

func ociRepoFromPath(path string) (name, sub string) {
	// /v2/{name}/{sub...}
	rest := strings.TrimPrefix(path, "/v2/")
	name, sub, _ = strings.Cut(rest, "/")
	return name, sub
}

func actionFor(method string) Action {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ActionRead
	case http.MethodDelete:
		return ActionDelete
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
