// Package maven implements the Maven 2 repository layout.
//
// Maven is essentially a structured filesystem over HTTP:
//
//	/{group/as/path}/{artifactId}/{version}/{artifactId}-{version}.{ext}
//
// Hosted: clients PUT artifacts (+ their .pom and checksum sidecars). We store
// them verbatim, synthesize missing .md5/.sha1/.sha256 sidecars on read, and
// generate maven-metadata.xml from the versions actually present.
//
// Proxy: read-through cache of an upstream (e.g. Maven Central). On a miss we
// fetch upstream, persist, and serve.
//
// Group: read-only fan-out over ordered member repos. maven-metadata.xml merges
// version lists from all members; artifact GETs try each member in order.
package maven

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "maven" }

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.get(w, r, c)
	case http.MethodPut:
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to a non-hosted repository", http.StatusMethodNotAllowed)
			return
		}
		h.put(w, r, c)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request, c *format.Context) {
	info, err := c.Blob.Put(c.Key(c.Sub), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored %s (%d bytes, sha1=%s)\n", c.Sub, info.Size, info.SHA1)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *format.Context) {
	if c.Repo.Kind == repo.Group {
		h.groupGet(w, r, c)
		return
	}

	key := c.Key(c.Sub)

	// 1. Serve from storage if present.
	if rc, err := c.Blob.Get(key); err == nil {
		defer rc.Close()
		w.Header().Set("Content-Type", contentType(c.Sub))
		io.Copy(w, rc)
		return
	}

	// 2. Synthesize a checksum sidecar from its base artifact if possible.
	if cs := checksumExt(c.Sub); cs != "" {
		baseSub := strings.TrimSuffix(c.Sub, "."+cs)
		if base, err := c.Blob.Get(c.Key(baseSub)); err == nil {
			defer base.Close()
			b, _ := io.ReadAll(base)
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, checksumOf(cs, b))
			return
		}
	}

	// 3. Generate maven-metadata.xml from the versions we actually hold.
	if path.Base(c.Sub) == "maven-metadata.xml" {
		if xml, ok := h.generateMetadata(c); ok {
			w.Header().Set("Content-Type", "application/xml")
			w.Write(xml)
			return
		}
	}

	// 4. Checksum of maven-metadata.xml: pass a context with the metadata path
	// so generateMetadata can find the artifact directory correctly.
	if cs := checksumExt(c.Sub); cs != "" {
		if base := strings.TrimSuffix(c.Sub, "."+cs); path.Base(base) == "maven-metadata.xml" {
			metaCtx := *c
			metaCtx.Sub = base
			if xml, ok := h.generateMetadata(&metaCtx); ok {
				io.WriteString(w, checksumOf(cs, xml))
				return
			}
		}
	}

	// 5. Proxy: read-through to upstream.
	if c.Repo.Kind == repo.Proxy {
		h.proxyFetch(w, r, c, key)
		return
	}

	http.NotFound(w, r)
}

// groupGet fans out GET requests across the group's ordered member repos.
// maven-metadata.xml is merged (union of versions); all other artifacts use
// first-member-wins semantics.
func (h *Handler) groupGet(w http.ResponseWriter, r *http.Request, c *format.Context) {
	if path.Base(c.Sub) == "maven-metadata.xml" {
		h.groupMetadata(w, c)
		return
	}
	if cs := checksumExt(c.Sub); cs != "" {
		if base := strings.TrimSuffix(c.Sub, "."+cs); path.Base(base) == "maven-metadata.xml" {
			metaSub := base
			// Build a temporary context with the metadata path for merging.
			mc := *c
			mc.Sub = metaSub
			if xml, ok := h.groupMetadataBytes(&mc); ok {
				io.WriteString(w, checksumOf(cs, xml))
			} else {
				http.NotFound(w, nil)
			}
			return
		}
	}
	// Artifact: first member hit wins (proxy members will fetch+cache on miss).
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		cap := format.NewCapture()
		h.get(cap, r, mc)
		if cap.OK() {
			cap.Replay(w)
			return
		}
	}
	http.NotFound(w, r)
}

