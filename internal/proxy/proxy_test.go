package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// ── test helpers ─────────────────────────────────────────────────────────────

// fake is a controllable upstream HTTP server.
type fake struct {
	srv       *httptest.Server
	code      int
	body      string
	etag      string
	calls     int // number of requests received
}

func newFake(t *testing.T, code int, body string) *fake {
	t.Helper()
	f := &fake{code: code, body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		// Honour If-None-Match when an ETag is configured.
		if f.etag != "" && r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(f.code)
		io.WriteString(w, f.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newStores(t *testing.T) (blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return b, m
}

// fetchOnce is a convenience wrapper around Fetch.
func fetchOnce(t *testing.T, f *Fetcher, up *fake, b blob.Store, m meta.Store) (string, error) {
	t.Helper()
	rc, _, err := f.Fetch("key/item", "ns", up.srv.URL+"/item", b, m)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data), nil
}

// ── OnCacheFill seam ───────────────────────────────────────────────────────────

// TestOnCacheFill_FiresOnceOnFillNotOnHit verifies the OnCacheFill callback is
// invoked with the blob key exactly when a cache miss fetches+stores upstream,
// and not on a subsequent fresh hit (which must not re-emit artifact.cached).
func TestOnCacheFill_FiresOnceOnFillNotOnHit(t *testing.T) {
	up := newFake(t, 200, "payload")
	b, m := newStores(t)
	var fills []string
	f := New(http.DefaultClient, Config{
		TTL:         time.Hour,
		OnCacheFill: func(key string) { fills = append(fills, key) },
	})

	// First fetch — cache miss → fill → one callback with the blob key.
	if _, err := fetchOnce(t, f, up, b, m); err != nil {
		t.Fatal(err)
	}
	// Second fetch — fresh hit → no additional callback.
	if _, err := fetchOnce(t, f, up, b, m); err != nil {
		t.Fatal(err)
	}

	if len(fills) != 1 {
		t.Fatalf("expected exactly 1 cache-fill callback, got %d (%v)", len(fills), fills)
	}
	if fills[0] != "key/item" {
		t.Fatalf("callback key = %q, want %q", fills[0], "key/item")
	}
}

// ── TTL ──────────────────────────────────────────────────────────────────────

func TestTTL_CacheMiss_FetchesUpstream(t *testing.T) {
	up := newFake(t, 200, "hello")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if body != "hello" {
		t.Errorf("got %q, want %q", body, "hello")
	}
	if up.calls != 1 {
		t.Errorf("expected 1 upstream call, got %d", up.calls)
	}
}

func TestTTL_FreshHit_SkipsUpstream(t *testing.T) {
	up := newFake(t, 200, "original")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	// First fetch populates cache.
	fetchOnce(t, f, up, b, m)
	// Second fetch should be served from cache.
	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if body != "original" {
		t.Errorf("got %q", body)
	}
	if up.calls != 1 {
		t.Errorf("expected 1 upstream call (cache hit), got %d", up.calls)
	}
}

func TestTTL_Expiry_RefetchesUpstream(t *testing.T) {
	up := newFake(t, 200, "v2")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	// Seed a stale cache entry (fetched 25h ago).
	b.Put("key/item", strings.NewReader("v1"))
	m.PutJSON("ns", "key/item", CacheEntry{
		FetchedAt:   time.Now().Add(-25 * time.Hour),
		ContentType: "text/plain",
	})

	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2" {
		t.Errorf("expected fresh content v2, got %q", body)
	}
	if up.calls != 1 {
		t.Errorf("expected 1 upstream call on expiry, got %d", up.calls)
	}
}

// ── ETag revalidation ────────────────────────────────────────────────────────

func TestETag_304_RefreshesMetaServesFromCache(t *testing.T) {
	up := newFake(t, 200, "content")
	up.etag = `"abc123"`
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	// First fetch — cache miss, upstream returns 200 + ETag.
	fetchOnce(t, f, up, b, m)
	firstCalls := up.calls

	// Expire the TTL so the second fetch triggers revalidation.
	var entry CacheEntry
	m.GetJSON("ns", "key/item", &entry)
	entry.FetchedAt = time.Now().Add(-25 * time.Hour)
	m.PutJSON("ns", "key/item", entry)

	// Second fetch — stale + ETag → sends If-None-Match → 304.
	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if body != "content" {
		t.Errorf("got %q", body)
	}
	if up.calls != firstCalls+1 {
		t.Errorf("expected one extra upstream call for revalidation, got %d total", up.calls)
	}

	// Third fetch — TTL was refreshed by 304 → should be served from cache.
	body, err = fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if up.calls != firstCalls+1 {
		t.Errorf("expected no extra upstream call after TTL refresh, got %d total", up.calls)
	}
	_ = body
}

func TestETag_200_UpdatesBlob(t *testing.T) {
	up := newFake(t, 200, "v1")
	up.etag = `"etag1"`
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	fetchOnce(t, f, up, b, m)

	// Expire + change upstream content.
	var entry CacheEntry
	m.GetJSON("ns", "key/item", &entry)
	entry.FetchedAt = time.Now().Add(-25 * time.Hour)
	entry.ETag = `"etag1"`
	m.PutJSON("ns", "key/item", entry)
	up.body = "v2"
	up.etag = `"etag2"` // different ETag → 200

	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2" {
		t.Errorf("expected updated content v2, got %q", body)
	}
}

