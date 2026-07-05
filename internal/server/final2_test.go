package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/queue"
	"forge/internal/webhook"
)

// TestCreateRepo_RichFieldsAndInvalidDurations covers toRepository's optional
// field wiring (durations, enabled, quota, claims) and its duration-parse errors.
func TestCreateRepo_RichFieldsAndInvalidDurations(t *testing.T) {
	srv := newAdminServer(t)
	h := srv.Routes()

	// A proxy repo exercising ContentMaxAge, MetadataMaxAge, enabled, negativeCache.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "npm-proxy", "format": "npm", "kind": "proxy",
		"upstream": "https://registry.npmjs.org", "anonymousRead": true,
		"contentMaxAge": "6h", "metadataMaxAge": "10m", "enabled": true,
		"negativeCache": false, "quotaGB": 5,
	}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create rich proxy: status %d (%s)", rw.Code, rw.Body.String())
	}
	rp, _ := srv.Repos.Get("npm-proxy")
	if rp.ContentMaxAge == nil || rp.MetadataMaxAge == nil {
		t.Errorf("durations not parsed: %+v", rp)
	}

	// Invalid contentMaxAge → 400 from toRepository.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "bad", "format": "npm", "kind": "hosted", "contentMaxAge": "not-a-duration",
	}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("invalid contentMaxAge: status %d, want 400", rw.Code)
	}

	// ProxyTTL branch (legacy) + invalid metadataMaxAge.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/repos", map[string]any{
		"name": "leg", "format": "npm", "kind": "proxy", "upstream": "https://x",
		"proxyTTL": "1h", "metadataMaxAge": "bogus",
	}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("invalid metadataMaxAge: status %d, want 400", rw.Code)
	}
}

// TestTrivyHelmJobHandlers covers the remaining best-effort worker handlers.
func TestTrivyHelmJobHandlers(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "helm-hosted", "helm")
	ctx := context.Background()

	if err := s.handleTrivyScanJob(ctx, queue.Job{Type: "trivy.scan", Payload: []byte("!")}); err != nil {
		t.Errorf("trivy bad payload: %v", err)
	}
	if err := s.handleTrivyScanJob(ctx, job(t, "trivy.scan", trivyScanPayload{Repo: "no-such", Image: "app", Tag: "v1"})); err != nil {
		t.Errorf("trivy valid payload: %v", err)
	}
	if err := s.handleHelmRepoScanJob(ctx, queue.Job{Type: "helm.scan", Payload: []byte("!")}); err != nil {
		t.Errorf("helm bad payload: %v", err)
	}
	if err := s.handleHelmRepoScanJob(ctx, job(t, "helm.scan", helmRepoScanPayload{Repo: "helm-hosted"})); err != nil {
		t.Errorf("helm valid payload: %v", err)
	}
}

// TestTestWebhook_DeliveryFailure covers testWebhook's failure branch: a
// subscription to an unreachable target returns ok:false rather than erroring.
func TestTestWebhook_DeliveryFailure(t *testing.T) {
	s := newMigrationServer(t)
	eng := webhook.New(s.Meta, queue.NewMem(4), nil).WithSSRFGuard(webhook.NewSSRFGuard(true))
	s.WithWebhooks(eng)
	// Create a subscription pointing at a dead port (allowed by the guard, but
	// the connection is refused → delivery fails → ok:false).
	sub, err := eng.Store().Create(webhook.Subscription{Name: "dead", URL: "http://127.0.0.1:1/dead", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks/"+sub.ID+"/test", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("test webhook: status %d", rw.Code)
	}
	// A missing id → 404 (covers the not-found branch).
	rw = httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks/ghost/test", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("test missing webhook: status %d, want 404", rw.Code)
	}
}
