package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/obs"
	"forge/internal/queue"
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
