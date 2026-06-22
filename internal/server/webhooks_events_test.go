package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/oci"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/webhook"
)

func TestOCIManifestRef(t *testing.T) {
	cases := []struct {
		sub         string
		image, ref  string
		ok          bool
	}{
		{"myapp/manifests/v1.0", "myapp", "v1.0", true},
		{"org/team/app/manifests/latest", "org/team/app", "latest", true},
		{"myapp/manifests/sha256:abc", "myapp", "sha256:abc", true},
		{"myapp/blobs/sha256:abc", "", "", false},
		{"myapp/blobs/uploads/uuid", "", "", false},
		{"myapp/tags/list", "", "", false},
		{"myapp/manifests/", "", "", false},
		{"/manifests/v1", "", "", false},
		{"myapp/manifests/v1/extra", "", "", false},
	}
	for _, c := range cases {
		image, ref, ok := ociManifestRef(c.sub)
		if image != c.image || ref != c.ref || ok != c.ok {
			t.Errorf("ociManifestRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.sub, image, ref, ok, c.image, c.ref, c.ok)
		}
	}
}

// newEventServer wires a Server with a webhook engine + worker delivering to a
// receiver, plus the OCI handler and one hosted OCI repo. Returns the server, a
// channel of delivered events, and a cleanup func.
func newEventServer(t *testing.T) (*Server, <-chan webhook.Event, func()) {
	t.Helper()
	events := make(chan webhook.Event, 16)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev webhook.Event
		_ = json.Unmarshal(body, &ev)
		events <- ev
		w.WriteHeader(http.StatusOK)
	}))

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	mgr.Add(repo.Repository{Name: "oci-hosted", Format: "oci", Kind: repo.Hosted, Enabled: true})
	reg := format.NewRegistry()
	reg.Register(oci.New())

	q := queue.NewMem(32)
	eng := webhook.New(m, q, recv.Client())
	if _, err := eng.Store().Create(webhook.Subscription{
		Name: "t", URL: recv.URL, Secret: "k", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(mgr, reg, b, m, nil).WithWebhooks(eng)

	ctx, cancel := context.WithCancel(context.Background())
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	return srv, events, func() {
		cancel()
		recv.Close()
	}
}

// awaitEvent waits for the next delivered event or fails.
func awaitEvent(t *testing.T, ch <-chan webhook.Event) webhook.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for webhook event")
		return webhook.Event{}
	}
}

func expectNoEvent(t *testing.T, ch <-chan webhook.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("expected no webhook event, got %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestMiddleware_OCIManifestPUT_EmitsPublished(t *testing.T) {
	srv, events, done := newEventServer(t)
	defer done()

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	req := httptest.NewRequest(http.MethodPut, "/v2/oci-hosted/myapp/manifests/v1.0", bytes.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("manifest PUT status = %d, want 201", rw.Code)
	}

	ev := awaitEvent(t, events)
	if ev.Type != webhook.EventArtifactPublished {
		t.Fatalf("type = %q, want %q", ev.Type, webhook.EventArtifactPublished)
	}
	if ev.Format != "oci" || ev.Repo != "oci-hosted" || ev.Path != "myapp:v1.0" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestMiddleware_OCIBlobUpload_DoesNotEmit(t *testing.T) {
	srv, events, done := newEventServer(t)
	defer done()

	// A monolithic blob upload is a layer push, not a publish — no event.
	blobBody := []byte("layerbytes")
	sum := sha256.Sum256(blobBody)
	dgst := "sha256:" + hex.EncodeToString(sum[:])
	req := httptest.NewRequest(http.MethodPost,
		"/v2/oci-hosted/myapp/blobs/uploads/?digest="+dgst, bytes.NewReader(blobBody))
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("blob upload status = %d, want 201", rw.Code)
	}
	expectNoEvent(t, events)
}

func TestMiddleware_OCIManifestDELETE_EmitsDeleted(t *testing.T) {
	srv, events, done := newEventServer(t)
	defer done()

	manifest := []byte(`{"schemaVersion":2}`)
	put := httptest.NewRequest(http.MethodPut, "/v2/oci-hosted/myapp/manifests/v1.0", bytes.NewReader(manifest))
	srv.Routes().ServeHTTP(httptest.NewRecorder(), put)
	_ = awaitEvent(t, events) // drain the publish

	del := httptest.NewRequest(http.MethodDelete, "/v2/oci-hosted/myapp/manifests/v1.0", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, del)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("manifest DELETE status = %d, want 202", rw.Code)
	}

	ev := awaitEvent(t, events)
	if ev.Type != webhook.EventArtifactDeleted || ev.Path != "myapp:v1.0" || ev.Format != "oci" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestMiddleware_RepositoryDELETE_EmitsDeleted(t *testing.T) {
	srv, events, done := newEventServer(t)
	defer done()
	srv.Repos.Add(repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, Enabled: true})
	srv.Handlers.Register(stubDeletable{})

	req := httptest.NewRequest(http.MethodDelete, "/repository/maven-hosted/g/a/1.0/a.jar", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("repository DELETE status = %d, want 200", rw.Code)
	}

	ev := awaitEvent(t, events)
	if ev.Type != webhook.EventArtifactDeleted || ev.Repo != "maven-hosted" || ev.Path != "g/a/1.0/a.jar" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

// stubDeletable is a minimal format handler that 200s on DELETE, standing in
// for a format-native delete (npm unpublish, maven delete, etc.).
type stubDeletable struct{}

func (stubDeletable) Format() string { return "maven" }
func (stubDeletable) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	w.WriteHeader(http.StatusOK)
}
