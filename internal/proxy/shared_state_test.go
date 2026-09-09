package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/proxy"
)

// Coalescing and circuit-breaking only mean anything ACROSS concurrent
// requests, and every format constructs a Fetcher inside its handler — one per
// request. While that state lived on the Fetcher, both features were inert:
// ten concurrent requests for one uncached artifact produced ten upstream
// fetches against a live server, and a breaker could never reach the failure
// count that opens it.
//
// These tests use a separate Fetcher per caller on purpose. That is what
// production does, and testing with one shared Fetcher would prove nothing.
func stores(t *testing.T) (blob.Store, meta.Store) {
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
	return b, m
}

func TestCoalescing_HoldsAcrossPerRequestFetchers(t *testing.T) {
	proxy.ResetBreakers()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond) // long enough for callers to pile up
		_, _ = w.Write([]byte("ARTIFACT"))
	}))
	defer up.Close()
	b, m := stores(t)

	const n = 10
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := proxy.New(up.Client(), proxy.Config{}) // a fresh Fetcher, as a handler does
			rc, _, err := f.Fetch("r/a.jar", "r:proxy", up.URL+"/a.jar", b, m)
			if err == nil {
				rc.Close() //nolint:errcheck
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != n {
		t.Errorf("%d of %d concurrent fetches succeeded", ok.Load(), n)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream was fetched %d times for one artifact; concurrent "+
			"cache misses must coalesce into a single upstream request", got)
	}
}

func TestBreaker_OpensAcrossPerRequestFetchers(t *testing.T) {
	proxy.ResetBreakers()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer up.Close()
	b, m := stores(t)

	// Each attempt is its own Fetcher, exactly as a sequence of requests would be.
	for i := 0; i < 12; i++ {
		f := proxy.New(up.Client(), proxy.Config{MaxRetries: 1})
		if rc, _, err := f.Fetch("r/b-"+string(rune('a'+i))+".jar", "r:proxy",
			up.URL+"/b.jar", b, m); err == nil {
			rc.Close() //nolint:errcheck
		}
	}
	// With a breaker that survives, later attempts fast-fail instead of dialling.
	if got := hits.Load(); got >= 24 {
		t.Errorf("upstream was contacted %d times across 12 failing requests; the "+
			"circuit breaker never opened, so it is not protecting a failing host", got)
	}
}
