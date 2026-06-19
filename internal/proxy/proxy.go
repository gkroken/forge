// Package proxy implements cache-on-read semantics for forge's proxy repos.
//
// All four format handlers share this Fetcher so proxy behaviour is consistent
// and tested in one place rather than duplicated.
//
// Features
//
//   - TTL-based freshness: a cached blob is served without contacting upstream
//     until its TTL expires.
//   - ETag / Last-Modified revalidation (RFC 7232): when a blob is stale but
//     carries an ETag or Last-Modified value, a conditional GET is sent.
//     A 304 Not Modified refreshes the TTL without re-downloading the body.
//   - Negative caching: a 404 from upstream is cached for NegativeTTL so the
//     same missing artifact doesn't hammer the upstream registry.
//   - Stale-on-error: when upstream returns 5xx or is unreachable, the cached
//     (even if stale) blob is served so clients are not broken by transient
//     upstream failures. Disable with Config.DisableStaleOnError.
//   - Retries: transient network errors and 5xx responses are retried up to
//     MaxRetries times with exponential back-off.
//   - Upstream auth: an Authorization header is forwarded on every upstream
//     request when Config.Auth is set.
//   - Circuit breaker: after cbFailureThreshold consecutive upstream failures
//     the circuit opens and requests fast-fail (serving stale if available)
//     for cbOpenTimeout. One probe is then allowed; success closes the circuit,
//     failure keeps it open.
//
// Cache storage
//
//   - Blob bytes  → blob.Store at blobKey
//   - CacheEntry  → meta.Store at (cacheNS, blobKey)
//
// Callers choose the namespace; the convention used by the format handlers is
// "{repo-name}:proxy".
//
// Lifecycle
//
// Fetcher must be long-lived (one per proxy repository, stored on the Server)
// so the circuit-breaker state persists across requests. Creating a fresh
// Fetcher per request loses the circuit state and defeats the breaker.
package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
)

// Sentinel errors returned by Fetcher.Fetch.
var (
	// ErrNotFound is returned when the upstream returned 404 (or a recent
	// negative-cache entry exists).
	ErrNotFound = errors.New("proxy: not found")

	// ErrUpstreamFailed is returned when the upstream is unreachable or returns
	// a non-OK status and no cached content is available to serve.
	ErrUpstreamFailed = errors.New("proxy: upstream unavailable")
)

// HealthKey is the meta key under which the upstream health record is stored
// in the cacheNS namespace (alongside individual CacheEntry records).
const HealthKey = "__health"

// HealthRecord captures the result of the most recent upstream fetch attempt.
// Stored in meta at (cacheNS, HealthKey).
type HealthRecord struct {
	OK        bool      `json:"ok"`
	CheckedAt time.Time `json:"checkedAt"`
	ErrMsg    string    `json:"err,omitempty"`
}

// CacheEntry records the provenance of a cached blob.
// Stored in meta at (cacheNS, blobKey).
type CacheEntry struct {
	FetchedAt    time.Time `json:"fetchedAt"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"lastModified,omitempty"`
	NotFound     bool      `json:"notFound,omitempty"` // negative-cache flag
	ContentType  string    `json:"contentType,omitempty"`
}

// Config controls caching and upstream-fetch behaviour.
// Zero values are replaced with the package defaults.
type Config struct {
	// TTL is how long a cached blob is considered fresh before re-validation.
	TTL time.Duration

	// NegativeTTL is how long a 404 response is suppressed before retry.
	NegativeTTL time.Duration

	// Auth is an Authorization header value forwarded to every upstream request,
	// e.g. "Basic dXNlcjpwYXNz" or "Bearer mytoken".
	Auth string

	// MaxRetries is the number of additional attempts after a transient failure.
	MaxRetries int

	// DisableStaleOnError prevents serving stale cached content when upstream
	// fails. Default (false) = stale IS served on error.
	DisableStaleOnError bool

	// RecordHit is called when a request is served from the local cache
	// (TTL-fresh or ETag-revalidated). May be nil.
	RecordHit func()

	// RecordMiss is called when a full upstream 200 OK fetch was required.
	// May be nil.
	RecordMiss func()
}

const (
	DefaultTTL         = 24 * time.Hour
	DefaultNegativeTTL = 5 * time.Minute
	DefaultMaxRetries  = 2
)

func (c Config) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

func (c Config) negativeTTL() time.Duration {
	if c.NegativeTTL > 0 {
		return c.NegativeTTL
	}
	return DefaultNegativeTTL
}

func (c Config) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return DefaultMaxRetries
}

// ── Circuit breaker ──────────────────────────────────────────────────────────

const (
	cbFailureThreshold = 5                // consecutive upstream failures to open
	cbOpenTimeout      = 30 * time.Second // time before allowing a half-open probe
)

type cbState uint8

