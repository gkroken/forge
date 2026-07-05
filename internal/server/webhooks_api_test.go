package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/webhook"
)

// newLocalWebhookServer builds a webhook-enabled server whose SSRF guard allows
// private targets, plus a local receiver, so test-delivery actually completes
// and a delivery record is written.
func newLocalWebhookServer(t *testing.T) (*Server, *int32, string) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	var hits int32
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recv.Close)

	eng := webhook.New(m, queue.NewMem(8), nil).WithSSRFGuard(webhook.NewSSRFGuard(true))
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil).WithWebhooks(eng)
	return srv, &hits, recv.URL
}

func TestWebhookAPI_ListTestDeliveries(t *testing.T) {
	srv, hits, recvURL := newLocalWebhookServer(t)
	h := srv.Routes()

	// Empty list first.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/webhooks", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list empty: status %d", rw.Code)
	}
	var subs []webhook.Subscription
	json.NewDecoder(rw.Body).Decode(&subs)
	if len(subs) != 0 {
		t.Fatalf("expected empty list, got %d", len(subs))
	}

	// Create a subscription pointing at the local receiver.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks",
		map[string]any{"name": "local", "url": recvURL, "secret": "shh", "enabled": true}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create: status %d (%s)", rw.Code, rw.Body.String())
	}
	var created webhook.Subscription
	json.NewDecoder(rw.Body).Decode(&created)

	// List now returns it, with the secret blanked.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/webhooks", nil))
	json.NewDecoder(rw.Body).Decode(&subs)
	if len(subs) != 1 || subs[0].Secret != "" {
		t.Fatalf("list: got %+v, want one sub with blank secret", subs)
	}

	// Test-fire the webhook → delivered to the receiver, ok:true.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks/"+created.ID+"/test", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("test: status %d (%s)", rw.Code, rw.Body.String())
	}
	var testResp map[string]any
	json.NewDecoder(rw.Body).Decode(&testResp)
	if testResp["ok"] != true {
		t.Errorf("test delivery: got %v, want ok:true", testResp)
	}
	if atomic.LoadInt32(hits) == 0 {
		t.Error("receiver was never hit by the test delivery")
	}

	// Deliveries list now has a record.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/webhooks/"+created.ID+"/deliveries", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("deliveries: status %d", rw.Code)
	}
	var recs []webhook.DeliveryRecord
	if err := json.NewDecoder(rw.Body).Decode(&recs); err != nil {
		t.Errorf("deliveries body not a JSON array: %v", err)
	}

	// Deliveries for an unknown id → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodGet, "/api/v1/webhooks/nope/deliveries", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("deliveries unknown: status %d, want 404", rw.Code)
	}
}
