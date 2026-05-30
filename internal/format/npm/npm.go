// Package npm implements the npm registry protocol (subset).
//
// Hosted:
//
//	PUT  /{pkg}                          -> publish (metadata + base64 _attachments)
//	PUT  /{pkg}  (no _attachments)       -> deprecate (sets deprecated field on versions)
//	GET  /{pkg}                          -> packument (tarball URLs point back at us)
//	GET  /{pkg}/-/{tarball}              -> tarball download
//	DELETE /{pkg}                        -> unpublish whole package
//	DELETE /{pkg}/-/{tarball}            -> unpublish one tarball + prune packument
//	GET  /-/package/{pkg}/dist-tags      -> list dist-tags
//	PUT  /-/package/{pkg}/dist-tags/{t}  -> set a dist-tag
//	DELETE /-/package/{pkg}/dist-tags/{t} -> remove a dist-tag
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
// Misc endpoints (registry API, not repo-specific):
//
//	GET  /-/ping                         -> {}
//	GET  /-/whoami                       -> {"username":"anonymous"}
//	PUT  /-/user/org.couchdb.user:{u}    -> CouchDB login (returns empty token)
//	POST /-/npm/v1/security/audits[/quick] -> empty clean audit report
//
// Scoped packages (@scope/name) arrive with the slash percent-encoded
// (%2F); Go's net/http decodes it in r.URL.Path so routing is transparent.
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

	// Registry API endpoints live under the "-/" namespace and are not package ops.
	if strings.HasPrefix(sub, "-/") {
		h.serveAPI(w, r, c, sub)
		return
	}

	// Tarball path: {pkg}/-/{filename}
	if strings.Contains(sub, "/-/") {
		switch r.Method {
		case http.MethodGet:
			h.tarball(w, c, sub)
		case http.MethodDelete:
			if c.Repo.Kind != repo.Hosted {
				http.Error(w, "cannot delete from non-hosted repo", http.StatusMethodNotAllowed)
				return
			}
			h.deleteTarball(w, c, sub)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// Package-level operations.
	switch r.Method {
	case http.MethodGet:
		h.packument(w, r, c, sub)
	case http.MethodPut:
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.publish(w, r, c, sub)
	case http.MethodDelete:
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot delete from non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.unpublish(w, c, sub)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveAPI handles the /-/ registry endpoints that are not tied to a specific
// package (login, audit, ping, whoami, dist-tags).
func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, c *format.Context, sub string) {
	switch {
	case strings.HasPrefix(sub, "-/package/") && strings.Contains(sub, "/dist-tags"):
		h.distTags(w, r, c, sub)

	case strings.HasPrefix(sub, "-/user/") && r.Method == http.MethodPut:
		h.login(w)

	case strings.HasPrefix(sub, "-/npm/v1/security/audits") && r.Method == http.MethodPost:
		h.audit(w)

	case sub == "-/ping" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}\n")

	case sub == "-/whoami" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"username": "anonymous"})

	default:
		http.Error(w, "unsupported npm api endpoint: "+sub, http.StatusNotFound)
	}
}

// --- publish & deprecate ---------------------------------------------------

// publish parses the npm publish payload, stores attachments, and updates the
// packument. When _attachments is absent (e.g. npm deprecate) it only updates
// the packument, which propagates the deprecated field on version objects.
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

// --- unpublish -------------------------------------------------------------