const (
	cbClosed   cbState = iota // normal — requests pass through
	cbOpen                    // tripped — fast-fail, serve stale
	cbHalfOpen                // probing — one request allowed through
)

// breaker is a per-upstream-host circuit breaker.
type breaker struct {
	mu          sync.Mutex
	host        string // "scheme://host" — used to update globalHealth
	state       cbState
	failures    int
	lastFailure time.Time
}

// allow reports whether a request to this upstream should be attempted.
// Transitions Open→HalfOpen after cbOpenTimeout has elapsed.
func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case cbClosed:
		return true
	case cbOpen:
		if now.Sub(b.lastFailure) >= cbOpenTimeout {
			b.state = cbHalfOpen
			return true // one probe
		}
		return false
	case cbHalfOpen:
		return true // the probe
	}
	return true
}

// success resets the breaker to Closed.
func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = cbClosed
	if b.host != "" {
		globalHealth.Store(b.host, "ok")
	}
}

// failure increments the failure count; opens the circuit when the threshold
// is reached, or keeps it open after a failed half-open probe.
func (b *breaker) failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.lastFailure = now
	if b.state == cbHalfOpen || b.failures >= cbFailureThreshold {
		b.state = cbOpen
		if b.host != "" {
			globalHealth.Store(b.host, "down")
		}
	}
}

// ── Fetcher ──────────────────────────────────────────────────────────────────

// Fetcher performs cache-on-read proxy fetches with a per-upstream circuit breaker.
// It must be long-lived — create one per proxy repository on server start-up.
type Fetcher struct {
	client   *http.Client
	cfg      Config
	now      func() time.Time // injectable for deterministic testing

	mu       sync.Mutex
	breakers map[string]*breaker // keyed by "scheme://host"
}

// New returns a Fetcher backed by client with the given config.
func New(client *http.Client, cfg Config) *Fetcher {
	return &Fetcher{
		client:   client,
		cfg:      cfg,
		now:      time.Now,
		breakers: make(map[string]*breaker),
	}
}

func (f *Fetcher) getBreaker(host string) *breaker {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.breakers[host]
	if !ok {
		b = &breaker{host: host}
		f.breakers[host] = b
	}
	return b
}

// upstreamHost extracts the scheme+host from a URL for use as a breaker key.
func upstreamHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

// ── Health registry ──────────────────────────────────────────────────────────
//
// globalHealth persists circuit-breaker state across per-request Fetchers.
// Keys are "scheme://host"; values are one of "ok", "degraded", "down".
// Written by breaker state transitions; read by Server for the health dot.
var globalHealth sync.Map

// HealthOf returns the most recently observed health state for the upstream at
// rawURL. Returns "ok" if no CB trip has been recorded for this host.
func HealthOf(rawURL string) string {
	if v, ok := globalHealth.Load(upstreamHost(rawURL)); ok {
		return v.(string)
	}
	return "ok"
}

