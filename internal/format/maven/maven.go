// Package maven implements the Maven 2 repository layout.
//
// Maven is essentially a structured filesystem over HTTP:
//
//	/{group/as/path}/{artifactId}/{version}/{artifactId}-{version}.{ext}
//
// Hosted: clients PUT artifacts (+ their .pom and checksum sidecars). We store
// them verbatim, can synthesize missing .md5/.sha1/.sha256 sidecars on read,
// and generate maven-metadata.xml from the versions actually present.
//
// Proxy: read-through cache of an upstream (e.g. Maven Central). On a miss we
// fetch upstream, persist, and serve.
//
// Not yet handled (documented TODOs): timestamped SNAPSHOT metadata, parent-POM
// prefetch, group repositories merging metadata across members.
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

func New() *Handler          { return &Handler{} }
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
	if base := strings.TrimSuffix(c.Sub, "."+checksumExt(c.Sub)); checksumExt(c.Sub) != "" &&
		path.Base(base) == "maven-metadata.xml" {
		if xml, ok := h.generateMetadata(c); ok {
			io.WriteString(w, checksumOf(checksumExt(c.Sub), xml))
			return
		}
	}

	// 4. Proxy: read-through to upstream.
	if c.Repo.Kind == repo.Proxy {
		h.proxyFetch(w, r, c, key)
		return
	}

	http.NotFound(w, r)
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
	groupArtifact := strings.ReplaceAll(artifactSub, "/", ".")
	lastDot := strings.LastIndex(groupArtifact, ".")
	if lastDot < 0 {
		return nil, false
	}
	groupID := groupArtifact[:lastDot]
	artifactID := groupArtifact[lastDot+1:]

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
	if len(versions) == 0 {
		return nil, false
	}
	sort.Strings(versions)
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
	return []byte(b.String()), true
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
