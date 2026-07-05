package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSystemAPI_AllResources drives every /api/v1/system/{resource} endpoint on
// an eval-mode server (auth off → RequireAdmin passes). GlobalStats is nil here,
// so the chart endpoints take their empty-payload branch; each must still return
// 200 with valid JSON, and unknown resources must 404.
func TestSystemAPI_AllResources(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	for _, res := range []string{"health", "tasks", "request-chart", "metrics-chart", "status-breakdown"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/system/"+res, nil))
		if rw.Code != http.StatusOK {
			t.Errorf("GET system/%s: status %d, want 200 (%s)", res, rw.Code, rw.Body.String())
		}
		if ct := rw.Header().Get("Content-Type"); ct == "" {
			t.Errorf("GET system/%s: missing Content-Type", res)
		}
		// Body must be valid JSON.
		var v any
		if err := json.Unmarshal(rw.Body.Bytes(), &v); err != nil {
			t.Errorf("GET system/%s: invalid JSON: %v", res, err)
		}
	}
}

func TestSystemAPI_Health_Shape(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/system/health", nil))

	var resp systemHealthResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	// The FS blob store implements Capacitor, so total capacity should be > 0.
	if resp.BlobTotalGB <= 0 {
		t.Errorf("BlobTotalGB = %v, want > 0 (FS store reports capacity)", resp.BlobTotalGB)
	}
	if resp.UpstreamHealth == nil {
		t.Error("UpstreamHealth should be a (possibly empty) map, not null")
	}
}

func TestSystemAPI_UnknownResource_404(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/system/nope", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown resource: status %d, want 404", rw.Code)
	}
}

func TestSystemAPI_WrongMethod_405(t *testing.T) {
	srv := newAdminServer(t)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/system/health", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST system/health: status %d, want 405", rw.Code)
	}
}
