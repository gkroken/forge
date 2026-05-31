package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// newCtx returns a Context for a hosted OCI repo backed by temp FS stores.
func newCtx(t *testing.T) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return &format.Context{
		Repo: repo.Repository{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

func serve(t *testing.T, c *format.Context, method, sub string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/v2/docker-hosted/"+sub, bodyReader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// c.Sub mirrors how server.go sets it: path only, no query string.
	pathOnly, _, _ := strings.Cut(sub, "?")
	c.Sub = pathOnly
	rw := httptest.NewRecorder()
	New().Serve(rw, req, c)
	return rw
}

// --- parseOCISub ----------------------------------------------------------

func TestParseOCISub(t *testing.T) {
	cases := []struct {
		sub   string
		image string
		op    string
		ref   string
		ok    bool
	}{
		{"myapp/manifests/latest", "myapp", "manifests", "latest", true},
		{"myapp/manifests/sha256:abc", "myapp", "manifests", "sha256:abc", true},
		{"myapp/blobs/sha256:abc", "myapp", "blobs", "sha256:abc", true},
		{"myapp/blobs/uploads/", "myapp", "blobs/uploads", "", true},
		{"myapp/blobs/uploads/uuid123", "myapp", "blobs/uploads", "uuid123", true},
		{"myapp/blobs/uploads", "myapp", "blobs/uploads", "", true},
		{"myapp/tags/list", "myapp", "tags/list", "", true},
		{"org/image/manifests/v1.0", "org/image", "manifests", "v1.0", true},
		{"org/image/tags/list", "org/image", "tags/list", "", true},
		{"invalid", "", "", "", false},
	}
	for _, tc := range cases {
		img, op, ref, ok := parseOCISub(tc.sub)
		if ok != tc.ok || img != tc.image || op != tc.op || ref != tc.ref {
			t.Errorf("parseOCISub(%q): got (%q,%q,%q,%v) want (%q,%q,%q,%v)",
				tc.sub, img, op, ref, ok, tc.image, tc.op, tc.ref, tc.ok)
		}
	}
}

// --- blob round-trip -------------------------------------------------------

func TestBlob_MonolithicUpload(t *testing.T) {
	c := newCtx(t)
	data := []byte("hello world blob data")
	dgst := computeDigest(data)

	// POST /blobs/uploads/?digest=... with body
	rw := serve(t, c, http.MethodPost,
		fmt.Sprintf("myapp/blobs/uploads/?digest=%s", dgst),
		data, nil)
	if rw.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", rw.Code, rw.Body)
	}
	if loc := rw.Header().Get("Location"); !strings.Contains(loc, dgst) {
		t.Errorf("Location %q doesn't contain digest", loc)
	}

	// GET /blobs/{digest}
	rw2 := serve(t, c, http.MethodGet, "myapp/blobs/"+dgst, nil, nil)
	if rw2.Code != http.StatusOK {
		t.Fatalf("GET blob status %d", rw2.Code)
	}
	if !bytes.Equal(rw2.Body.Bytes(), data) {
		t.Error("blob data mismatch")
	}
	if rw2.Header().Get("Docker-Content-Digest") != dgst {
		t.Error("missing Docker-Content-Digest header")
	}
}

func TestBlob_ChunkedUpload(t *testing.T) {
	c := newCtx(t)
	part1 := []byte("chunk one ")
	part2 := []byte("chunk two")
	full := append(part1, part2...)
	dgst := computeDigest(full)

	// POST to initiate
	rw := serve(t, c, http.MethodPost, "myapp/blobs/uploads/", nil, nil)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("initiate status %d", rw.Code)
	}
	uuid := rw.Header().Get("OCI-Upload-UUID")
	if uuid == "" {
		t.Fatal("missing OCI-Upload-UUID")
	}

	// PATCH first chunk
	rw2 := serve(t, c, http.MethodPatch,
		"myapp/blobs/uploads/"+uuid, part1, nil)
	if rw2.Code != http.StatusAccepted {
		t.Fatalf("PATCH status %d", rw2.Code)
	}

	// PATCH second chunk
	rw3 := serve(t, c, http.MethodPatch,
		"myapp/blobs/uploads/"+uuid, part2, nil)
	if rw3.Code != http.StatusAccepted {
		t.Fatalf("PATCH2 status %d", rw3.Code)
	}

	// PUT to finalize
	rw4 := serve(t, c, http.MethodPut,
		fmt.Sprintf("myapp/blobs/uploads/%s?digest=%s", uuid, dgst), nil, nil)
	if rw4.Code != http.StatusCreated {
		t.Fatalf("PUT finalize status %d: %s", rw4.Code, rw4.Body)
	}

	// GET blob
	rw5 := serve(t, c, http.MethodGet, "myapp/blobs/"+dgst, nil, nil)
	if rw5.Code != http.StatusOK {
		t.Fatalf("GET blob status %d", rw5.Code)
	}
	if !bytes.Equal(rw5.Body.Bytes(), full) {
		t.Error("chunked blob data mismatch")
	}
}