// Fetch returns the content for blobKey, fetching from upURL when needed.
//
// Caching contract:
//
//	blob data   → blobs.Put/Get(blobKey)
//	cache state → metas.PutJSON/GetJSON(cacheNS, blobKey, &CacheEntry{})
//
// Return values:
//
//	(rc, ct, nil)              — success; caller must Close rc
//	(nil, "", ErrNotFound)     — upstream 404 or active negative cache
//	(nil, "", ErrUpstreamFailed) — upstream down and no cached copy
func (f *Fetcher) Fetch(blobKey, cacheNS, upURL string, blobs blob.Store, metas meta.Store) (io.ReadCloser, string, error) {
	now := f.now()

	var entry CacheEntry
	hasMeta, _ := metas.GetJSON(cacheNS, blobKey, &entry)
	_, blobExists, _ := blobs.Stat(blobKey)

	// ── 1. Negative cache ──────────────────────────────────────────────────
	if hasMeta && entry.NotFound && now.Sub(entry.FetchedAt) < f.cfg.negativeTTL() {
		return nil, "", ErrNotFound
	}

	// ── 2. Fresh cache hit ─────────────────────────────────────────────────
	if blobExists && hasMeta && !entry.NotFound && now.Sub(entry.FetchedAt) < f.cfg.ttl() {
		if rc, err := blobs.Get(blobKey); err == nil {
			if f.cfg.RecordHit != nil {
				f.cfg.RecordHit()
			}
			// Populate health record on first cache hit so the UI can show a
			// green dot even when no upstream contact has happened this session.
			var existing HealthRecord
			if ok, _ := metas.GetJSON(cacheNS, HealthKey, &existing); !ok {
				metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: true, CheckedAt: entry.FetchedAt}) //nolint:errcheck
			}
			return rc, entry.ContentType, nil
		}
	}

	// ── 3. Circuit breaker check ───────────────────────────────────────────
	host := upstreamHost(upURL)
	cb := f.getBreaker(host)
	if !cb.allow(now) {
		if !f.cfg.DisableStaleOnError && blobExists {
			if rc, err := blobs.Get(blobKey); err == nil {
				return rc, entry.ContentType, nil
			}
		}
		return nil, "", fmt.Errorf("%w: circuit open for %s", ErrUpstreamFailed, host)
	}

	// ── 4. Build conditional request headers ───────────────────────────────
	condHeaders := map[string]string{}
	if blobExists && hasMeta && !entry.NotFound {
		if entry.ETag != "" {
			condHeaders["If-None-Match"] = entry.ETag
		} else if entry.LastModified != "" {
			condHeaders["If-Modified-Since"] = entry.LastModified
		}
	}

	// ── 5. Upstream fetch (with retries and optional auth) ─────────────────
	upResp, fetchErr := f.fetchUpstream(upURL, condHeaders)
	if fetchErr != nil {
		cb.failure(now)
		metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: false, CheckedAt: now, ErrMsg: fetchErr.Error()}) //nolint:errcheck
		if !f.cfg.DisableStaleOnError && blobExists {
			if rc, err := blobs.Get(blobKey); err == nil {
				return rc, entry.ContentType, nil
			}
		}
		return nil, "", fmt.Errorf("%w: %v", ErrUpstreamFailed, fetchErr)
	}
	cb.success()

	// ── 6. Handle upstream response ────────────────────────────────────────
	switch upResp.statusCode {
	case http.StatusNotModified:
		entry.FetchedAt = now
		metas.PutJSON(cacheNS, blobKey, entry)
		metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: true, CheckedAt: now}) //nolint:errcheck
		if rc, err := blobs.Get(blobKey); err == nil {
			if f.cfg.RecordHit != nil {
				f.cfg.RecordHit()
			}
			return rc, entry.ContentType, nil
		}
		return nil, "", fmt.Errorf("%w: blob disappeared after 304", ErrUpstreamFailed)

	case http.StatusOK:
		ct := upResp.contentType
		newEntry := CacheEntry{
			FetchedAt:    now,
			ETag:         upResp.etag,
			LastModified: upResp.lastMod,
			ContentType:  ct,
		}
		blobs.Put(blobKey, bytes.NewReader(upResp.body))
		metas.PutJSON(cacheNS, blobKey, newEntry)
		metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: true, CheckedAt: now}) //nolint:errcheck
		if f.cfg.RecordMiss != nil {
			f.cfg.RecordMiss()
		}
		return io.NopCloser(bytes.NewReader(upResp.body)), ct, nil

	case http.StatusNotFound:
		metas.PutJSON(cacheNS, blobKey, CacheEntry{FetchedAt: now, NotFound: true})
		metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: false, CheckedAt: now, ErrMsg: "upstream returned 404"}) //nolint:errcheck
		return nil, "", ErrNotFound

	default:
		errMsg := fmt.Sprintf("upstream returned %d", upResp.statusCode)
		metas.PutJSON(cacheNS, HealthKey, HealthRecord{OK: false, CheckedAt: now, ErrMsg: errMsg}) //nolint:errcheck
		if !f.cfg.DisableStaleOnError && blobExists {
			if rc, err := blobs.Get(blobKey); err == nil {
				return rc, entry.ContentType, nil
			}
		}
		return nil, "", fmt.Errorf("%w: %s", ErrUpstreamFailed, errMsg)
	}
}

// upstreamResult holds the parsed outcome of one upstream HTTP request.
type upstreamResult struct {
	statusCode  int
	etag        string
	lastMod     string
	contentType string
	body        []byte
}

// fetchUpstream performs the upstream GET, retrying on network errors and 5xx.
func (f *Fetcher) fetchUpstream(upURL string, condHeaders map[string]string) (*upstreamResult, error) {
	var lastErr error
	for attempt := 0; attempt <= f.cfg.maxRetries(); attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond)
		}
		result, err := f.doRequest(upURL, condHeaders)
		if err != nil {
			lastErr = err
			continue
		}
		if result.statusCode >= 500 {
			lastErr = fmt.Errorf("upstream returned %d", result.statusCode)
			continue
		}
		return result, nil
	}
	return nil, lastErr
}

func (f *Fetcher) doRequest(upURL string, condHeaders map[string]string) (*upstreamResult, error) {
	req, err := http.NewRequest(http.MethodGet, upURL, nil)
	if err != nil {
		return nil, err
	}
	if f.cfg.Auth != "" {
		req.Header.Set("Authorization", f.cfg.Auth)
	}
	for k, v := range condHeaders {
		req.Header.Set(k, v)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &upstreamResult{statusCode: http.StatusNotModified}, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &upstreamResult{
		statusCode:  resp.StatusCode,
		etag:        resp.Header.Get("ETag"),
		lastMod:     resp.Header.Get("Last-Modified"),
		contentType: resp.Header.Get("Content-Type"),
		body:        body,
	}, nil
}
