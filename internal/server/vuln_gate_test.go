package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/repo"
	"forge/internal/vuln"
)

// newGateServer builds a server with an npm-hosted repo holding a lodash tarball
// blob, a high-severity finding for it, and a policy manager. The OSV client is
// left nil — the gate enforces against persisted findings only.
func newGateServer(t *testing.T) *Server {
	t.Helper()
	srv := newVulnServer(t, "") // npm-hosted + helm-hosted, vuln store wired
	srv.OSV = nil
	srv.VulnPolicy = vuln.NewPolicyManager(srv.Meta)
	// handleRepo returns 503 for disabled repos; the harness adds them without
	// Enabled set, so flip it on for the serve path.
	srv.Repos.Update(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true})    //nolint:errcheck
	srv.Repos.Update(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted, Enabled: true}) //nolint:errcheck

	// A primary artifact to download.
	if _, err := srv.Blob.Put("npm-hosted/lodash/-/lodash-4.17.20.tgz", bytes.NewReader([]byte("tgz-bytes"))); err != nil {
		t.Fatal(err)
	}
	// A high-severity finding for it.
	if err := srv.Vuln.Put("npm-hosted", vuln.Finding{
		Component: "lodash", Version: "4.17.20", Source: vuln.SourceOSV,
		Advisories: []vuln.Advisory{{ID: "GHSA-x", Severity: vuln.SeverityHigh, URL: "https://osv.dev/GHSA-x"}},
	}); err != nil {
		t.Fatal(err)
	}
	return srv
}

func getTarball(srv *Server, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/repository/npm-hosted/"+path, nil)
	w := httptest.NewRecorder()
	srv.handleRepo(w, r)
	return w
}

func TestVulnGate_Block(t *testing.T) {
	srv := newGateServer(t)
	if err := srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh, FailOpen: true}); err != nil {
		t.Fatal(err)
	}
	w := getTarball(srv, "lodash/-/lodash-4.17.20.tgz")
	if w.Code != http.StatusForbidden {
		t.Fatalf("blocked download: status %d want 403", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("GHSA-x")) {
		t.Errorf("403 body should carry advisory link, got %s", w.Body.String())
	}
}

func TestVulnGate_Warn(t *testing.T) {
	srv := newGateServer(t)
	if err := srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeWarn, Threshold: vuln.SeverityHigh, FailOpen: true}); err != nil {
		t.Fatal(err)
	}
	w := getTarball(srv, "lodash/-/lodash-4.17.20.tgz")
	if w.Code != http.StatusOK {
		t.Fatalf("warned download: status %d want 200", w.Code)
	}
	if h := w.Header().Get("X-Forge-Vulnerabilities"); h == "" {
		t.Error("warn must set X-Forge-Vulnerabilities header")
	}
	if w.Body.String() != "tgz-bytes" {
		t.Errorf("warn must still serve the artifact, got %q", w.Body.String())
	}
}

func TestVulnGate_OffServes(t *testing.T) {
	srv := newGateServer(t)
	// No default set → resolves to Off.
	w := getTarball(srv, "lodash/-/lodash-4.17.20.tgz")
	if w.Code != http.StatusOK || w.Body.String() != "tgz-bytes" {
		t.Fatalf("off policy must serve: status %d body %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Forge-Vulnerabilities") != "" {
		t.Error("off policy must not set warning header")
	}
}

func TestVulnGate_FailOpenUnscanned(t *testing.T) {
	srv := newGateServer(t)
	if err := srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh, FailOpen: true}); err != nil {
		t.Fatal(err)
	}
	// A different, unscanned version present in the blob store.
	if _, err := srv.Blob.Put("npm-hosted/lodash/-/lodash-9.9.9.tgz", bytes.NewReader([]byte("clean"))); err != nil {
		t.Fatal(err)
	}
	w := getTarball(srv, "lodash/-/lodash-9.9.9.tgz")
	if w.Code != http.StatusOK {
		t.Fatalf("fail-open unscanned must serve: status %d", w.Code)
	}
}

func TestVulnGate_MetadataNotGated(t *testing.T) {
	srv := newGateServer(t)
	if err := srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh, FailOpen: false}); err != nil {
		t.Fatal(err)
	}
	// The packument path is not a primary artifact: even fail-closed Block must
	// not 403 it (otherwise the registry metadata becomes unreachable).
	w := getTarball(srv, "lodash")
	if w.Code == http.StatusForbidden {
		t.Fatalf("packument must not be gated, got 403")
	}
}

func TestVulnGate_NotConfigured(t *testing.T) {
	srv := newGateServer(t)
	srv.VulnPolicy = nil // gate disabled entirely
	w := getTarball(srv, "lodash/-/lodash-4.17.20.tgz")
	if w.Code != http.StatusOK {
		t.Fatalf("no policy manager → serve, status %d", w.Code)
	}
}

// Sanity: a format without VulnGate (helm) is never gated.
func TestVulnGate_FormatNotGateable(t *testing.T) {
	srv := newGateServer(t)
	if err := srv.VulnPolicy.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityLow, FailOpen: false}); err != nil {
		t.Fatal(err)
	}
	srv.Repos.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted}) //nolint:errcheck
	r := httptest.NewRequest(http.MethodGet, "/repository/helm-hosted/mychart-1.0.0.tgz", nil)
	w := httptest.NewRecorder()
	srv.handleRepo(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatal("helm has no VulnGate; must not be blocked")
	}
}
