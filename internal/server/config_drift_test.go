package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/config"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/repo"
	"github.com/prometheus/client_golang/prometheus"
)

// driftServer wires a server whose planner reads a real config file on disk, so
// the tests exercise the same reload-on-every-call path production uses.
func driftServer(t *testing.T, cfgYAML string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := repo.NewManager()
	appliers := config.Appliers{Repos: mgr, Cleanup: cleanup.NewPolicyManager(m), Meta: m}

	path := filepath.Join(dir, "forge.config.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Boot-time apply, exactly as main does.
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply(f, appliers); err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	srv := New(mgr, format.NewRegistry(), b, m, nil).
		WithConfigMode(path, false).
		WithMetrics(obs.NewMetrics(reg), reg).
		WithConfigDrift(func() (config.Result, error) {
			f, err := config.Load(path)
			if err != nil {
				return config.Result{}, err
			}
			return config.Plan(f, appliers)
		})
	return srv, path
}

const baseCfg = `
repositories:
  - name: npm-managed
    format: npm
    kind: hosted
    enabled: true
`

func getDrift(t *testing.T, srv *Server) driftResponse {
	t.Helper()
	rec := do(t, srv, http.MethodGet, "/api/v1/config/drift", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("drift = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var d driftResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// TestDrift_CleanAfterApply — right after a boot apply nothing should differ.
func TestDrift_CleanAfterApply(t *testing.T) {
	srv, _ := driftServer(t, baseCfg)
	d := getDrift(t, srv)
	if d.Drift || d.Objects != 0 {
		t.Errorf("expected no drift after apply, got %+v", d)
	}
	if d.Kinds["repositories"].Noop != 1 {
		t.Errorf("expected the managed repo to be a noop, got %+v", d.Kinds["repositories"])
	}
}

// TestDrift_DetectsEditedFile is the point of the endpoint: a commit that has
// not been applied yet must show up as pending work.
func TestDrift_DetectsEditedFile(t *testing.T) {
	srv, path := driftServer(t, baseCfg)
	edited := baseCfg + `  - name: npm-new
    format: npm
    kind: hosted
    enabled: true
`
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	d := getDrift(t, srv)
	if !d.Drift {
		t.Fatalf("expected drift after editing the file, got %+v", d)
	}
	if d.Kinds["repositories"].Created != 1 {
		t.Errorf("expected 1 pending create, got %+v", d.Kinds["repositories"])
	}
}

// TestDrift_DetectsLiveChange — the other direction: state edited out from
// under the file (only reachable via -allow-config-override) reads as a
// pending update.
func TestDrift_DetectsLiveChange(t *testing.T) {
	srv, _ := driftServer(t, baseCfg)
	rp, _ := srv.Repos.Get("npm-managed")
	rp.AnonymousRead = !rp.AnonymousRead
	if err := srv.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}
	d := getDrift(t, srv)
	if !d.Drift || d.Kinds["repositories"].Updated != 1 {
		t.Errorf("expected 1 pending update, got %+v", d)
	}
}

// TestDrift_PublishesGauge — Prometheus must see it without an API call.
func TestDrift_PublishesGauge(t *testing.T) {
	srv, path := driftServer(t, baseCfg)
	if err := os.WriteFile(path, []byte(baseCfg+`  - name: npm-new
    format: npm
    kind: hosted
    enabled: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.computeDrift(); err != nil {
		t.Fatal(err)
	}
	// Assert through the real scrape endpoint rather than reading the gauge
	// object: this covers registration and exposition too.
	if !strings.Contains(scrape(t, srv), driftCreateLine+" 1") {
		t.Errorf("gauge not exposed; scrape was:\n%s", driftLines(t, srv))
	}
}

// TestDrift_404WithoutConfigMode — no file, nothing to diff.
func TestDrift_404WithoutConfigMode(t *testing.T) {
	srv, _ := driftServer(t, baseCfg)
	srv.configPlan = nil
	if rec := do(t, srv, http.MethodGet, "/api/v1/config/drift", ""); rec.Code != http.StatusNotFound {
		t.Errorf("drift without config mode = %d, want 404", rec.Code)
	}
}

// TestDrift_ReportsBrokenFile — a file that stopped parsing is itself drift
// worth reporting, not a silent "all clear".
func TestDrift_ReportsBrokenFile(t *testing.T) {
	srv, path := driftServer(t, baseCfg)
	if err := os.WriteFile(path, []byte("repositories:\n  - name: x\n   bad: indent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, srv, http.MethodGet, "/api/v1/config/drift", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("broken config = %d, want 500", rec.Code)
	}
}

// TestDrift_WatcherRefreshesGauge — the background loop must publish without
// anyone calling the endpoint.
func TestDrift_WatcherRefreshesGauge(t *testing.T) {
	srv, path := driftServer(t, baseCfg)
	if err := os.WriteFile(path, []byte(baseCfg+`  - name: npm-new
    format: npm
    kind: hosted
    enabled: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv.StartDriftWatcher(10*time.Millisecond, done)
	defer close(done)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(scrape(t, srv), driftCreateLine+" 1") {
			return // published
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("watcher did not publish the drift gauge within 2s")
}

const driftCreateLine = `forge_config_drift_objects{kind="repositories",op="create"}`

// scrape returns the Prometheus exposition text from the server's /metrics.
func scrape(t *testing.T, srv *Server) string {
	t.Helper()
	return do(t, srv, http.MethodGet, "/metrics", "").Body.String()
}

// driftLines filters a scrape down to the drift gauge, for readable failures.
func driftLines(t *testing.T, srv *Server) string {
	t.Helper()
	var keep []string
	for _, l := range strings.Split(scrape(t, srv), "\n") {
		if strings.HasPrefix(l, "forge_config_drift_objects") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}