// ── Negative caching ─────────────────────────────────────────────────────────

func TestNegativeCache_404_StopsRetries(t *testing.T) {
	up := newFake(t, 404, "not found")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{NegativeTTL: time.Hour})

	// First fetch: upstream 404 → ErrNotFound, entry cached.
	_, err := fetchOnce(t, f, up, b, m)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Second fetch: negative cache hit → ErrNotFound without hitting upstream.
	_, err = fetchOnce(t, f, up, b, m)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound on negative cache hit, got %v", err)
	}
	if up.calls != 1 {
		t.Errorf("expected 1 upstream call (negative cache suppresses second), got %d", up.calls)
	}
}

func TestNegativeCache_Expires_Retries(t *testing.T) {
	up := newFake(t, 200, "now exists")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{NegativeTTL: 5 * time.Minute})

	// Seed an expired negative cache entry.
	m.PutJSON("ns", "key/item", CacheEntry{
		FetchedAt: time.Now().Add(-10 * time.Minute),
		NotFound:  true,
	})

	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatalf("expected success after negative TTL expiry, got %v", err)
	}
	if body != "now exists" {
		t.Errorf("got %q", body)
	}
}

// ── Stale-on-error ───────────────────────────────────────────────────────────

func TestStaleOnError_5xx_ServesStaleContent(t *testing.T) {
	up := newFake(t, 500, "error")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{
		TTL:        time.Hour,
		MaxRetries: 0, // no retries so the test is fast
	})

	// Seed a stale blob + meta.
	b.Put("key/item", strings.NewReader("stale content"))
	m.PutJSON("ns", "key/item", CacheEntry{
		FetchedAt:   time.Now().Add(-25 * time.Hour),
		ContentType: "text/plain",
	})

	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatalf("expected stale content on upstream error, got error: %v", err)
	}
	if body != "stale content" {
		t.Errorf("expected stale content, got %q", body)
	}
}

func TestStaleOnError_Disabled_ReturnsError(t *testing.T) {
	up := newFake(t, 500, "error")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{
		MaxRetries:          0,
		DisableStaleOnError: true,
	})

	b.Put("key/item", strings.NewReader("stale"))
	m.PutJSON("ns", "key/item", CacheEntry{
		FetchedAt:   time.Now().Add(-25 * time.Hour),
		ContentType: "text/plain",
	})

	_, err := fetchOnce(t, f, up, b, m)
	if err == nil {
		t.Fatal("expected error when DisableStaleOnError is true")
	}
}

// ── Retries ──────────────────────────────────────────────────────────────────

