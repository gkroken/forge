package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/vuln"
)

// TestSecurityPolicies_CRUD drives the named security-policy CRUD and the global
// default endpoint, covering handleSecurityPoliciesList, handleSecurityPolicyByName,
// handleSecurityDefault and stampSuppressions.
func TestSecurityPolicies_CRUD(t *testing.T) {
	srv := newRichUIServer(t) // VulnPolicy wired
	h := srv.Routes()

	// Create a named policy carrying a suppression (exercises stampSuppressions).
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/security-policies", map[string]any{
		"name":         "block-high",
		"mode":         "block",
		"threshold":    "high",
		"suppressions": []map[string]any{{"id": "CVE-2021-1", "reason": "accepted risk"}},
	}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create policy: status %d (%s)", rw.Code, rw.Body.String())
	}
	var created vuln.NamedPolicy
	json.NewDecoder(rw.Body).Decode(&created)
	if len(created.Suppressions) != 1 || created.Suppressions[0].At.IsZero() || created.Suppressions[0].By == "" {
		t.Errorf("suppression not stamped with who/when: %+v", created.Suppressions)
	}

	// List → contains it.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/security-policies", nil))
	var list []vuln.NamedPolicy
	json.NewDecoder(rw.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "block-high" {
		t.Fatalf("list = %+v, want [block-high]", list)
	}

	// GET by name.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/security-policies/block-high", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("get by name: status %d", rw.Code)
	}

	// GET missing → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/security-policies/ghost", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("get missing: status %d, want 404", rw.Code)
	}

	// PUT update the policy (URL name wins).
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/security-policies/block-high", map[string]any{
		"mode": "warn", "threshold": "critical",
	}))
	if rw.Code != http.StatusOK {
		t.Fatalf("update policy: status %d (%s)", rw.Code, rw.Body.String())
	}
	if p, _, _ := srv.VulnPolicy.Get("block-high"); p.Mode != vuln.ModeWarn {
		t.Errorf("mode = %q, want warn after update", p.Mode)
	}

	// Global default: GET then PUT.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/security-policies/_default", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("get default: status %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPut, "/api/v1/security-policies/_default", map[string]any{
		"mode": "block", "threshold": "high",
	}))
	if rw.Code != http.StatusOK {
		t.Fatalf("set default: status %d (%s)", rw.Code, rw.Body.String())
	}
	if def, _ := srv.VulnPolicy.Default(); def.Mode != vuln.ModeBlock {
		t.Errorf("default mode = %q, want block", def.Mode)
	}

	// DELETE the named policy.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/security-policies/block-high", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("delete policy: status %d", rw.Code)
	}
}
