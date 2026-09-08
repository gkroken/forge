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
	"forge/internal/vuln"
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

// --- UI paths -------------------------------------------------------------
//
// The UI mutates repositories through its own handlers rather than the admin
// API, so the API gate alone would leave the enforcement trivially bypassable
// by clicking. These cover that.

// TestConfigOwned_UIDeleteRefused covers /ui/admin/repos/{name} DELETE.
func TestConfigOwned_UIDeleteRefused(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodDelete, "/ui/admin/repos/managed", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("UI delete of config-managed repo = %d, want 409", rec.Code)
	}
	if _, ok := srv.Repos.Get("managed"); !ok {
		t.Error("repo was deleted despite the refusal")
	}
}

// TestConfigOwned_UIDeleteAllowsUnmanaged — the UI stays usable for everything
// config does not own.
func TestConfigOwned_UIDeleteAllowsUnmanaged(t *testing.T) {
	srv, _ := ownedServer(t, false)
	if rec := do(t, srv, http.MethodDelete, "/ui/admin/repos/adhoc", ""); rec.Code == http.StatusConflict {
		t.Fatalf("UI delete of API-created repo must be allowed, got 409")
	}
}

// TestConfigOwned_SettingsFormIsReadOnly — the edit page must render disabled
// rather than accept input it will reject (grafana/grafana#37679).
func TestConfigOwned_SettingsFormIsReadOnly(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodGet, "/ui/admin/repos/managed/edit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("edit page = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Managed by config") {
		t.Error("edit page must explain that the repo is config-managed")
	}
	if !strings.Contains(body, "<fieldset disabled") {
		t.Error("form controls must be disabled at the door, not on submit")
	}

	// And the unmanaged one stays fully editable.
	rec = do(t, srv, http.MethodGet, "/ui/admin/repos/adhoc/edit", "")
	if strings.Contains(rec.Body.String(), "<fieldset disabled") {
		t.Error("API-created repo must render an editable form")
	}
}

// TestConfigOwned_ReposListShowsBadge — the list must distinguish the two.
func TestConfigOwned_ReposListShowsBadge(t *testing.T) {
	srv, _ := ownedServer(t, false)
	rec := do(t, srv, http.MethodGet, "/ui/admin/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin home = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pill-config") {
		t.Error("config-managed repo must carry a badge in the repositories list")
	}
}

// --- repo policy bindings over the admin API ------------------------------

// apiServer is a plain admin-API server with the policy managers wired, and no
// config-as-code mode.
func apiServer(t *testing.T) *Server {
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
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil)
	srv.Cleanup = cleanup.NewPolicyManager(m)
	srv.VulnPolicy = vuln.NewPolicyManager(m)
	return srv
}

// TestRepoUpdate_PreservesPolicyBindings is the regression: repoRequest had no
// cleanupPolicyName or securityPolicyName, and updateRepo rebuilds the
// repository from the request — so any unrelated PUT silently detached a repo's
// vulnerability gate and retention policy.
func TestRepoUpdate_PreservesPolicyBindings(t *testing.T) {
	srv := apiServer(t)
	do(t, srv, http.MethodPost, "/api/v1/cleanup-policies", `{"name":"age30","deleteOlderThanDays":30}`)
	do(t, srv, http.MethodPost, "/api/v1/security-policies", `{"name":"blockme","mode":"block","threshold":"high"}`)
	do(t, srv, http.MethodPost, "/api/v1/repos",
		`{"name":"t1","format":"npm","kind":"hosted","enabled":true,"cleanupPolicyName":"age30","securityPolicyName":"blockme"}`)

	got, ok := srv.Repos.Get("t1")
	if !ok || got.CleanupPolicyName != "age30" || got.SecurityPolicyName != "blockme" {
		t.Fatalf("create did not accept the bindings: %+v", got)
	}

	// An update that says nothing about policies must not clear them.
	do(t, srv, http.MethodPut, "/api/v1/repos/t1",
		`{"name":"t1","format":"npm","kind":"hosted","enabled":true,"anonymousRead":true}`)
	got, _ = srv.Repos.Get("t1")
	if got.CleanupPolicyName != "age30" {
		t.Errorf("cleanupPolicyName was cleared by an unrelated update: %q", got.CleanupPolicyName)
	}
	if got.SecurityPolicyName != "blockme" {
		t.Errorf("securityPolicyName was cleared by an unrelated update: %q", got.SecurityPolicyName)
	}
	if !got.AnonymousRead {
		t.Error("the update itself did not apply")
	}
}

// TestRepoUpdate_ExplicitUnbind — an empty string still detaches, so there is a
// way to remove a binding through the API.
func TestRepoUpdate_ExplicitUnbind(t *testing.T) {
	srv := apiServer(t)
	do(t, srv, http.MethodPost, "/api/v1/security-policies", `{"name":"blockme","mode":"block","threshold":"high"}`)
	do(t, srv, http.MethodPost, "/api/v1/repos",
		`{"name":"t1","format":"npm","kind":"hosted","enabled":true,"securityPolicyName":"blockme"}`)
	do(t, srv, http.MethodPut, "/api/v1/repos/t1",
		`{"name":"t1","format":"npm","kind":"hosted","enabled":true,"securityPolicyName":""}`)

	if got, _ := srv.Repos.Get("t1"); got.SecurityPolicyName != "" {
		t.Errorf("explicit unbind ignored: %q", got.SecurityPolicyName)
	}
}

// TestRepoUpdate_RejectsUnknownPolicy — config-as-code refuses a dangling
// policy reference; the admin API must not be the looser door.
func TestRepoUpdate_RejectsUnknownPolicy(t *testing.T) {
	srv := apiServer(t)
	do(t, srv, http.MethodPost, "/api/v1/repos", `{"name":"t1","format":"npm","kind":"hosted","enabled":true}`)
	for _, body := range []string{
		`{"name":"t1","format":"npm","kind":"hosted","cleanupPolicyName":"nope"}`,
		`{"name":"t1","format":"npm","kind":"hosted","securityPolicyName":"nope"}`,
	} {
		if rec := do(t, srv, http.MethodPut, "/api/v1/repos/t1", body); rec.Code != http.StatusBadRequest {
			t.Errorf("dangling policy reference = %d, want 400 (%s)", rec.Code, body)
		}
	}
}
