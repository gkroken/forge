package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/vuln"
)

func doAdmin(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rdr)
	w := httptest.NewRecorder()
	// Auth nil → enforcer is AllowAll, RequireAdmin passes.
	srv.handleSecurityPolicies(w, r)
	return w
}

func TestSecurityPolicyAPI_CRUDAndDefault(t *testing.T) {
	srv := newGateServer(t)

	// Create a named policy.
	np := vuln.NamedPolicy{Name: "strict", Description: "block highs",
		Policy: vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh, FailOpen: true}}
	if w := doAdmin(t, srv, http.MethodPost, "/api/v1/security-policies", np); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}

	// List shows it.
	w := doAdmin(t, srv, http.MethodGet, "/api/v1/security-policies", nil)
	var list []vuln.NamedPolicy
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "strict" || list[0].Mode != vuln.ModeBlock {
		t.Fatalf("list = %+v", list)
	}

	// Global default round-trip.
	def := vuln.Policy{Mode: vuln.ModeWarn, Threshold: vuln.SeverityModerate}
	if w := doAdmin(t, srv, http.MethodPut, "/api/v1/security-policies/_default", def); w.Code != http.StatusOK {
		t.Fatalf("set default: %d", w.Code)
	}
	w = doAdmin(t, srv, http.MethodGet, "/api/v1/security-policies/_default", nil)
	var gotDef vuln.Policy
	json.Unmarshal(w.Body.Bytes(), &gotDef)
	if gotDef.Mode != vuln.ModeWarn {
		t.Fatalf("default mode = %v", gotDef.Mode)
	}

	// Delete the named policy.
	if w := doAdmin(t, srv, http.MethodDelete, "/api/v1/security-policies/strict", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
}

func TestRepoSecurityPolicy_AssignResolves(t *testing.T) {
	srv := newGateServer(t)
	srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeWarn, Threshold: vuln.SeverityModerate}) //nolint:errcheck
	srv.VulnPolicy.Put(vuln.NamedPolicy{Name: "strict",                                           //nolint:errcheck
		Policy: vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh}})

	assign := func(pn string) resolvedSecurity {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/repos/npm-hosted/security-policy",
			bytes.NewReader([]byte(`{"policyName":"`+pn+`"}`)))
		w := httptest.NewRecorder()
		srv.handleRepoSecurityPolicy(w, r, "npm-hosted")
		if w.Code != http.StatusOK {
			t.Fatalf("assign %q: %d %s", pn, w.Code, w.Body)
		}
		var rs resolvedSecurity
		json.Unmarshal(w.Body.Bytes(), &rs)
		return rs
	}

	if rs := assign("strict"); rs.Source != "named:strict" || rs.Policy.Mode != vuln.ModeBlock {
		t.Fatalf("named assign resolved = %+v", rs)
	}
	if rp, _ := srv.Repos.Get("npm-hosted"); rp.SecurityPolicyName != "strict" {
		t.Fatalf("repo not persisted, got %q", rp.SecurityPolicyName)
	}
	// Clearing falls back to the global default.
	if rs := assign(""); rs.Source != "default" || rs.Policy.Mode != vuln.ModeWarn {
		t.Fatalf("cleared assign resolved = %+v", rs)
	}

	// Unknown policy name rejected.
	r := httptest.NewRequest(http.MethodPut, "/api/v1/repos/npm-hosted/security-policy",
		bytes.NewReader([]byte(`{"policyName":"ghost"}`)))
	w := httptest.NewRecorder()
	srv.handleRepoSecurityPolicy(w, r, "npm-hosted")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown policy: %d want 400", w.Code)
	}
}

func TestSecurityDryRun_BlastRadius(t *testing.T) {
	srv := newGateServer(t) // lodash@4.17.20 has a HIGH finding
	// Add a second vulnerable version + a moderate one.
	srv.Vuln.Put("npm-hosted", vuln.Finding{Component: "lodash", Version: "3.0.0", //nolint:errcheck
		Advisories: []vuln.Advisory{{ID: "GHSA-y", Severity: vuln.SeverityCritical}}})
	srv.Vuln.Put("npm-hosted", vuln.Finding{Component: "left-pad", Version: "1.0.0", //nolint:errcheck
		Advisories: []vuln.Advisory{{ID: "GHSA-z", Severity: vuln.SeverityModerate}}})
	srv.VulnPolicy.Put(vuln.NamedPolicy{Name: "strict", //nolint:errcheck
		Policy: vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh}})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/security-policy/dry-run",
		bytes.NewReader([]byte(`{"policyName":"strict"}`)))
	w := httptest.NewRecorder()
	srv.handleRepoSecurityDryRun(w, r, "npm-hosted")
	if w.Code != http.StatusOK {
		t.Fatalf("dry-run: %d %s", w.Code, w.Body)
	}
	var d securityDryRun
	json.Unmarshal(w.Body.Bytes(), &d)
	// high + critical lodash versions block; moderate left-pad doesn't.
	if d.BlockedVersions != 2 || d.BlockedComponents != 1 || d.TotalScanned != 3 {
		t.Fatalf("blast radius = %+v, want blocked 2/1 of 3", d)
	}
}
