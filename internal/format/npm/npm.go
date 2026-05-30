// Package npm implements the npm registry protocol (subset).
//
// Hosted:
//
//	PUT  /{pkg}                  -> publish (metadata + base64 _attachments)
//	GET  /{pkg}                  -> packument (tarball URLs point back at us)
//	GET  /{pkg}/-/{tarball}      -> tarball download
//
// Proxy:
//
//	GET  /{pkg}                  -> fetch upstream packument, rewrite tarball
//	                               URLs to our host, cache, serve
//	GET  /{pkg}/-/{tarball}      -> fetch upstream tarball, cache, serve
//
// Group: read-only fan-out. Packuments are merged (first-member wins per
// version and dist-tag); tarball downloads try each member in order.
//
// Scoped packages (@scope/name) arrive URL-encoded (@scope%2fname); we decode.
// The legacy CouchDB-style login flow is a documented TODO.
package npm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"forge/internal/format"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "npm" }

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + ":npm" }

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	sub, _ := url.PathUnescape(c.Sub)

	// Tarball request: .../-/...
	if i := strings.Index(sub, "/-/"); i >= 0 && r.Method == http.MethodGet {
		h.tarball(w, c, sub)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.packument(w, r, c, sub)
	case http.MethodPut:
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.publish(w, r, c, sub)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// publish parses the npm publish payload, stores attachments, and updates the
// packument with tarball URLs that point back at this server.
func (h *Handler) publish(w http.ResponseWriter, r *http.Request, c *format.Context, pkg string) {
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad publish payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Store attachments (the .tgz files), base64-decoded.
	var attachments map[string]struct {
		Data string `json:"data"`
	}
	if raw, ok := payload["_attachments"]; ok {
		json.Unmarshal(raw, &attachments)
	}
	for fname, att := range attachments {
		data, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			http.Error(w, "bad attachment encoding", http.StatusBadRequest)
			return
		}
		if _, err := c.Blob.Put(c.Key(pkg+"/-/"+fname), bytes.NewReader(data)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Load-or-init the stored packument.
	packument := map[string]any{
		"name":      pkg,
		"versions":  map[string]any{},
		"dist-tags": map[string]any{},
	}
	c.Meta.GetJSON(h.ns(c), pkg, &packument)

	versions, _ := packument["versions"].(map[string]any)
	if versions == nil {
		versions = map[string]any{}
	}

	// Merge incoming versions, rewriting dist.tarball to our URL.
	var incoming map[string]map[string]any
	if raw, ok := payload["versions"]; ok {
		json.Unmarshal(raw, &incoming)
	}
	base := publicBase(r)
	for ver, vobj := range incoming {
		tarName := fmt.Sprintf("%s-%s.tgz", lastPathSeg(pkg), ver)
		dist, _ := vobj["dist"].(map[string]any)
		if dist == nil {
			dist = map[string]any{}
		}
		dist["tarball"] = fmt.Sprintf("%s/repository/%s/%s/-/%s",
			base, c.Repo.Name, pkg, tarName)
		vobj["dist"] = dist
		versions[ver] = vobj
	}
	packument["versions"] = versions

	// Merge dist-tags.
	if raw, ok := payload["dist-tags"]; ok {
		var dt map[string]any
		json.Unmarshal(raw, &dt)
		existing, _ := packument["dist-tags"].(map[string]any)
		if existing == nil {
			existing = map[string]any{}
		}
		for k, v := range dt {
			existing[k] = v
		}
		packument["dist-tags"] = existing
	}

	if err := c.Meta.PutJSON(h.ns(c), pkg, packument); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) packument(w http.ResponseWriter, r *http.Request, c *format.Context, pkg string) {
	if c.Repo.Kind == repo.Group {
		h.groupPackument(w, r, c, pkg)
		return
	}
	// Hosted (or proxy cache hit): serve stored packument.
	var stored map[string]any
	if ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &stored); ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stored)
		return
	}
	if c.Repo.Kind == repo.Proxy {
		h.proxyPackument(w, r, c, pkg)
		return
	}
	http.NotFound(w, r)
}

// fetchPackument returns the packument for pkg from this repo. For proxy repos
// it fetches from upstream, caches, and returns the cached document.
func (h *Handler) fetchPackument(r *http.Request, c *format.Context, pkg string) (map[string]any, bool) {
	var stored map[string]any
	if ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &stored); ok {
		return stored, true
	}
	if c.Repo.Kind != repo.Proxy {
		return nil, false
	}
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/" + url.PathEscape(pkg)
	resp, err := c.HTTP.Get(upURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, false
	}
	defer resp.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, false
	}
	base := publicBase(r)
	if versions, ok := doc["versions"].(map[string]any); ok {
		for _, v := range versions {
			vobj, _ := v.(map[string]any)
			dist, _ := vobj["dist"].(map[string]any)
			if dist == nil {
				continue
			}
			if orig, ok := dist["tarball"].(string); ok {
				dist["tarball"] = rewriteTarball(orig, base, c.Repo.Name, pkg)
			}
		}
	}
	c.Meta.PutJSON(h.ns(c), pkg, doc)
	return doc, true
}