func (h *Handler) groupMetadata(w http.ResponseWriter, c *format.Context) {
	xml, ok := h.groupMetadataBytes(c)
	if !ok {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write(xml)
}

func (h *Handler) groupMetadataBytes(c *format.Context) ([]byte, bool) {
	artifactSub := strings.TrimSuffix(c.Sub, "/maven-metadata.xml")
	if artifactSub == c.Sub {
		return nil, false
	}
	seen := map[string]bool{}
	var all []string
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		for _, v := range h.listVersions(mc, artifactSub) {
			if !seen[v] {
				seen[v] = true
				all = append(all, v)
			}
		}
	}
	return h.metadataFor(artifactSub, all)
}

func (h *Handler) proxyFetch(w http.ResponseWriter, r *http.Request, c *format.Context, key string) {
	url := strings.TrimRight(c.Repo.Upstream, "/") + "/" + c.Sub
	resp, err := c.HTTP.Get(url)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream returned "+resp.Status, resp.StatusCode)
		return
	}
	// Tee into storage while streaming to the client (cache-on-read).
	var buf bytes.Buffer
	tee := io.TeeReader(resp.Body, &buf)
	w.Header().Set("Content-Type", contentType(c.Sub))
	io.Copy(w, tee)
	_, _ = c.Blob.Put(key, &buf)
}

// generateMetadata lists versions under the artifact directory and emits a
// minimal but valid maven-metadata.xml.
func (h *Handler) generateMetadata(c *format.Context) ([]byte, bool) {
	artifactSub := strings.TrimSuffix(c.Sub, "/maven-metadata.xml")
	if artifactSub == c.Sub {
		return nil, false
	}
	return h.metadataFor(artifactSub, h.listVersions(c, artifactSub))
}

// listVersions returns the distinct artifact versions present under artifactSub.
func (h *Handler) listVersions(c *format.Context, artifactSub string) []string {
	keys, _ := c.Blob.List(c.Key(artifactSub) + "/")
	seen := map[string]bool{}
	var versions []string
	prefix := c.Key(artifactSub) + "/"
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		ver, _, ok := strings.Cut(rest, "/")
		if !ok || ver == "" || seen[ver] {
			continue
		}
		seen[ver] = true
		versions = append(versions, ver)
	}
	return versions
}

// metadataFor sorts versions and builds a maven-metadata.xml from them.
func (h *Handler) metadataFor(artifactSub string, versions []string) ([]byte, bool) {
	if len(versions) == 0 {
		return nil, false
	}
	sort.Strings(versions)
	groupArtifact := strings.ReplaceAll(artifactSub, "/", ".")
	lastDot := strings.LastIndex(groupArtifact, ".")
	if lastDot < 0 {
		return nil, false
	}
	return buildMetadataXML(groupArtifact[:lastDot], groupArtifact[lastDot+1:], versions), true
}

// buildMetadataXML is the pure generator (versions must be pre-sorted).
func buildMetadataXML(groupID, artifactID string, versions []string) []byte {
	latest := versions[len(versions)-1]
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, "<metadata>\n  <groupId>%s</groupId>\n  <artifactId>%s</artifactId>\n",
		groupID, artifactID)
	b.WriteString("  <versioning>\n")
	fmt.Fprintf(&b, "    <latest>%s</latest>\n    <release>%s</release>\n", latest, latest)
	b.WriteString("    <versions>\n")
	for _, v := range versions {
		fmt.Fprintf(&b, "      <version>%s</version>\n", v)
	}
	b.WriteString("    </versions>\n  </versioning>\n</metadata>\n")
	return []byte(b.String())
}

func checksumExt(p string) string {
	for _, e := range []string{"md5", "sha1", "sha256"} {
		if strings.HasSuffix(p, "."+e) {
			return e
		}
	}
	return ""
}

func checksumOf(ext string, b []byte) string {
	switch ext {
	case "md5":
		return blob.MD5(b)
	case "sha1":
		return blob.SHA1(b)
	case "sha256":
		return blob.SHA256(b)
	}
	return ""
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".jar"):
		return "application/java-archive"
	case strings.HasSuffix(p, ".pom"), strings.HasSuffix(p, ".xml"):
		return "application/xml"
	default:
		return "application/octet-stream"
	}
}