func TestRetry_5xx_RetriesAndSucceeds(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt < 3 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "success")
	}))
	defer srv.Close()

	b, m := newStores(t)
	f := New(&http.Client{}, Config{MaxRetries: 3})
	// Override sleep for speed.
	f.now = time.Now

	rc, _, err := f.Fetch("k", "ns", srv.URL+"/k", b, m)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "success" {
		t.Errorf("got %q", data)
	}
	if attempt != 3 {
		t.Errorf("expected 3 attempts, got %d", attempt)
	}
}

func TestRetry_ExhaustedWithNoCache_ReturnsError(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 1, DisableStaleOnError: true})

	_, err := fetchOnce(t, f, up, b, m)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if up.calls != 2 { // 1 initial + 1 retry
		t.Errorf("expected 2 calls (1+1 retry), got %d", up.calls)
	}
}

// ── Upstream auth ────────────────────────────────────────────────────────────

func TestUpstreamAuth_HeaderForwarded(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "secret content")
	}))
	defer srv.Close()

	b, m := newStores(t)
	f := New(http.DefaultClient, Config{Auth: "Bearer mytoken123"})

	rc, _, err := f.Fetch("k", "ns", srv.URL+"/k", b, m)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	if receivedAuth != "Bearer mytoken123" {
		t.Errorf("expected Authorization: Bearer mytoken123, got %q", receivedAuth)
	}
}

// ── Error sentinel values ────────────────────────────────────────────────────

func TestErrNotFound_IsDistinctFromErrUpstreamFailed(t *testing.T) {
	if ErrNotFound == ErrUpstreamFailed {
		t.Error("sentinel errors must be distinct")
	}
}

// ── Request coalescing (single-flight) ───────────────────────────────────────

// TestSingleFlight_CoalescesConcurrentMisses fires N simultaneous cache misses
// for the same key and asserts the upstream is contacted exactly once, while
// every caller still receives the body.
func TestSingleFlight_CoalescesConcurrentMisses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(75 * time.Millisecond) // upstream latency widens the herd window
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "payload")
	}))
	defer srv.Close()

	b, m := newStores(t)
	f := New(http.DefaultClient, Config{TTL: time.Hour})

	const N = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	bodies := make([]string, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together
			rc, _, err := f.Fetch("k/coalesce", "ns", srv.URL+"/item", b, m)
			if err != nil {
				errs[i] = err
				return
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			bodies[i] = string(data)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 upstream call (coalesced), got %d", got)
	}
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error %v", i, errs[i])
		}
		if bodies[i] != "payload" {
			t.Errorf("goroutine %d: got %q, want %q", i, bodies[i], "payload")
		}
	}
}

// ── Circuit breaker ──────────────────────────────────────────────────────────

// driveFailures exhausts the circuit breaker threshold by making failing
// requests. MaxRetries=0 keeps the test fast (no backoff sleeps).
func driveFailures(t *testing.T, f *Fetcher, up *fake, b blob.Store, m meta.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		fetchOnce(t, f, up, b, m) //nolint:errcheck
	}
}

func TestCircuitBreaker_Opens_AfterThreshold(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0, DisableStaleOnError: true})

	// cbFailureThreshold consecutive failures should open the circuit.
	driveFailures(t, f, up, b, m, cbFailureThreshold)

	before := up.calls
	_, err := fetchOnce(t, f, up, b, m)
	if err == nil {
		t.Fatal("expected error when circuit is open")
	}
	// No additional upstream calls after the circuit opens.
	if up.calls != before {
		t.Errorf("circuit open: expected no upstream call, got %d extra", up.calls-before)
	}
}

func TestCircuitBreaker_FastFail_ServesStale(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0})

	// Pre-seed stale cache.
	b.Put("key/item", strings.NewReader("stale data"))
	m.PutJSON("ns", "key/item", CacheEntry{
		FetchedAt:   time.Now().Add(-48 * time.Hour),
		ContentType: "text/plain",
	})

	driveFailures(t, f, up, b, m, cbFailureThreshold)

	before := up.calls
	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatalf("expected stale content from open circuit, got: %v", err)
	}
	if body != "stale data" {
		t.Errorf("expected stale data, got %q", body)
	}
	if up.calls != before {
		t.Errorf("circuit open: expected no upstream call, got %d extra", up.calls-before)
	}
}

