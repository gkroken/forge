package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/auth"
)

// TestUIUploadHelmChart drives the browser upload path for a helm chart, covering
// processUpload's helm branch, uploadHelm, and callHandler.
func TestUIUploadHelmChart(t *testing.T) {
	srv := newUIServer(t) // registers helm, has helm-hosted
	chart := makeChartTGZ(t, "uploaded", "0.1.0")
	body, ct := buildMultipartForm(t, "file", "uploaded-0.1.0.tgz", chart)

	req := httptest.NewRequest(http.MethodPost, "/ui/repos/helm-hosted/upload", body)
	req.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("helm upload: status %d (%.200s)", rw.Code, rw.Body.String())
	}
	// The chart is now in the blob store.
	if _, ok, _ := srv.Blob.Stat("helm-hosted/uploaded-0.1.0.tgz"); !ok {
		t.Error("uploaded chart not stored")
	}
}

// TestActorLabel covers the token/cookie identity resolution branches.
func TestActorLabel(t *testing.T) {
	srv, store := newAuthServer(t)
	_, secret, err := store.Create("ci-bot", []auth.Grant{auth.GrantForRole("*", auth.RoleWrite)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// nil auth → anonymous.
	if got := actorLabel(httptest.NewRequest(http.MethodGet, "/", nil), nil); got != "anonymous" {
		t.Errorf("nil auth = %q, want anonymous", got)
	}
	// No credentials → anonymous.
	if got := actorLabel(httptest.NewRequest(http.MethodGet, "/", nil), srv.Auth); got != "anonymous" {
		t.Errorf("no creds = %q, want anonymous", got)
	}
	// Bearer token → the token's description.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	if got := actorLabel(r, srv.Auth); got != "ci-bot" {
		t.Errorf("bearer = %q, want ci-bot", got)
	}
	// Session cookie → the token's description.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: auth.UISessionCookie, Value: secret})
	if got := actorLabel(r, srv.Auth); got != "ci-bot" {
		t.Errorf("cookie = %q, want ci-bot", got)
	}
	// Invalid secret → anonymous.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-real-token")
	if got := actorLabel(r, srv.Auth); got != "anonymous" {
		t.Errorf("bad token = %q, want anonymous", got)
	}
}

// TestSystemChartsWithData hits the system chart endpoints on a GlobalStats-backed
// server so the non-empty snapshot branch is taken.
func TestSystemChartsWithData(t *testing.T) {
	srv := newRichUIServer(t) // GlobalStats wired
	h := srv.Routes()
	for _, res := range []string{"request-chart", "metrics-chart", "status-breakdown"} {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/system/"+res, nil))
		if rw.Code != http.StatusOK {
			t.Errorf("system/%s with stats: status %d", res, rw.Code)
		}
	}
}

// TestUIWebhooksPageWithData renders the webhooks UI page after a subscription
// exists, covering the with-data branch of uiWebhooks.
func TestUIWebhooksPageWithData(t *testing.T) {
	srv, _, recvURL := newLocalWebhookServer(t)
	h := srv.Routes()
	// Create a subscription via the API.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, adminReq(t, http.MethodPost, "/api/v1/webhooks",
		map[string]any{"name": "wh", "url": recvURL, "enabled": true}))
	if rw.Code != http.StatusCreated {
		t.Fatalf("create webhook: status %d", rw.Code)
	}
	// Render the page.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/ui/admin/webhooks", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("webhooks page: status %d (%.200s)", rw.Code, rw.Body.String())
	}
}