func TestBlob_Head(t *testing.T) {
	c := newCtx(t)
	data := []byte("content")
	dgst := computeDigest(data)

	serve(t, c, http.MethodPost, fmt.Sprintf("app/blobs/uploads/?digest=%s", dgst), data, nil)

	rw := serve(t, c, http.MethodHead, "app/blobs/"+dgst, nil, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("HEAD status %d", rw.Code)
	}
	if rw.Header().Get("Docker-Content-Digest") != dgst {
		t.Error("missing Docker-Content-Digest on HEAD")
	}
	if rw.Body.Len() != 0 {
		t.Error("HEAD should return no body")
	}
}

func TestBlob_NotFound(t *testing.T) {
	c := newCtx(t)
	rw := serve(t, c, http.MethodGet, "app/blobs/sha256:"+strings.Repeat("0", 64), nil, nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestBlob_DigestMismatch(t *testing.T) {
	c := newCtx(t)
	data := []byte("hello")
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	rw := serve(t, c, http.MethodPost,
		fmt.Sprintf("app/blobs/uploads/?digest=%s", wrongDigest), data, nil)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on digest mismatch, got %d", rw.Code)
	}
}

func TestBlob_CrossRepoMount(t *testing.T) {
	c := newCtx(t)
	data := []byte("shared blob")
	dgst := computeDigest(data)

	// Pre-populate the source repo's blob directly.
	c.Blob.Put("source-repo/blobs/"+dgst, bytes.NewReader(data))

	rw := serve(t, c, http.MethodPost,
		fmt.Sprintf("app/blobs/uploads/?mount=%s&from=source-repo", dgst), nil, nil)
	if rw.Code != http.StatusCreated {
		t.Fatalf("cross-repo mount status %d: %s", rw.Code, rw.Body)
	}

	// Verify blob is now accessible in this repo.
	rw2 := serve(t, c, http.MethodGet, "app/blobs/"+dgst, nil, nil)
	if rw2.Code != http.StatusOK {
		t.Fatalf("GET after mount status %d", rw2.Code)
	}
}

// --- manifest round-trip --------------------------------------------------

func TestManifest_PushPullByTag(t *testing.T) {
	c := newCtx(t)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	dgst := computeDigest(manifest)
	mt := "application/vnd.oci.image.manifest.v1+json"

	// PUT by tag
	rw := serve(t, c, http.MethodPut, "myapp/manifests/v1.0", manifest,
		map[string]string{"Content-Type": mt})
	if rw.Code != http.StatusCreated {
		t.Fatalf("PUT manifest status %d: %s", rw.Code, rw.Body)
	}
	if rw.Header().Get("Docker-Content-Digest") != dgst {
		t.Errorf("wrong digest: got %q want %q", rw.Header().Get("Docker-Content-Digest"), dgst)
	}

	// GET by tag
	rw2 := serve(t, c, http.MethodGet, "myapp/manifests/v1.0", nil, nil)
	if rw2.Code != http.StatusOK {
		t.Fatalf("GET by tag status %d", rw2.Code)
	}
	if !bytes.Equal(rw2.Body.Bytes(), manifest) {
		t.Error("manifest data mismatch")
	}
	if rw2.Header().Get("Content-Type") != mt {
		t.Errorf("Content-Type %q, want %q", rw2.Header().Get("Content-Type"), mt)
	}

	// GET by digest
	rw3 := serve(t, c, http.MethodGet, "myapp/manifests/"+dgst, nil, nil)
	if rw3.Code != http.StatusOK {
		t.Fatalf("GET by digest status %d", rw3.Code)
	}
}

func TestManifest_Head(t *testing.T) {
	c := newCtx(t)
	manifest := []byte(`{"schemaVersion":2}`)
	serve(t, c, http.MethodPut, "app/manifests/latest", manifest,
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})

	rw := serve(t, c, http.MethodHead, "app/manifests/latest", nil, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("HEAD status %d", rw.Code)
	}
	if rw.Body.Len() != 0 {
		t.Error("HEAD should return no body")
	}
	if rw.Header().Get("Docker-Content-Digest") == "" {
		t.Error("missing Docker-Content-Digest on HEAD")
	}
}

