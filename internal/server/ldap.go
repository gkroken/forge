package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"forge/internal/auth"
	forgeldap "forge/internal/ldap"
)

// ldapAuthenticator is the interface the LDAP login path depends on.
// *forgeldap.Client satisfies it; tests use a fake implementation.
type ldapAuthenticator interface {
	Authenticate(ctx context.Context, username, password string) (forgeldap.UserInfo, error)
	DefaultGrants() []auth.Grant
	TokenTTL() time.Duration
	// Read-only config accessors for the admin Access page. The bind password
	// is deliberately never exposed.
	URLs() []string
	BindDN() string
	UserBaseDN() string
	UserFilter() string
	GroupMode() string
}

// tryLDAPLogin attempts a search-then-bind against the configured directory.
// On success it mints a forge session (via establishSSOSession, source="ldap")
// and returns handled=true. A directory/credential failure returns false so the
// caller can fall through to a generic "invalid credentials" response — the
// specific reason is logged, never shown, to avoid a username-enumeration oracle.
func (s *Server) tryLDAPLogin(w http.ResponseWriter, r *http.Request, username, password, next string) (handled bool) {
	if s.LDAP == nil || s.Auth == nil {
		return false
	}
	info, err := s.LDAP.Authenticate(r.Context(), username, password)
	if err != nil {
		slog.Info("ldap: login failed", "user", username, "err", err)
		return false
	}
	// Use the login name (not the DN) as the session subject so the Users tab and
	// audit log read "alice", not "uid=alice,ou=people,dc=…".
	if err := s.establishSSOSession(w, r, "ldap", info.Username, info.Email, info.Groups,
		s.ldapMapper, s.LDAP.DefaultGrants(), s.LDAP.TokenTTL(), next); err != nil {
		slog.Error("ldap: establish session failed", "err", err)
		return false
	}
	return true
}