// proxyPackument fetches upstream metadata, rewrites every tarball URL to point
// at this proxy, caches the result, and serves it.
func (h *Handler) proxyPackument(w http.ResponseWriter, r *http.Request, c *format.Context, pkg string) {
	doc, ok := h.fetchPackument(r, c, pkg)
	if !ok {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// groupPackument merges packuments from all member repos. First member wins for
// version and dist-tag conflicts. Tarball URLs are rewritten to point at the
// group repo so clients download via the group.
func (h *Handler) groupPackument(w http.ResponseWriter, r *http.Request, c *format.Context, pkg string) {
	merged := map[string]any{
		"name":      pkg,
		"versions":  map[string]any{},
		"dist-tags": map[string]any{},
	}
	versions := merged["versions"].(map[string]any)
	distTags := merged["dist-tags"].(map[string]any)
	topSet := false
	found := false

	base := publicBase(r)
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		doc, ok := h.fetchPackument(r, mc, pkg)
		if !ok {
			continue
		}
		found = true
		// First member supplies package-level fields (description, homepage, etc.).
		if !topSet {
			for k, v := range doc {
				if k != "versions" && k != "dist-tags" {
					merged[k] = v
				}
			}
			topSet = true
		}
		// Merge versions: first member with a given version wins.
		if mv, ok := doc["versions"].(map[string]any); ok {
			for ver, vdata := range mv {
				if _, exists := versions[ver]; exists {
					continue
				}
				// Rewrite tarball URL to point to this group, not the member.
				if vobj, ok := vdata.(map[string]any); ok {
					if dist, ok := vobj["dist"].(map[string]any); ok {
						if orig, ok := dist["tarball"].(string); ok {
							dist["tarball"] = rewriteTarball(orig, base, c.Repo.Name, pkg)
						}
					}
				}
				versions[ver] = vdata
			}
		}
		// Merge dist-tags: first member wins.
		if dt, ok := doc["dist-tags"].(map[string]any); ok {
			for tag, ver := range dt {
				if _, exists := distTags[tag]; !exists {
					distTags[tag] = ver
				}
			}
		}
	}

	if !found {
		http.NotFound(w, r)
		return
	}
	merged["versions"] = versions
	merged["dist-tags"] = distTags
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(merged)
}

func (h *Handler) tarball(w http.ResponseWriter, c *format.Context, sub string) {
	if c.Repo.Kind == repo.Group {
		h.groupTarball(w, c, sub)
		return
	}
	key := c.Key(sub)
	if rc, err := c.Blob.Get(key); err == nil {
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, rc)
		return
	}
	if c.Repo.Kind == repo.Proxy {
		upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/" + sub
		resp, err := c.HTTP.Get(upURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			http.Error(w, "upstream tarball fetch failed", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		tee := io.TeeReader(resp.Body, &buf)
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, tee)
		c.Blob.Put(key, &buf)
		return
	}
	http.NotFound(w, nil)
}

// groupTarball tries each member in order; for proxy members it will fetch and
// cache from upstream on a blob cache miss.
func (h *Handler) groupTarball(w http.ResponseWriter, c *format.Context, sub string) {
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		key := mc.Key(sub)
		if rc, err := mc.Blob.Get(key); err == nil {
			defer rc.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			io.Copy(w, rc)
			return
		}
		if mc.Repo.Kind == repo.Proxy {
			upURL := strings.TrimRight(mc.Repo.Upstream, "/") + "/" + sub
			resp, err := mc.HTTP.Get(upURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				var buf bytes.Buffer
				tee := io.TeeReader(resp.Body, &buf)
				w.Header().Set("Content-Type", "application/octet-stream")
				io.Copy(w, tee)
				mc.Blob.Put(key, &buf)
				return
			}
		}
	}
	http.NotFound(w, nil)
}

// rewriteTarball maps an upstream tarball URL to this proxy. It keeps only the
// path after the package name so the proxy's own /-/ route resolves it.
func rewriteTarball(orig, base, repoName, pkg string) string {
	idx := strings.Index(orig, "/-/")
	if idx < 0 {
		return orig
	}
	tail := orig[idx:] // "/-/pkg-1.0.0.tgz"
	return fmt.Sprintf("%s/repository/%s/%s%s", base, repoName, pkg, tail)
}

func publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func lastPathSeg(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