func TestCircuitBreaker_HalfOpen_AllowsProbeAfterTimeout(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0, DisableStaleOnError: true})

	// Open the circuit.
	driveFailures(t, f, up, b, m, cbFailureThreshold)

	// Advance time past cbOpenTimeout so the probe is allowed.
	fakeNow := time.Now().Add(cbOpenTimeout + time.Second)
	f.now = func() time.Time { return fakeNow }

	// Switch upstream to healthy.
	up.code = 200
	up.body = "back online"

	before := up.calls
	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatalf("expected probe to succeed: %v", err)
	}
	if body != "back online" {
		t.Errorf("got %q", body)
	}
	if up.calls != before+1 {
		t.Errorf("expected exactly one probe request, got %d extra", up.calls-before)
	}
}

func TestCircuitBreaker_Closes_AfterSuccessfulProbe(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0, DisableStaleOnError: true})

	driveFailures(t, f, up, b, m, cbFailureThreshold)

	// Advance time past cbOpenTimeout so the probe is allowed.
	probeTime := time.Now().Add(cbOpenTimeout + time.Second)
	f.now = func() time.Time { return probeTime }
	up.code = 200
	up.body = "ok"

	// Probe succeeds → circuit should close.
	fetchOnce(t, f, up, b, m) //nolint:errcheck

	// Advance past TTL so the probe's cache entry is stale; this forces the
	// final fetch through the circuit-breaker path rather than a cache hit.
	f.now = func() time.Time { return probeTime.Add(DefaultTTL + time.Second) }
	before := up.calls
	body, err := fetchOnce(t, f, up, b, m)
	if err != nil {
		t.Fatalf("expected circuit closed after successful probe: %v", err)
	}
	if body != "ok" {
		t.Errorf("got %q", body)
	}
	// Should have hit upstream again (circuit closed = not fast-failing).
	if up.calls == before {
		t.Error("expected upstream call after circuit closed")
	}
}

func TestCircuitBreaker_ReopensAfterFailedProbe(t *testing.T) {
	up := newFake(t, 503, "still down")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0, DisableStaleOnError: true})

	driveFailures(t, f, up, b, m, cbFailureThreshold)

	// Advance past timeout → half-open probe, but upstream still failing.
	f.now = func() time.Time { return time.Now().Add(cbOpenTimeout + time.Second) }
	fetchOnce(t, f, up, b, m) //nolint:errcheck — probe fails

	// Circuit should be open again; next request fast-fails without extra call.
	before := up.calls
	_, err := fetchOnce(t, f, up, b, m)
	if err == nil {
		t.Fatal("expected error: circuit should re-open after failed probe")
	}
	if up.calls != before {
		t.Errorf("expected no upstream call (re-opened), got %d extra", up.calls-before)
	}
}

// ── ConfigForRepo: repo settings → proxy Config wiring ─────────────────────────

