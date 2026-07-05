package server

import (
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/ldap"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/oidc"
	"forge/internal/repo"
	"forge/internal/vuln"
)

// TestServerWiringSetters exercises the optional-subsystem builder setters so
// their field assignments are covered and confirmed to return the receiver.
func TestServerWiringSetters(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil)

	mapper := auth.NewGroupRoleMapper(nil)

	if srv.WithOIDC(&oidc.Provider{}, mapper) != srv {
		t.Error("WithOIDC should return the receiver")
	}
	if srv.OIDC == nil || srv.GroupMapper == nil {
		t.Error("WithOIDC did not wire OIDC/GroupMapper")
	}

	if srv.WithLDAP(&ldap.Client{}, mapper) != srv || srv.LDAP == nil {
		t.Error("WithLDAP did not wire the LDAP client")
	}

	vs := vuln.NewStore(m)
	if srv.WithVuln(vs, nil) != srv || srv.Vuln == nil {
		t.Error("WithVuln did not wire the vuln store")
	}

	sched := cleanup.NewScheduler(srv.Repos, srv.Cleanup, nil, nil)
	if srv.WithScheduler(sched) != srv || srv.Scheduler == nil {
		t.Error("WithScheduler did not wire the scheduler")
	}
}

// TestGatherCounters covers the Prometheus counter-gathering dashboard helpers
// against a registry with known counter values.
func TestGatherCounters(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	promReg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(promReg)
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil).WithMetrics(metrics, promReg)

	metrics.Downloads.WithLabelValues("npm-hosted").Add(3)
	metrics.Downloads.WithLabelValues("maven-hosted").Add(2)
	metrics.HTTPRequests.WithLabelValues("GET", "/x", "200").Add(5)

	if got := srv.gatherCounterTotal("forge_artifact_downloads_total"); got != 5 {
		t.Errorf("gatherCounterTotal(downloads) = %d, want 5", got)
	}
	// Only repos whose name starts with "n".
	if got := srv.gatherCounterByLabelPrefix("forge_artifact_downloads_total", "repo", "n"); got != 3 {
		t.Errorf("gatherCounterByLabelPrefix(repo~n) = %d, want 3", got)
	}
	// Unknown metric → 0.
	if got := srv.gatherCounterTotal("nonexistent_metric"); got != 0 {
		t.Errorf("gatherCounterTotal(unknown) = %d, want 0", got)
	}
}
