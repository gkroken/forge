package npm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/repo"
)

// A proxy has to tell "upstream says this package does not exist" apart from
// "upstream is unreachable". Collapsing them made every miss a 502 and, because
// nothing was negative-cached, re-asked the upstream registry on every single
// lookup — a typo'd CI dependency hammering npmjs forever.

func proxyCtx(t *testing.T, upstreamURL string, client *http.Client) *format.Context {
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
		Repo: repo.Repository{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, Upstream: upstreamURL},
		Blob: b, Meta: m, HTTP: client,
	}
}

func get(t *testing.T, c *format.Context, pkg string) *httptest.ResponseRecorder {
	t.Helper()
	c.Sub = pkg
	rw := httptest.NewRecorder()
	npm.New().Serve(rw, httptest.NewRequest(http.MethodGet, "/", nil), c)
	return rw
}

// TestProxy_UpstreamNotFound_Is404AndNegativeCached pins both halves: the status
// clients see, and that a second lookup never reaches upstream.
func TestProxy_UpstreamNotFound_Is404AndNegativeCached(t *testing.T) {
	var hits int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.NotFound(w, r)
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())

	if rw := get(t, c, "no-such-pkg"); rw.Code != http.StatusNotFound {
		t.Errorf("first lookup = %d, want 404 (a 502 tells npm the registry is broken)", rw.Code)
	}
	if rw := get(t, c, "no-such-pkg"); rw.Code != http.StatusNotFound {
		t.Errorf("second lookup = %d, want 404", rw.Code)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("upstream was asked %d times; the negative cache must answer the second lookup locally", n)
	}
}

// TestProxy_UpstreamDown_Is502 — the other side of the split: unreachable is not
// "not found", and must not be negative-cached as though it were.
func TestProxy_UpstreamDown_Is502(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())

	if rw := get(t, c, "some-pkg"); rw.Code != http.StatusBadGateway {
		t.Errorf("upstream 5xx = %d, want 502", rw.Code)
	}
}

// TestProxy_StaleServedWhenUpstreamDown — an unreachable upstream still serves
// the cached copy, which is the whole point of a proxy during an outage.
func TestProxy_StaleServedWhenUpstreamDown(t *testing.T) {
	var fail atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "cached-pkg",
			"versions": map[string]any{"1.0.0": map[string]any{
				"name": "cached-pkg", "version": "1.0.0",
				"dist": map[string]any{"tarball": "https://up.example.com/cached-pkg-1.0.0.tgz"}}},
		})
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())

	if rw := get(t, c, "cached-pkg"); rw.Code != http.StatusOK {
		t.Fatalf("warm the cache: %d", rw.Code)
	}
	fail.Store(true)
	rw := get(t, c, "cached-pkg")
	if rw.Code != http.StatusOK {
		t.Errorf("with upstream down = %d, want the stale cached packument", rw.Code)
	}
}

// TestProxy_UnpublishedUpstreamStopsBeingServed — a 404 is authoritative, so a
// package removed upstream stops being served even though a copy is cached.
// This matches the shared proxy.Fetcher, which does not serve stale on 404.
func TestProxy_UnpublishedUpstreamStopsBeingServed(t *testing.T) {
	var gone atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "doomed",
			"versions": map[string]any{"1.0.0": map[string]any{
				"name": "doomed", "version": "1.0.0",
				"dist": map[string]any{"tarball": "https://up.example.com/doomed-1.0.0.tgz"}}},
		})
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())

	if rw := get(t, c, "doomed"); rw.Code != http.StatusOK {
		t.Fatalf("warm the cache: %d", rw.Code)
	}
	gone.Store(true)
	// TTL is 24h by default, so force revalidation by ageing the cache entry.
	var ce map[string]any
	if ok, _ := c.Meta.GetJSON("npm-proxy:proxy", "doomed", &ce); ok {
		ce["fetchedAt"] = "2000-01-01T00:00:00Z"
		_ = c.Meta.PutJSON("npm-proxy:proxy", "doomed", ce)
	}
	if rw := get(t, c, "doomed"); rw.Code != http.StatusNotFound {
		t.Errorf("unpublished upstream = %d, want 404 — a 404 is authoritative, not an outage", rw.Code)
	}
}

// TestProxy_StalePackumentIsRevalidated — a cached packument must expire. If a
// stored copy is served unconditionally, the TTL and the conditional-GET
// machinery never run, and a proxy pins a package's version list at whatever it
// first saw: versions published upstream afterwards stay invisible forever.
func TestProxy_StalePackumentIsRevalidated(t *testing.T) {
	var hits int64
	var second atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		versions := map[string]any{"1.0.0": map[string]any{
			"name": "growing", "version": "1.0.0",
			"dist": map[string]any{"tarball": "https://up.example.com/growing-1.0.0.tgz"}}}
		if second.Load() { // a new release appears upstream
			versions["2.0.0"] = map[string]any{
				"name": "growing", "version": "2.0.0",
				"dist": map[string]any{"tarball": "https://up.example.com/growing-2.0.0.tgz"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "growing", "versions": versions})
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())

	if rw := get(t, c, "growing"); rw.Code != http.StatusOK {
		t.Fatalf("warm the cache: %d", rw.Code)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Fatalf("expected one upstream fetch, got %d", n)
	}

	// Within the TTL nothing should be re-fetched.
	get(t, c, "growing")
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Errorf("fresh cache still contacted upstream (%d hits)", n)
	}

	// Age the cache entry past its TTL, then publish upstream.
	var ce map[string]any
	if ok, _ := c.Meta.GetJSON("npm-proxy:proxy", "growing", &ce); !ok {
		t.Fatal("no cache entry recorded for the packument")
	}
	ce["fetchedAt"] = "2000-01-01T00:00:00Z"
	if err := c.Meta.PutJSON("npm-proxy:proxy", "growing", ce); err != nil {
		t.Fatal(err)
	}
	second.Store(true)

	rw := get(t, c, "growing")
	if n := atomic.LoadInt64(&hits); n < 2 {
		t.Fatalf("stale packument served without revalidating (%d upstream hits) — "+
			"the TTL is never consulted, so upstream releases stay invisible", n)
	}
	if !strings.Contains(rw.Body.String(), "2.0.0") {
		t.Errorf("revalidated packument is missing the new upstream version:\n%s", rw.Body.String())
	}
}
