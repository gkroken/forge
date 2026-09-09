package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"forge/internal/format"
	"forge/internal/proxy"
)

// upstreamURL builds the registry v2 URL for one operation against this repo's
// configured upstream.
func upstreamURL(c *format.Context, image, op, ref string) (string, bool) {
	var p string
	switch op {
	case "manifests":
		p = "/" + image + "/manifests/" + ref
	case "blobs":
		p = "/" + image + "/blobs/" + ref
	case "tags/list":
		p = "/" + image + "/tags/list"
	case "_catalog":
		p = "/_catalog"
	default:
		return "", false
	}
	return strings.TrimRight(c.Repo.Upstream, "/") + "/v2" + p, true
}

// upstreamGet performs one GET against the upstream registry. Every upstream
// read goes through here so authentication and caching have a single place to
// live. The caller closes the body.
func (h *Handler) upstreamGet(c *format.Context, image, upURL, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, upURL, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return h.doUpstream(c, image, req)
}

// upstreamTags reads a proxy member's tag list from upstream. A proxy's local
// cache holds only what has been pulled, so a group built from it would hide
// tags the group can actually serve.
func (h *Handler) upstreamTags(c *format.Context, image string) []string {
	upURL, ok := upstreamURL(c, image, "tags/list", "")
	if !ok || c.Repo.Upstream == "" {
		return nil
	}
	resp, err := h.upstreamGet(c, image, upURL, "application/json")
	if err != nil {
		return nil // an unreachable member must not fail the whole group
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil {
		return nil
	}
	return doc.Tags
}

// doUpstream sends one prepared upstream request, with whatever authorization
// this registry requires (see auth.go).
func (h *Handler) doUpstream(c *format.Context, image string, req *http.Request) (*http.Response, error) {
	if auth := h.authFor(c, image); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return c.HTTP.Do(req) // #nosec G704 -- URL built from the admin-configured upstream, not user input
}

// --- caching proxy reads ----------------------------------------------------
//
// Every proxy read goes through the shared proxy.Fetcher, which brings TTL
// handling, ETag revalidation, negative caching, stale-on-error, request
// coalescing and the circuit breaker. Before this, proxyPass streamed upstream
// straight to the client and stored nothing: a "proxy" that gave no rate-limit
// relief and no offline resilience, which is the whole reason to run one in
// front of Docker Hub.
//
// What may be cached forever and what may not is the crux, and getting it
// wrong reproduces the npm packument bug (a tag pinned to whatever was first
// pulled):
//
//	blobs/{digest}      immutable — the digest IS the content
//	manifests/{digest}  immutable
//	manifests/{tag}     MUTABLE — TTL + revalidation
//	tags/list           MUTABLE — TTL + revalidation

// isDigest reports whether a reference addresses content rather than a name.
func isDigest(ref string) bool { return strings.HasPrefix(ref, "sha256:") }

// proxyCacheKey picks the blob-store key for one proxy read.
//
// Tag-addressed manifests include a hash of the Accept header: the header
// selects which manifest kind the registry returns (an image manifest or a
// multi-platform index), so one key per tag would serve one client the media
// type another asked for.
func (h *Handler) proxyCacheKey(c *format.Context, image, op, ref, accept string) string {
	switch op {
	case "blobs":
		return h.blobKey(c, ref)
	case "manifests":
		if isDigest(ref) {
			return h.manifestKey(c, ref)
		}
		sum := sha256.Sum256([]byte(accept))
		return c.Key("proxy-manifests/" + image + "/" + ref + "__" + hex.EncodeToString(sum[:4]))
	case "tags/list":
		return c.Key("proxy-tags/" + image)
	case "_catalog":
		return c.Key("proxy-catalog")
	}
	return c.Key(op + "/" + image + "/" + ref)
}

// proxyFetchConfig builds the fetcher config for one read: metadata reads get
// MetadataMaxAge when the repo sets it, so a tag can be refreshed more often
// than content.
func (h *Handler) proxyFetchConfig(c *format.Context, image, accept string, mutable bool) proxy.Config {
	cfg := c.ProxyConfig()
	if mutable {
		cfg = c.ProxyMetadataConfig()
	}
	if auth := h.authFor(c, image); auth != "" {
		cfg.Auth = auth
	}
	if accept != "" {
		cfg.Headers = map[string]string{"Accept": accept}
	}
	return cfg
}

// serveProxyRead answers one GET/HEAD from cache, fetching and caching on miss.
func (h *Handler) serveProxyRead(w http.ResponseWriter, r *http.Request, c *format.Context, image, op, ref string) {
	upURL, ok := upstreamURL(c, image, op, ref)
	if !ok {
		ociError(w, "UNSUPPORTED", "unsupported proxy operation", http.StatusNotFound)
		return
	}
	accept := r.Header.Get("Accept")
	key := h.proxyCacheKey(c, image, op, ref, accept)
	immutable := op == "blobs" || (op == "manifests" && isDigest(ref))

	// Immutable content never changes, so a local copy is the final answer and
	// upstream is not consulted at all.
	if immutable {
		if info, exists, _ := c.Blob.Stat(key); exists {
			h.writeCached(w, r, c, key, op, ref, info.Size)
			return
		}
	}

	cfg := h.proxyFetchConfig(c, image, accept, !immutable)
	f := proxy.New(c.HTTP, cfg)
	rc, ct, err := f.Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if errors.Is(err, proxy.ErrNotFound) {
		if op == "blobs" {
			ociError(w, "BLOB_UNKNOWN", "blob not found upstream", http.StatusNotFound)
		} else {
			ociError(w, "MANIFEST_UNKNOWN", "not found upstream", http.StatusNotFound)
		}
		return
	}
	if err != nil {
		ociError(w, "UNSUPPORTED", "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(rc)
	if err != nil {
		ociError(w, "UNSUPPORTED", "upstream read failed", http.StatusBadGateway)
		return
	}
	h.writeProxyResponse(w, r, op, ref, ct, body)
	if c.OnCacheFill != nil {
		c.OnCacheFill(key)
	}
}

// writeCached serves an immutable object already in the blob store.
func (h *Handler) writeCached(w http.ResponseWriter, r *http.Request, c *format.Context, key, op, ref string, size int64) {
	rc, err := c.Blob.Get(key)
	if err != nil {
		ociError(w, "BLOB_UNKNOWN", "cached object unreadable", http.StatusNotFound)
		return
	}
	defer rc.Close() //nolint:errcheck
	var ct string
	var ce proxy.CacheEntry
	if ok, _ := c.Meta.GetJSON(c.Repo.Name+":proxy", key, &ce); ok {
		ct = ce.ContentType
	}
	if op == "blobs" {
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Docker-Content-Digest", ref)
		if size > 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		}
		if r.Method == http.MethodHead {
			return
		}
		io.Copy(w, rc) //nolint:errcheck
		return
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		ociError(w, "MANIFEST_UNKNOWN", "cached manifest unreadable", http.StatusNotFound)
		return
	}
	h.writeProxyResponse(w, r, op, ref, ct, body)
}

// writeProxyResponse sends the bytes with the headers an OCI client expects.
func (h *Handler) writeProxyResponse(w http.ResponseWriter, r *http.Request, op, ref, ct string, body []byte) {
	if ct == "" {
		if op == "manifests" {
			ct = "application/vnd.oci.image.manifest.v1+json"
		} else {
			ct = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", ct)
	// Clients verify this against what they asked for; for a manifest it is the
	// digest of the bytes, which is exactly what we hold.
	switch {
	case op == "manifests":
		w.Header().Set("Docker-Content-Digest", computeDigest(body))
	case isDigest(ref):
		w.Header().Set("Docker-Content-Digest", ref)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body) //nolint:errcheck
}

// upstreamCatalog reads a proxy member's image list. Most public registries
// refuse _catalog outright, which is not an error worth surfacing: the member
// simply contributes nothing to a merged catalog.
func (h *Handler) upstreamCatalog(c *format.Context) []string {
	upURL, ok := upstreamURL(c, "", "_catalog", "")
	if !ok || c.Repo.Upstream == "" {
		return nil
	}
	resp, err := h.upstreamGet(c, "", upURL, "application/json")
	if err != nil {
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc); err != nil {
		return nil
	}
	return doc.Repositories
}
