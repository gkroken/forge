package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"forge/internal/auth"
	forgeldap "forge/internal/ldap"
)

// fakeLDAP is a scripted ldapAuthenticator for handler tests.
type fakeLDAP struct {
	info          forgeldap.UserInfo
	err           error
	defaultGrants []auth.Grant
	ttl           time.Duration
}

func (f *fakeLDAP) Authenticate(_ context.Context, _, _ string) (forgeldap.UserInfo, error) {
	if f.err != nil {
		return forgeldap.UserInfo{}, f.err
	}
	return f.info, nil
}
func (f *fakeLDAP) DefaultGrants() []auth.Grant {
	if f.defaultGrants == nil {
		return []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
	}
	return f.defaultGrants
}
func (f *fakeLDAP) TokenTTL() time.Duration {
	if f.ttl == 0 {
		return time.Hour
	}
	return f.ttl
}
func (f *fakeLDAP) URLs() []string     { return []string{"ldap://dir.example.com:389"} }
func (f *fakeLDAP) BindDN() string     { return "cn=svc,dc=example,dc=com" }
func (f *fakeLDAP) UserBaseDN() string { return "ou=people,dc=example,dc=com" }
func (f *fakeLDAP) UserFilter() string { return "(uid=%s)" }
func (f *fakeLDAP) GroupMode() string  { return "memberof" }

func newLDAPServer(t *testing.T, fake *fakeLDAP) (*Server, auth.Store) {
	t.Helper()
	srv, authStore := newAuthServer(t)
	srv.LDAP = fake
	return srv, authStore
}

func loginPost(username, password string) *http.Request {
	form := url.Values{"username": {username}, "password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestLDAPLogin_Success_MintsSessionWithGroupRole(t *testing.T) {
	fake := &fakeLDAP{info: forgeldap.UserInfo{
		Username: "alice", Email: "alice@example.com", Groups: []string{"forge-admins"},
	}}
	srv, authStore := newLDAPServer(t, fake)
	srv.ldapMapper = auth.NewGroupRoleMapper([]auth.GroupRule{{Group: "forge-admins", Role: auth.RoleAdmin}})

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, loginPost("alice", "pw"))

	if rw.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rw.Code)
	}
	tok, _ := authStore.Verify(sessionCookieValue(rw))
	if tok == nil {
		t.Fatal("no session token minted")
	}
	if len(tok.Grants) != 1 || tok.Grants[0].Repo != "*" || tok.Grants[0].Role != auth.RoleAdmin {
		t.Fatalf("expected admin grant on *, got %+v", tok.Grants)
	}
	if tok.Description != "ldap:alice@example.com" {
		t.Errorf("description: got %q, want ldap:alice@example.com", tok.Description)
	}
}

func TestLDAPLogin_NoGroupMatch_UsesFallback(t *testing.T) {
	fake := &fakeLDAP{
		info:          forgeldap.UserInfo{Username: "carol", Groups: []string{"nobody"}},
		defaultGrants: []auth.Grant{{Repo: "*", Role: auth.RoleRead}},
	}
	srv, authStore := newLDAPServer(t, fake)
	srv.ldapMapper = auth.NewGroupRoleMapper([]auth.GroupRule{{Group: "forge-admins", Role: auth.RoleAdmin}})

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, loginPost("carol", "pw"))

	tok, _ := authStore.Verify(sessionCookieValue(rw))
	if tok == nil || len(tok.Grants) != 1 || tok.Grants[0].Role != auth.RoleRead {
		t.Fatalf("expected fallback read grant, got %+v", tok)
	}
}

func TestLDAPLogin_BindFailure_ShowsGenericError(t *testing.T) {
	fake := &fakeLDAP{err: errors.New("invalid credentials")}
	srv, _ := newLDAPServer(t, fake)

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, loginPost("alice", "wrong"))

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rw.Code)
	}
	if sessionCookieValue(rw) != "" {
		t.Fatal("no session cookie should be set on failed LDAP login")
	}
	if !strings.Contains(rw.Body.String(), "Invalid username or password") {
		t.Error("expected generic invalid-credentials message")
	}
}

// Local users are tried before LDAP; a valid local user must not touch the
// directory (and gets its local role).
func TestLDAPLogin_LocalUserTakesPrecedence(t *testing.T) {
	fake := &fakeLDAP{err: errors.New("should not be called")}
	srv, authStore := newLDAPServer(t, fake)
	srv.Users = auth.NewUserStore(srv.Meta)
	if _, err := srv.Users.Create("localadmin", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, loginPost("localadmin", "pw123456"))

	tok, _ := authStore.Verify(sessionCookieValue(rw))
	if tok == nil {
		t.Fatal("local login should have succeeded without hitting LDAP")
	}
	if !strings.HasPrefix(tok.Description, "session:") {
		t.Errorf("expected local session token, got %q", tok.Description)
	}
}
