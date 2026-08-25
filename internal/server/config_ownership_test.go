package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/config"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/repo"
)

// ownedServer builds a server whose "managed" repo is config-owned and whose
// "adhoc" repo is not, by running a real config.Apply to populate the managed
// set — the same path production takes.
func ownedServer(t *testing.T, override bool) (*Server, *obs.AuditLog) {
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

	managedRepo := repo.Repository{Name: "managed", Format: "npm", Kind: repo.Hosted, Enabled: true}
	f := config.File{
		Repositories:    []repo.Repository{managedRepo},
		CleanupPolicies: []cleanup.NamedPolicy{{Name: "cp-managed"}},
	}
	appliers := config.Appliers{
		Repos:   mgr,
		Cleanup: cleanup.NewPolicyManager(m),
		Meta:    m,
	}
	if _, err := config.Apply(f, appliers); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	// An object created the ordinary way — must stay editable.
	if err := mgr.Add(repo.Repository{Name: "adhoc", Format: "npm", Kind: repo.Hosted, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	sink := obs.NewAuditLog(32)
	srv := New(mgr, format.NewRegistry(), b, m, nil).
		WithConfigMode("/etc/forge/forge.config.yaml", override).
		WithAuditLog(sink)
	srv.Cleanup = cleanup.NewPolicyManager(m)
	return srv, sink
}

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// TestConfigOwned_RefusesRepoWrite is the core C3 guarantee.
func TestConfigOwned_RefusesRepoWrite(t *testing.T) {
	srv, _ := ownedServer(t, false)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/repos/managed", `{"name":"managed","format":"npm","kind":"hosted"}`},
		{http.MethodDelete, "/api/v1/repos/managed", ""},
	} {
		rec := do(t, srv, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s %s = %d, want 409", tc.method, tc.path, rec.Code)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 409 body: %v", err)
		}
		if body["source"] != "/etc/forge/forge.config.yaml" {
			t.Errorf("409 must name the config source, got %q", body["source"])
		}
		if !strings.Contains(body["remedy"], "-allow-config-override") {
			t.Errorf("409 must state the remedy, got %q", body["remedy"])
		}
	}
}

// TestConfigOwned_AllowsAPICreatedObject — ownership is per object, not a mode.
func TestConfigOwned_AllowsAPICreatedObject(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodDelete, "/api/v1/repos/adhoc", "")
	if rec.Code == http.StatusConflict {
		t.Fatalf("API-created repo must stay editable, got 409: %s", rec.Body.String())
	}
}

// TestConfigOwned_ReadsUnaffected — the gate is write-only.
func TestConfigOwned_ReadsUnaffected(t *testing.T) {
	srv, _ := ownedServer(t, false)
	if rec := do(t, srv, http.MethodGet, "/api/v1/repos/managed", ""); rec.Code != http.StatusOK {
		t.Errorf("GET = %d, want 200", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/v1/repos", ""); rec.Code != http.StatusOK {
		t.Errorf("list = %d, want 200", rec.Code)
	}
}

// TestConfigOwned_ManagedByInResponses — the UI needs to know which is which.
func TestConfigOwned_ManagedByInResponses(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodGet, "/api/v1/repos", "")
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	got := map[string]any{}
	for _, r := range list {
		got[r["name"].(string)] = r["managedBy"]
	}
	if got["managed"] != "config" {
		t.Errorf("managed repo managedBy = %v, want config", got["managed"])
	}
	if got["adhoc"] != "api" {
		t.Errorf("adhoc repo managedBy = %v, want api", got["adhoc"])
	}
}

// TestConfigOwned_RefusesCleanupPolicyWrite covers a second object kind, and
// the upsert-by-POST path where the name comes from the body.
func TestConfigOwned_RefusesCleanupPolicyWrite(t *testing.T) {
	srv, _ := ownedServer(t, false)
	if rec := do(t, srv, http.MethodPost, "/api/v1/cleanup-policies", `{"name":"cp-managed"}`); rec.Code != http.StatusConflict {
		t.Errorf("POST upsert of managed policy = %d, want 409", rec.Code)
	}
	if rec := do(t, srv, http.MethodDelete, "/api/v1/cleanup-policies/cp-managed", ""); rec.Code != http.StatusConflict {
		t.Errorf("DELETE managed policy = %d, want 409", rec.Code)
	}
	if rec := do(t, srv, http.MethodPost, "/api/v1/cleanup-policies", `{"name":"cp-adhoc"}`); rec.Code == http.StatusConflict {
		t.Errorf("unmanaged policy must be writable, got 409")
	}
}

// TestConfigOwned_RefusalIsAudited — a blocked write must leave a trace.
func TestConfigOwned_RefusalIsAudited(t *testing.T) {
	srv, sink := ownedServer(t, false)
	do(t, srv, http.MethodDelete, "/api/v1/repos/managed", "")
	var found bool
	for _, e := range sink.Recent(32) {
		if e.Status == http.StatusConflict && strings.Contains(e.Detail, "config-managed") {
			found = true
		}
	}
	if !found {
		t.Errorf("refusal not audited: %+v", sink.Recent(32))
	}
}

// TestConfigOverride_PermitsAndAudits — break-glass lets the write through but
// records it, so an incident edit is visible afterwards.
func TestConfigOverride_PermitsAndAudits(t *testing.T) {
	srv, sink := ownedServer(t, true)
	rec := do(t, srv, http.MethodDelete, "/api/v1/repos/managed", "")
	if rec.Code == http.StatusConflict {
		t.Fatalf("override must permit the write, got 409")
	}
	var found bool
	for _, e := range sink.Recent(32) {
		if strings.Contains(e.Detail, "config override") {
			found = true
		}
	}
	if !found {
		t.Errorf("override write not audited: %+v", sink.Recent(32))
	}
}

// TestConfigOwned_InactiveWithoutConfigMode — forge started without -config
// must behave exactly as before.
func TestConfigOwned_InactiveWithoutConfigMode(t *testing.T) {
	srv, _ := ownedServer(t, false)
	srv.configSource = "" // simulate a boot with no -config
	if rec := do(t, srv, http.MethodDelete, "/api/v1/repos/managed", ""); rec.Code == http.StatusConflict {
		t.Errorf("no -config mode must not gate anything, got 409")
	}
}

// TestConfigOwned_ContentRoutesNotGated is the regression guard for the mistake
// a blanket route-level middleware would have made: config owns a repository's
// definition, not its artifacts, so cleanup/promote/component/trash must stay
// open on a config-managed repo.
func TestConfigOwned_ContentRoutesNotGated(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodPost, "/api/v1/repos/managed/cleanup", `{"dryRun":true}`)
	if rec.Code == http.StatusConflict {
		t.Errorf("content route on a config-managed repo must not 409: %s", rec.Body.String())
	}
}
