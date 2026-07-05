package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/vuln"
)

// newRichUIServer builds an eval-mode server with the optional subsystems the
// admin UI pages read from (users, roles, metrics, global stats, audit log,
// cleanup + security policy managers) so each page renders its real content
// rather than short-circuiting on a nil dependency.
func newRichUIServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(npm.New())
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, AnonymousRead: true})                              //nolint:errcheck
	mgr.Add(repo.Repository{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, Upstream: "https://registry.npmjs.org"})            //nolint:errcheck

	promReg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(promReg)
	al := obs.NewAuditLog(50)
	al.Append(obs.AuditEntry{Timestamp: time.Now(), Actor: "alice", Method: "PUT", Path: "/repository/npm-hosted/pkg", Status: 201})
	al.Append(obs.AuditEntry{Timestamp: time.Now(), Actor: "anonymous", Method: "GET", Path: "/repository/npm-hosted/pkg", Status: 403})

	srv := New(mgr, reg, b, m, nil).
		WithUsers(auth.NewUserStore(m)).
		WithRoles(auth.NewRoleStore(m)).
		WithMetrics(metrics, promReg).
		WithGlobalStats(obs.NewGlobalStats()).
		WithAuditLog(al)
	srv.WithCleanup(cleanup.NewPolicyManager(m))
	srv.WithVulnPolicy(vuln.NewPolicyManager(m))
	return srv
}

// TestUIAdminPages_Render walks every admin UI page and asserts it renders HTML
// with a 200. This exercises the page builders (users/roles tab data, dashboard
// request/metrics bars, cleanup + security panels, audit rows) end to end.
func TestUIAdminPages_Render(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()

	pages := []string{
		"/ui/dashboard",
		"/ui/admin",
		"/ui/admin/access",
		"/ui/admin/cleanup-policies",
		"/ui/admin/cleanup-policies/new",
		"/ui/admin/webhooks",
		"/ui/admin/observability",
		"/ui/admin/audit",
		"/ui/admin/security",
		"/ui/admin/security-policies",
		"/ui/admin/integrity",
		"/ui/admin/migration",
	}
	for _, p := range pages {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, p, nil))
		if rw.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200 (%.200s)", p, rw.Code, rw.Body.String())
			continue
		}
		if ct := rw.Header().Get("Content-Type"); ct != "" && !contains(ct, "html") {
			t.Errorf("GET %s: Content-Type %q, want html", p, ct)
		}
		if rw.Body.Len() == 0 {
			t.Errorf("GET %s: empty body", p)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
