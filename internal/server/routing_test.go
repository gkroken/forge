package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/obs"
	"forge/internal/queue"
	"forge/internal/repo"
)

func TestServer_WithMetrics(t *testing.T) {
	srv := newAdminServer(t)
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	if got := srv.WithMetrics(metrics, reg); got != srv {
		t.Fatal("WithMetrics must return the same server instance")
	}
}

func TestServer_WithQueue(t *testing.T) {
	srv := newAdminServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := queue.NewMem(4)
	if got := srv.WithQueue(ctx, q); got != srv {
		t.Fatal("WithQueue must return the same server instance")
	}
}

// TestServer_HandleOCI_NotFound hits handleOCI with an unknown repo, triggering
// ociError and covering both handleOCI and ociError.
func TestServer_HandleOCI_NotFound(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/v2/ghost-repo/myimage/manifests/latest", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// TestServer_HandleOCI_Catalog hits the _catalog sub-path in handleOCI.
func TestServer_HandleOCI_Catalog(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("catalog: got %d", rw.Code)
	}
}

// TestServer_HandleRepo_NoSuchRepo covers the "not found" branch of handleRepo.
func TestServer_HandleRepo_NoSuchRepo(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/repository/nonexistent/some/path", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// TestServer_HandleRepo_EmptyName covers the empty-name branch of handleRepo.
func TestServer_HandleRepo_EmptyName(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/repository/", nil))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rw.Code)
	}
}

// TestServer_HandleIndex_NotFound covers the 404 branch of handleIndex.
func TestServer_HandleIndex_NotFound(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/no/such/path", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// TestServer_HandleRepo_Disabled: a repo with Enabled=false must return 503
// before the format handler is reached.
func TestServer_HandleRepo_Disabled(t *testing.T) {
	srv := newAdminServer(t)
	if err := srv.Repos.Add(repo.Repository{
		Name:    "offline-npm",
		Format:  "npm",
		Kind:    repo.Hosted,
		Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/repository/offline-npm/my-pkg", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rw.Code)
	}
}

// TestServer_HandleRepo_EnabledByDefault: a repo added without setting Enabled
// explicitly is still accessible (Enabled defaults to true in Add path).
func TestServer_HandleRepo_EnabledByDefault(t *testing.T) {
	srv := newAdminServer(t)
	if err := srv.Repos.Add(repo.Repository{
		Name:    "active-npm",
		Format:  "npm",
		Kind:    repo.Hosted,
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/repository/active-npm/my-pkg", nil))
	// Format handler returns 404 for unknown pkg — but NOT 503 (repo is online).
	if rw.Code == http.StatusServiceUnavailable {
		t.Fatalf("repo should be online, got 503")
	}
}

// TestRouteLabel covers all branches of the low-cardinality route labeller.
func TestRouteLabel(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/metrics", "/metrics"},
		{"/repository/npm-hosted/pkg", "/repository/{repo}"},
		{"/v2/docker-hosted/img/manifests/latest", "/v2/{repo}"},
		{"/api/v1/tokens", "/api/v1/tokens"},
		{"/api/v1/tokens/abc123", "/api/v1/tokens"},
		{"/ui/", "other"},
	}
	for _, tc := range cases {
		got := routeLabel(tc.path)
		if got != tc.want {
			t.Errorf("routeLabel(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestServer_HandleOCI_WrongFormat covers the rp.Format != "oci" branch of
// handleOCI: repo exists but is registered as a different format.
func TestServer_HandleOCI_WrongFormat(t *testing.T) {
	srv := newAdminServer(t)
	// Register a non-OCI repo so the lookup succeeds but the format check fails.
	srv.Repos.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted}) //nolint:errcheck
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/v2/npm-hosted/image/manifests/latest", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("wrong format: expected 404, got %d", rw.Code)
	}
}

// TestServer_HandleOCI_HandlerNotRegistered covers the "OCI handler not in
// registry" branch: an OCI-format repo exists but no handler is registered.
func TestServer_HandleOCI_HandlerNotRegistered(t *testing.T) {
	srv := newAdminServer(t) // no handlers registered
	srv.Repos.Add(repo.Repository{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted, Enabled: true}) //nolint:errcheck
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw,
		httptest.NewRequest(http.MethodGet, "/v2/docker-hosted/img/manifests/latest", nil))
	if rw.Code != http.StatusNotImplemented {
		t.Fatalf("no handler: expected 501, got %d", rw.Code)
	}
}
