package oci

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"forge/internal/format"
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
	default:
		return "", false
	}
	return strings.TrimRight(c.Repo.Upstream, "/") + "/v2" + p, true
}

// upstreamGet performs one GET against the upstream registry. Every upstream
// read goes through here so authentication and caching have a single place to
// live. The caller closes the body.
func upstreamGet(c *format.Context, upURL, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, upURL, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return doUpstream(c, req)
}

// upstreamTags reads a proxy member's tag list from upstream. A proxy's local
// cache holds only what has been pulled, so a group built from it would hide
// tags the group can actually serve.
func (h *Handler) upstreamTags(c *format.Context, image string) []string {
	upURL, ok := upstreamURL(c, image, "tags/list", "")
	if !ok || c.Repo.Upstream == "" {
		return nil
	}
	resp, err := upstreamGet(c, upURL, "application/json")
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

// doUpstream sends one prepared upstream request.
//
// TODO(F3a): registries that require a token (Docker Hub, ghcr.io, quay.io)
// answer 401 with a Bearer challenge; this is where that handshake belongs.
func doUpstream(c *format.Context, req *http.Request) (*http.Response, error) {
	if c.Repo.ProxyAuth != "" {
		req.Header.Set("Authorization", c.Repo.ProxyAuth)
	}
	return c.HTTP.Do(req) // #nosec G704 -- URL built from the admin-configured upstream, not user input
}