func TestManifest_NotFound(t *testing.T) {
	c := newCtx(t)
	rw := serve(t, c, http.MethodGet, "app/manifests/doesnotexist", nil, nil)
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.Code)
	}
}

func TestManifest_Delete(t *testing.T) {
	c := newCtx(t)
	manifest := []byte(`{"schemaVersion":2}`)
	dgst := computeDigest(manifest)
	serve(t, c, http.MethodPut, "app/manifests/v1.0", manifest,
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})

	// Delete by digest.
	rw := serve(t, c, http.MethodDelete, "app/manifests/"+dgst, nil, nil)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("DELETE status %d", rw.Code)
	}

	// Tag should also be gone.
	rw2 := serve(t, c, http.MethodGet, "app/manifests/v1.0", nil, nil)
	if rw2.Code != http.StatusNotFound {
		t.Errorf("expected tag to be removed, got %d", rw2.Code)
	}
}

// --- tags -----------------------------------------------------------------

func TestTags_List(t *testing.T) {
	c := newCtx(t)
	for _, tag := range []string{"v1.0", "v1.1", "latest"} {
		manifest := []byte(fmt.Sprintf(`{"tag":%q}`, tag))
		serve(t, c, http.MethodPut, "myapp/manifests/"+tag, manifest,
			map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	}

	rw := serve(t, c, http.MethodGet, "myapp/tags/list", nil, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("list tags status %d", rw.Code)
	}
	var resp map[string]any
	json.NewDecoder(rw.Body).Decode(&resp)
	tags, _ := resp["tags"].([]any)
	if len(tags) != 3 {
		t.Errorf("expected 3 tags, got %d", len(tags))
	}
}

func TestTags_Empty(t *testing.T) {
	c := newCtx(t)
	rw := serve(t, c, http.MethodGet, "emptyimage/tags/list", nil, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d", rw.Code)
	}
	var resp map[string]any
	json.NewDecoder(rw.Body).Decode(&resp)
	tags, _ := resp["tags"].([]any)
	if len(tags) != 0 {
		t.Errorf("expected empty tag list, got %v", tags)
	}
}

// --- computeDigest --------------------------------------------------------

func TestComputeDigest(t *testing.T) {
	// Known SHA256 of "hello"
	got := computeDigest([]byte("hello"))
	want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("computeDigest: got %q want %q", got, want)
	}
}

func TestFormat_OCI(t *testing.T) {
	if got := New().Format(); got != "oci" {
		t.Fatalf("Format() = %q, want oci", got)
	}
}

func TestDeleteBlob(t *testing.T) {
	c := newCtx(t)
	// Push a blob via monolithic POST (digest in query param).
	data := []byte("some-blob-content")
	dgst := computeDigest(data)
	serve(t, c, "POST", "myimage/blobs/uploads/?digest="+dgst, data, nil)

	// Now delete it — should return 202.
	rw := serve(t, c, "DELETE", "myimage/blobs/"+dgst, nil, nil)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("deleteBlob: got %d", rw.Code)
	}
	// Subsequent HEAD should 404.
	rw = serve(t, c, "HEAD", "myimage/blobs/"+dgst, nil, nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rw.Code)
	}
}

func TestBrowseRepo_OCI(t *testing.T) {
	c := newCtx(t)

	// Push a minimal manifest so the handler records the tag in meta.
	configData := []byte("{}")
	configDgst := computeDigest(configData)
	serve(t, c, "POST", "myimage/blobs/uploads/?digest="+configDgst, configData, nil)

	manifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":%d},"layers":[]}`,
		configDgst, len(configData)))
	serve(t, c, "PUT", "myimage/manifests/v1.0", manifest,
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "myimage" {
			found = true
			for _, tag := range e.Versions {
				if tag == "v1.0" {
					return
				}
			}
			t.Fatalf("myimage found but tag v1.0 missing: %v", e.Versions)
		}
	}
	if !found {
		t.Fatalf("myimage not found in browse entries: %v", entries)
	}
}

func TestProxy_RejectsWrites(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "oci-proxy", Format: "oci", Kind: repo.Proxy,
			Upstream: "https://registry-1.docker.io"},
		Blob: b, Meta: m,
		HTTP: &http.Client{},
	}
	c.Sub = "library/ubuntu/blobs/uploads/"
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(http.MethodPost, "/", nil), c)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}