// unpublish removes the entire package: its packument and all stored tarballs.
func (h *Handler) unpublish(w http.ResponseWriter, c *format.Context, pkg string) {
	c.Meta.Delete(h.ns(c), pkg)
	// Blobs for all versions live under {repo}/{pkg}/-/
	if keys, err := c.Blob.List(c.Key(pkg + "/-/")); err == nil {
		for _, k := range keys {
			c.Blob.Delete(k)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// deleteTarball removes one tarball blob and prunes its version from the
// packument. sub is "{pkg}/-/{filename}".
func (h *Handler) deleteTarball(w http.ResponseWriter, c *format.Context, sub string) {
	c.Blob.Delete(c.Key(sub))

	i := strings.Index(sub, "/-/")
	pkg := sub[:i]
	filename := sub[i+3:]

	var packument map[string]any
	if ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &packument); !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if versions, ok := packument["versions"].(map[string]any); ok {
		// Derive version from filename: "{pkgbase}-{ver}.tgz"
		base := strings.TrimSuffix(filename, ".tgz")
		pkgBase := lastPathSeg(pkg)
		ver := strings.TrimPrefix(base, pkgBase+"-")
		delete(versions, ver)
		packument["versions"] = versions
		c.Meta.PutJSON(h.ns(c), pkg, packument)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// --- dist-tags -------------------------------------------------------------

// distTags handles GET/PUT/DELETE on /-/package/{pkg}/dist-tags[/{tag}].
func (h *Handler) distTags(w http.ResponseWriter, r *http.Request, c *format.Context, sub string) {
	// sub = "-/package/{pkg}/dist-tags" or "-/package/{pkg}/dist-tags/{tag}"
	rest := strings.TrimPrefix(sub, "-/package/")
	pkgPart, tagSuffix, _ := strings.Cut(rest, "/dist-tags")
	pkg := pkgPart
	tag := strings.TrimPrefix(tagSuffix, "/")

	var packument map[string]any
	if ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &packument); !ok {
		http.NotFound(w, r)
		return
	}
	distTags, _ := packument["dist-tags"].(map[string]any)
	if distTags == nil {
		distTags = map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		if tag == "" {
			json.NewEncoder(w).Encode(distTags)
		} else {
			ver, ok := distTags[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(ver)
		}

	case http.MethodPut:
		if tag == "" {
			http.Error(w, "tag name required", http.StatusBadRequest)
			return
		}
		var ver string
		if err := json.NewDecoder(r.Body).Decode(&ver); err != nil {
			http.Error(w, "body must be a JSON string (version)", http.StatusBadRequest)
			return
		}
		distTags[tag] = ver
		packument["dist-tags"] = distTags
		c.Meta.PutJSON(h.ns(c), pkg, packument)
		json.NewEncoder(w).Encode(distTags)

	case http.MethodDelete:
		if tag == "" {
			http.Error(w, "tag name required", http.StatusBadRequest)
			return
		}
		delete(distTags, tag)
		packument["dist-tags"] = distTags
		c.Meta.PutJSON(h.ns(c), pkg, packument)
		json.NewEncoder(w).Encode(distTags)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- login & audit ---------------------------------------------------------

// login implements the CouchDB-style PUT /-/user/... endpoint npm uses for
// `npm login`. We return an empty token; in eval mode (AllowAll) this is
// sufficient. In auth mode the empty token will be rejected by the middleware
// on the next request — users should configure _authToken directly instead.
func (h *Handler) login(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": ""})
}

// audit responds to npm audit requests with a clean (zero-vulnerability) report.
func (h *Handler) audit(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"actions":   []any{},
		"advisories": map[string]any{},
		"muted":     []any{},
		"metadata": map[string]any{
			"vulnerabilities": map[string]int{
				"info": 0, "low": 0, "moderate": 0, "high": 0, "critical": 0,
			},
			"dependencies":      0,
			"devDependencies":   0,
			"totalDependencies": 0,
		},
	})
}

// --- packument & tarball ---------------------------------------------------

func (h *Handler) packument(w http.ResponseWriter, r *http.Request, c *format.Context, pkg string) {
	if c.Repo.Kind == repo.Group {
		h.groupPackument(w, r, c, pkg)
		return
	}
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
		if !topSet {
			for k, v := range doc {
				if k != "versions" && k != "dist-tags" {
					merged[k] = v
				}
			}
			topSet = true
		}
		if mv, ok := doc["versions"].(map[string]any); ok {
			for ver, vdata := range mv {
				if _, exists := versions[ver]; exists {
					continue
				}
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

// --- helpers ---------------------------------------------------------------

// rewriteTarball maps a tarball URL to a different repo on this server,
// keeping the /-/{filename} tail intact.
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