func TestConfigForRepo(t *testing.T) {
	dur := func(d time.Duration) *time.Duration { return &d }
	iptr := func(i int) *int { return &i }
	bptr := func(b bool) *bool { return &b }

	t.Run("defaults when nothing set", func(t *testing.T) {
		cfg := ConfigForRepo(repo.Repository{})
		if cfg.TTL != DefaultTTL {
			t.Errorf("TTL = %v, want default %v", cfg.TTL, DefaultTTL)
		}
		if cfg.DisableNegativeCache {
			t.Error("negative cache should be enabled by default")
		}
		if cfg.MaxRetries != 0 || cfg.Timeout != 0 || cfg.Auth != "" {
			t.Errorf("unexpected non-zero defaults: %+v", cfg)
		}
	})

	t.Run("ContentMaxAge wins over ProxyTTL", func(t *testing.T) {
		cfg := ConfigForRepo(repo.Repository{
			ContentMaxAge: dur(2 * time.Hour),
			ProxyTTL:      5 * time.Minute,
		})
		if cfg.TTL != 2*time.Hour {
			t.Errorf("TTL = %v, want 2h (ContentMaxAge)", cfg.TTL)
		}
	})

	t.Run("legacy ProxyTTL used when no ContentMaxAge", func(t *testing.T) {
		cfg := ConfigForRepo(repo.Repository{ProxyTTL: 90 * time.Second})
		if cfg.TTL != 90*time.Second {
			t.Errorf("TTL = %v, want 90s (ProxyTTL)", cfg.TTL)
		}
	})

	t.Run("negative cache disabled, retries, timeout, auth", func(t *testing.T) {
		cfg := ConfigForRepo(repo.Repository{
			ProxyAuth:     "Bearer xyz",
			NegativeCache: bptr(false),
			Retries:       iptr(4),
			TimeoutSecs:   iptr(12),
		})
		if !cfg.DisableNegativeCache {
			t.Error("NegativeCache=false should set DisableNegativeCache")
		}
		if cfg.MaxRetries != 4 {
			t.Errorf("MaxRetries = %d, want 4", cfg.MaxRetries)
		}
		if cfg.Timeout != 12*time.Second {
			t.Errorf("Timeout = %v, want 12s", cfg.Timeout)
		}
		if cfg.Auth != "Bearer xyz" {
			t.Errorf("Auth = %q, want carried through", cfg.Auth)
		}
	})

	t.Run("NegativeCache=true leaves default enabled; zero TimeoutSecs ignored", func(t *testing.T) {
		cfg := ConfigForRepo(repo.Repository{
			NegativeCache: bptr(true),
			TimeoutSecs:   iptr(0),
		})
		if cfg.DisableNegativeCache {
			t.Error("NegativeCache=true must not disable negative cache")
		}
		if cfg.Timeout != 0 {
			t.Errorf("TimeoutSecs=0 should be ignored, got %v", cfg.Timeout)
		}
	})
}

// ── Health registry: HealthOf / AllHealth reflect breaker state ────────────────

func TestHealthOf_UnknownHostIsOK(t *testing.T) {
	if got := HealthOf("https://never-contacted.example.invalid"); got != "ok" {
		t.Errorf("HealthOf(unknown) = %q, want ok", got)
	}
}

func TestHealthOf_And_AllHealth_ReflectOpenBreaker(t *testing.T) {
	up := newFake(t, 503, "overload")
	b, m := newStores(t)
	f := New(http.DefaultClient, Config{MaxRetries: 0, DisableStaleOnError: true})

	// Drive the breaker for this upstream host open.
	driveFailures(t, f, up, b, m, cbFailureThreshold)

	host := upstreamHost(up.srv.URL)
	if got := HealthOf(up.srv.URL); got != "down" {
		t.Errorf("HealthOf(%s) = %q, want down after breaker opened", up.srv.URL, got)
	}

	all := AllHealth()
	if all[host] != "down" {
		t.Errorf("AllHealth()[%s] = %q, want down; full map: %v", host, all[host], all)
	}

	// A successful fetch to the same host should flip health back to ok.
	up.code = 200
	up.body = "recovered"
	probe := time.Now().Add(cbOpenTimeout + time.Second)
	f.now = func() time.Time { return probe }
	fetchOnce(t, f, up, b, m) //nolint:errcheck
	if got := HealthOf(up.srv.URL); got != "ok" {
		t.Errorf("HealthOf after recovery = %q, want ok", got)
	}
}

func TestUpstreamHost(t *testing.T) {
	cases := map[string]string{
		"https://charts.bitnami.com/bitnami/index.yaml": "https://charts.bitnami.com",
		"http://localhost:8080/v2/foo":                  "http://localhost:8080",
		"::not a url":                                   "::not a url", // parse failure → raw
	}
	for in, want := range cases {
		if got := upstreamHost(in); got != want {
			t.Errorf("upstreamHost(%q) = %q, want %q", in, got, want)
		}
	}
}
