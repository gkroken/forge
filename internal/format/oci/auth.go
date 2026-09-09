package oci

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"forge/internal/format"
)

// Registry v2 token authentication.
//
// Docker Hub, ghcr.io and quay.io all answer an unauthenticated request with
// 401 and a Bearer challenge naming a token endpoint. Without walking that
// handshake a proxy can only reach registries that allow anonymous access, so
// pointing one at Docker Hub — the first thing anybody tries — fails outright.

type tokenEntry struct {
	value     string // full Authorization header value, or "" for "none needed"
	expiresAt time.Time
}

// tokenCache is keyed by upstream+scope. It lives on the Handler, which the
// registry constructs once, so tokens are reused across requests rather than
// re-fetched per pull.
type tokenCache struct {
	mu sync.Mutex
	m  map[string]tokenEntry
}

func (t *tokenCache) get(key string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.m[key]
	if !ok || time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.value, true
}

func (t *tokenCache) put(key, value string, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]tokenEntry{}
	}
	t.m[key] = tokenEntry{value: value, expiresAt: time.Now().Add(ttl)}
}

// authFor returns the Authorization header to use for one image on this
// upstream, or "" when the registry needs none.
//
// The registry is asked once per (upstream, image) token lifetime via the /v2/
// ping endpoint: 200 means anonymous access works, 401 carries the challenge
// that says where to get a token. A registry needing no auth is cached as such,
// so the ping is not repeated on every pull.
func (h *Handler) authFor(c *format.Context, image string) string {
	if c.Repo.Upstream == "" {
		return ""
	}
	base := strings.TrimRight(c.Repo.Upstream, "/")
	key := base + "|" + image
	if v, ok := h.tokens.get(key); ok {
		return v
	}

	challenge, ok := h.ping(c, base)
	if !ok {
		// Anonymous access works (or the registry is unreachable, in which case
		// the real request reports it). Remember for a while either way.
		h.tokens.put(key, fallbackAuth(c), 10*time.Minute)
		return fallbackAuth(c)
	}
	value, ttl := h.requestToken(c, challenge, image)
	if value == "" {
		return fallbackAuth(c)
	}
	h.tokens.put(key, value, ttl)
	return value
}

// fallbackAuth is the configured static credential, used when the registry
// issues no challenge (or the token request fails).
func fallbackAuth(c *format.Context) string { return c.Repo.ProxyAuth }

// bearerChallenge is a parsed WWW-Authenticate: Bearer header.
type bearerChallenge struct{ realm, service string }

// ping asks the registry's /v2/ endpoint whether it requires a token.
func (h *Handler) ping(c *format.Context, base string) (bearerChallenge, bool) {
	req, err := http.NewRequest(http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return bearerChallenge{}, false
	}
	if c.Repo.ProxyAuth != "" {
		req.Header.Set("Authorization", c.Repo.ProxyAuth)
	}
	resp, err := c.HTTP.Do(req) // #nosec G704 -- admin-configured upstream
	if err != nil {
		return bearerChallenge{}, false
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusUnauthorized {
		return bearerChallenge{}, false
	}
	return parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
}

// parseBearerChallenge reads: Bearer realm="https://auth.docker.io/token",service="registry.docker.io"
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		if rest, ok = strings.CutPrefix(header, "bearer "); !ok {
			return bearerChallenge{}, false
		}
	}
	var ch bearerChallenge
	for _, part := range splitChallenge(rest) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(k) {
		case "realm":
			ch.realm = v
		case "service":
			ch.service = v
		}
	}
	return ch, ch.realm != ""
}

// splitChallenge splits on commas that are not inside quotes.
func splitChallenge(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// requestToken performs the token exchange for pull access to one image.
func (h *Handler) requestToken(c *format.Context, ch bearerChallenge, image string) (string, time.Duration) {
	q := url.Values{}
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	q.Set("scope", "repository:"+image+":pull")

	req, err := http.NewRequest(http.MethodGet, ch.realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", 0
	}
	// Configured credentials authenticate the token request itself, which is
	// how private upstream images work.
	if c.Repo.ProxyAuth != "" {
		req.Header.Set("Authorization", c.Repo.ProxyAuth)
	}
	resp, err := c.HTTP.Do(req) // #nosec G704 -- realm comes from the upstream's own challenge
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", 0
	}
	var doc struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", 0
	}
	tok := doc.Token
	if tok == "" {
		tok = doc.AccessToken
	}
	if tok == "" {
		return "", 0
	}
	ttl := time.Duration(doc.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute // registries may omit expires_in
	}
	// Renew a little early so a token does not expire mid-pull.
	if ttl > time.Minute {
		ttl -= 30 * time.Second
	}
	return "Bearer " + tok, ttl
}
