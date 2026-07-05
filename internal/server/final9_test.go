package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBroadHandlerBranches exercises a spread of remaining handler branches:
// system chart 405s, integrity rollup, search, webhook delete, dry-run method
// guard, and the cleanup policy list.
func TestBroadHandlerBranches(t *testing.T) {
	rich := newRichUIServer(t)
	h := rich.Routes()

	// system chart endpoints: non-GET → 405.
	for _, res := range []string{"request-chart", "metrics-chart", "status-breakdown", "tasks", "health"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/system/"+res, nil))
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST system/%s: %d, want 405", res, rw.Code)
		}
	}

	// integrity rollup.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/integrity", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("integrity rollup: %d, want 200", rw.Code)
	}

	// search over components.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/search?q=lo", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("search: %d, want 200", rw.Code)
	}

	// dry-run with a GET (POST-only) → 405.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/repos/npm-hosted/security-policy/dry-run", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET dry-run: %d, want 405", rw.Code)
	}

	// cleanup-policies list.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/cleanup-policies", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("cleanup-policies list: %d, want 200", rw.Code)
	}
}

// TestWebhookDeleteAndNotFound covers the webhook DELETE path and unknown-id 404s.
func TestWebhookDeleteAndNotFound(t *testing.T) {
	srv, _, recvURL := newLocalWebhookServer(t)
	h := srv.Routes()

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks",
		map[string]any{"name": "d", "url": recvURL, "enabled": true}))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(rw.Body).Decode(&created) //nolint:errcheck

	// DELETE it → 204.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodDelete, "/api/v1/webhooks/"+created.ID, nil))
	if rw.Code != http.StatusNoContent {
		t.Errorf("delete webhook: %d, want 204", rw.Code)
	}

	// Unknown sub-resource → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/webhooks/x/bogus", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown webhook sub-resource: %d, want 404", rw.Code)
	}
}
