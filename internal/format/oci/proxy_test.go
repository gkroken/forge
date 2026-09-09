package oci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// A proxy registry has to do two things this one previously did not: walk the
// Bearer-token handshake that Docker Hub, ghcr.io and quay.io all require, and
// actually cache. Before, proxyPass forwarded bytes and stored nothing, so
// every pull went upstream — no rate-limit relief, no offline resilience.

type tokenRegistry struct {
	*httptest.Server
	pulls   atomic.Int64 // authenticated content requests
	tokens  atomic.Int64 // token exchanges
	body    atomic.Value // current manifest body for :latest
	require bool         // demand a token
}

func newTokenRegistry(t *testing.T, requireToken bool) *tokenRegistry {
	t.Helper()
	reg := &tokenRegistry{require: requireToken}
	reg.body.Store(`{"schemaVersion":2,"rev":"one"}`)
	mux := http.NewServeMux()

	authed := func(r *http.Request) bool {
		return !reg.require || r.Header.Get("Authorization") == "Bearer test-token"
	}
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="%s/token",service="test.registry"`, reg.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}

	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			challenge(w, r)
			return
		}
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/manifests/latest"):
			reg.pulls.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write([]byte(reg.body.Load().(string)))
		case strings.Contains(r.URL.Path, "/blobs/sha256:"):
			reg.pulls.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("BLOB-BYTES"))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		reg.tokens.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "test-token", "expires_in": 300})
	})
	reg.Server = httptest.NewServer(mux)
	t.Cleanup(reg.Close)
	return reg
}

func proxyCtx(t *testing.T, upstream string, client *http.Client) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	return &format.Context{
		Repo: repo.Repository{Name: "oci-proxy", Format: "oci", Kind: repo.Proxy, Upstream: upstream},
		Blob: b, Meta: m, HTTP: client,
	}
}

func pget(t *testing.T, h *Handler, c *format.Context, sub string) *httptest.ResponseRecorder {
	t.Helper()
	c.Sub = sub
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	h.Serve(w, req, c)
	return w
}

// TestProxy_TokenHandshake — without this, pointing a proxy at Docker Hub just
// returns 401.
func TestProxy_TokenHandshake(t *testing.T) {
	reg := newTokenRegistry(t, true)
	h := New()
	c := proxyCtx(t, reg.URL, reg.Client())

	w := pget(t, h, c, "library/alpine/manifests/latest")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "rev") {
		t.Fatalf("token-protected registry: %d %s", w.Code, w.Body.String())
	}
	if n := reg.tokens.Load(); n != 1 {
		t.Errorf("token exchanges = %d, want 1", n)
	}
	// A second image reuses nothing but must not re-ping needlessly; the token
	// is cached per image scope, so at most one more exchange.
	pget(t, h, c, "library/alpine/manifests/latest")
	if n := reg.tokens.Load(); n != 1 {
		t.Errorf("token re-fetched on a second pull (%d exchanges); tokens must be cached", n)
	}
}

// TestProxy_CachesBlobs — a blob is immutable, so a second read must be served
// locally without contacting upstream at all.
func TestProxy_CachesBlobs(t *testing.T) {
	reg := newTokenRegistry(t, false)
	h := New()
	c := proxyCtx(t, reg.URL, reg.Client())
	dgst := "sha256:" + strings.Repeat("a", 64)

	if w := pget(t, h, c, "library/alpine/blobs/"+dgst); w.Code != 200 || w.Body.String() != "BLOB-BYTES" {
		t.Fatalf("first blob read: %d %q", w.Code, w.Body.String())
	}
	before := reg.pulls.Load()
	if w := pget(t, h, c, "library/alpine/blobs/"+dgst); w.Code != 200 || w.Body.String() != "BLOB-BYTES" {
		t.Fatalf("second blob read: %d %q", w.Code, w.Body.String())
	}
	if reg.pulls.Load() != before {
		t.Errorf("second read of an immutable blob contacted upstream (%d → %d); "+
			"the proxy is forwarding, not caching", before, reg.pulls.Load())
	}
}

// TestProxy_TagManifestRevalidates — the counterpart: a TAG is mutable, so it
// must expire. Caching it forever pins :latest to whatever was first pulled,
// which is exactly the npm packument bug in another format.
func TestProxy_TagManifestRevalidates(t *testing.T) {
	reg := newTokenRegistry(t, false)
	h := New()
	c := proxyCtx(t, reg.URL, reg.Client())
	ttl := 50 * time.Millisecond
	c.Repo.MetadataMaxAge = &ttl

	if w := pget(t, h, c, "library/alpine/manifests/latest"); !strings.Contains(w.Body.String(), "one") {
		t.Fatalf("first pull: %s", w.Body.String())
	}
	// Within the TTL, no upstream contact.
	before := reg.pulls.Load()
	pget(t, h, c, "library/alpine/manifests/latest")
	if reg.pulls.Load() != before {
		t.Errorf("fresh tag still contacted upstream")
	}

	// A new image is pushed upstream under the same tag.
	reg.body.Store(`{"schemaVersion":2,"rev":"two"}`)
	time.Sleep(2 * ttl)

	w := pget(t, h, c, "library/alpine/manifests/latest")
	if !strings.Contains(w.Body.String(), "two") {
		t.Errorf("stale tag was not revalidated: %s — :latest is pinned to the first pull",
			w.Body.String())
	}
}

// TestProxy_UnknownIsNotFoundAndNegativeCached — a missing image answers 404,
// and the miss is remembered so the registry is not re-asked every time.
func TestProxy_UnknownIsNotFoundAndNegativeCached(t *testing.T) {
	var upstreamHits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		upstreamHits.Add(1)
		http.NotFound(w, r)
	}))
	defer up.Close()
	h := New()
	c := proxyCtx(t, up.URL, up.Client())

	if w := pget(t, h, c, "nope/manifests/latest"); w.Code != 404 {
		t.Errorf("missing image = %d, want 404", w.Code)
	}
	first := upstreamHits.Load()
	if w := pget(t, h, c, "nope/manifests/latest"); w.Code != 404 {
		t.Errorf("second lookup = %d, want 404", w.Code)
	}
	if upstreamHits.Load() != first {
		t.Errorf("missing image re-asked upstream (%d → %d); it must be negative-cached",
			first, upstreamHits.Load())
	}
}

// TestParseBearerChallenge covers the header shapes real registries send.
func TestParseBearerChallenge(t *testing.T) {
	for _, tc := range []struct{ in, realm, service string }{
		{`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`,
			"https://auth.docker.io/token", "registry.docker.io"},
		{`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:x:pull"`,
			"https://ghcr.io/token", "ghcr.io"},
		{`Bearer realm="https://q.io/token"`, "https://q.io/token", ""},
	} {
		ch, ok := parseBearerChallenge(tc.in)
		if !ok || ch.realm != tc.realm || ch.service != tc.service {
			t.Errorf("parseBearerChallenge(%q) = %+v, %v", tc.in, ch, ok)
		}
	}
	if _, ok := parseBearerChallenge(`Basic realm="x"`); ok {
		t.Error("Basic challenge must not parse as Bearer")
	}
}
