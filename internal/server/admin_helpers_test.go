package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"forge/internal/trivy"
	"forge/internal/vuln"
)

func TestParseAuditCursor(t *testing.T) {
	// Valid cursor.
	ts := time.Now().UTC().Truncate(time.Second)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/audit?before_id=42&before_ts="+url.QueryEscape(ts.Format(time.RFC3339Nano)), nil)
	c := parseAuditCursor(r)
	if c.ID != 42 || !c.Timestamp.Equal(ts) {
		t.Errorf("cursor = %+v, want id=42 ts=%v", c, ts)
	}

	// Malformed values → zero cursor.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/audit?before_id=abc&before_ts=notatime", nil)
	if c := parseAuditCursor(r); !c.IsZero() {
		t.Errorf("malformed cursor should be zero, got %+v", c)
	}
}

func TestSecurityPolicyNames(t *testing.T) {
	srv := newRichUIServer(t) // has a VulnPolicy manager wired
	// Nil manager → nil names.
	bare := newAdminServer(t)
	if names := bare.securityPolicyNames(); names != nil {
		t.Errorf("nil VulnPolicy should give nil names, got %v", names)
	}
	// Two named policies, returned sorted.
	srv.VulnPolicy.Put(vuln.NamedPolicy{Name: "strict"}) //nolint:errcheck
	srv.VulnPolicy.Put(vuln.NamedPolicy{Name: "audit"})  //nolint:errcheck
	names := srv.securityPolicyNames()
	if len(names) != 2 || names[0] != "audit" || names[1] != "strict" {
		t.Errorf("names = %v, want [audit strict]", names)
	}
}

func TestWithTrivy(t *testing.T) {
	srv := newAdminServer(t)
	sc := trivy.New("trivy", "localhost:8080", "")
	if srv.WithTrivy(sc) != srv || srv.Trivy == nil {
		t.Error("WithTrivy did not wire the scanner")
	}
}

// TestRunPolicy drives POST /api/v1/cleanup-policies/{name}/run in dry-run mode.
func TestRunPolicy_DryRun(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()

	// Create a policy and assign it to npm-hosted.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/cleanup-policies", map[string]any{
		"name": "retain", "keepVersions": 2,
	}))
	if rw.Code != http.StatusCreated && rw.Code != http.StatusOK {
		t.Fatalf("create policy: status %d (%s)", rw.Code, rw.Body.String())
	}
	rp, _ := srv.Repos.Get("npm-hosted")
	rp.CleanupPolicyName = "retain"
	srv.Repos.Update(rp) //nolint:errcheck

	// Dry-run the policy.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/cleanup-policies/retain/run?dry=true", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("run policy dry: status %d (%s)", rw.Code, rw.Body.String())
	}

	// GET (wrong method) on the run endpoint → 405.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/cleanup-policies/retain/run", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET run: status %d, want 405", rw.Code)
	}
}
