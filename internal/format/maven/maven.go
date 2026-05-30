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
// fetch upstream, persist, and serve. Parent POMs are prefetched eagerly to
// avoid extra round trips during dependency resolution.
//
// Group: read-only fan-out over ordered member repos. maven-metadata.xml merges
// version lists from all members; artifact GETs try each member in order.
//
// SNAPSHOT support: when a timestamped SNAPSHOT artifact is PUT, the version
// directory's maven-metadata.xml is maintained in the meta store and generated
// on demand with proper <snapshotVersions> entries. Non-unique SNAPSHOTs
// (plain -SNAPSHOT suffix) are stored and served verbatim.
package maven

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "maven" }

// --- SNAPSHOT tracking types -----------------------------------------------

// snapshotMeta is stored in the meta store (ns "{repo}:maven:snap") keyed by
// the SNAPSHOT version directory path (e.g. "com/acme/lib/1.0-SNAPSHOT").
type snapshotMeta struct {
	GroupID     string            `json:"groupId"`
	ArtifactID  string            `json:"artifactId"`
	Version     string            `json:"version"`    // e.g. "1.0-SNAPSHOT"
	Timestamp   string            `json:"timestamp"`  // latest "20240115.123456"
	BuildNumber int               `json:"buildNumber"`
	Updated     string            `json:"updated"`    // "20240115123456"
	Versions    []snapshotVersion `json:"versions"`
}

type snapshotVersion struct {
	Classifier string `json:"classifier,omitempty"`
	Extension  string `json:"extension"`
	Value      string `json:"value"`   // e.g. "1.0-20240115.123456-1"
	Updated    string `json:"updated"` // "20240115123456"
}

func (h *Handler) snapNS(c *format.Context) string { return c.Repo.Name + ":maven:snap" }

// --- HTTP handlers ---------------------------------------------------------

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
	// Maintain SNAPSHOT version tracking for artifact files.
	h.maybeUpdateSnapshotMeta(c)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored %s (%d bytes, sha1=%s)\n", c.Sub, info.Size, info.SHA1)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *format.Context) {
	if c.Repo.Kind == repo.Group {
		h.groupGet(w, r, c)
		return
	}

	key := c.Key(c.Sub)

	// 1. Serve from storage if present (handles client-PUT maven-metadata.xml too).
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

	// 3. Generate maven-metadata.xml: SNAPSHOT-level or artifact-level.
	if path.Base(c.Sub) == "maven-metadata.xml" {
		if isSnapshotMetaPath(c.Sub) {
			if xml, ok := h.generateSnapshotMetadata(c); ok {
				w.Header().Set("Content-Type", "application/xml")
				w.Write(xml)
				return
			}
		}
		if xml, ok := h.generateMetadata(c); ok {
			w.Header().Set("Content-Type", "application/xml")
			w.Write(xml)
			return
		}
	}

	// 4. Checksum of a generated maven-metadata.xml.
	if cs := checksumExt(c.Sub); cs != "" {
		if base := strings.TrimSuffix(c.Sub, "."+cs); path.Base(base) == "maven-metadata.xml" {
			metaCtx := *c
			metaCtx.Sub = base
			var xml []byte
			var ok bool
			if isSnapshotMetaPath(base) {
				xml, ok = h.generateSnapshotMetadata(&metaCtx)
			}
			if !ok {
				xml, ok = h.generateMetadata(&metaCtx)
			}
			if ok {
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

// --- group -----------------------------------------------------------------

func (h *Handler) groupGet(w http.ResponseWriter, r *http.Request, c *format.Context) {
	if path.Base(c.Sub) == "maven-metadata.xml" {
		h.groupMetadata(w, c)
		return
	}
	if cs := checksumExt(c.Sub); cs != "" {
		if base := strings.TrimSuffix(c.Sub, "."+cs); path.Base(base) == "maven-metadata.xml" {
			mc := *c
			mc.Sub = base
			if xml, ok := h.groupMetadataBytes(&mc); ok {
				io.WriteString(w, checksumOf(cs, xml))
			} else {
				http.NotFound(w, nil)
			}
			return
		}
	}
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

// --- proxy -----------------------------------------------------------------

func (h *Handler) proxyFetch(w http.ResponseWriter, r *http.Request, c *format.Context, key string) {
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/" + c.Sub
	resp, err := c.HTTP.Get(upURL)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream returned "+resp.Status, resp.StatusCode)
		return
	}
	var buf bytes.Buffer
	tee := io.TeeReader(resp.Body, &buf)
	w.Header().Set("Content-Type", contentType(c.Sub))
	io.Copy(w, tee)
	_, _ = c.Blob.Put(key, &buf)

	// Eagerly prefetch parent POM to avoid extra round trips during resolution.
	if strings.HasSuffix(c.Sub, ".pom") {
		h.prefetchParentPOM(c, buf.Bytes())
	}
}

// prefetchParentPOM parses a POM for a <parent> element and fetches it from
// upstream if not already cached.
func (h *Handler) prefetchParentPOM(c *format.Context, pomData []byte) {
	var pom struct {
		XMLName xml.Name `xml:"project"`
		Parent  *struct {
			GroupID    string `xml:"groupId"`
			ArtifactID string `xml:"artifactId"`
			Version    string `xml:"version"`
		} `xml:"parent"`
	}
	if err := xml.Unmarshal(pomData, &pom); err != nil || pom.Parent == nil {
		return
	}
	p := pom.Parent
	if p.GroupID == "" || p.ArtifactID == "" || p.Version == "" {
		return
	}
	parentPath := strings.ReplaceAll(p.GroupID, ".", "/") + "/" +
		p.ArtifactID + "/" + p.Version + "/" +
		p.ArtifactID + "-" + p.Version + ".pom"
	parentKey := c.Key(parentPath)
	if _, err := c.Blob.Get(parentKey); err == nil {
		return // already cached
	}
	parentURL := strings.TrimRight(c.Repo.Upstream, "/") + "/" + parentPath
	resp, err := c.HTTP.Get(parentURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	c.Blob.Put(parentKey, bytes.NewReader(data))
}

// --- artifact-level metadata -----------------------------------------------

func (h *Handler) generateMetadata(c *format.Context) ([]byte, bool) {
	artifactSub := strings.TrimSuffix(c.Sub, "/maven-metadata.xml")
	if artifactSub == c.Sub {
		return nil, false
	}
	return h.metadataFor(artifactSub, h.listVersions(c, artifactSub))
}

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

// --- SNAPSHOT metadata -----------------------------------------------------

// isSnapshotMetaPath reports whether sub is a maven-metadata.xml inside a
// SNAPSHOT version directory (e.g. "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml").
func isSnapshotMetaPath(sub string) bool {
	return strings.HasSuffix(path.Dir(sub), "-SNAPSHOT")
}

// maybeUpdateSnapshotMeta updates SNAPSHOT version tracking in the meta store
// whenever an artifact is PUT into a SNAPSHOT version directory.
func (h *Handler) maybeUpdateSnapshotMeta(c *format.Context) {
	parts := strings.Split(c.Sub, "/")
	if len(parts) < 3 {
		return
	}
	versionDir := parts[len(parts)-2]
	if !strings.HasSuffix(versionDir, "-SNAPSHOT") {
		return
	}
	filename := parts[len(parts)-1]
	// Skip metadata, checksums, and signatures — only track artifacts.
	if filename == "maven-metadata.xml" || checksumExt(filename) != "" || strings.HasSuffix(filename, ".asc") {
		return
	}

	artifactID := parts[len(parts)-3]
	snapshotPath := strings.Join(parts[:len(parts)-1], "/")
	groupPath := strings.Join(parts[:len(parts)-3], "/")
	groupID := strings.ReplaceAll(groupPath, "/", ".")

	ext, value, ok := parseArtifactFilename(filename, artifactID)
	if !ok {
		return
	}

	var sm snapshotMeta
	c.Meta.GetJSON(h.snapNS(c), snapshotPath, &sm)
	if sm.ArtifactID == "" {
		sm = snapshotMeta{GroupID: groupID, ArtifactID: artifactID, Version: versionDir}
	}

	ts, bn, hasTimestamp := extractTimestamp(value)
	if !hasTimestamp {
		// Non-unique SNAPSHOT: record the extension but no timestamp info.
		h.upsertVersion(&sm, snapshotVersion{Extension: ext, Value: value})
		c.Meta.PutJSON(h.snapNS(c), snapshotPath, sm)
		return
	}

	updated := strings.ReplaceAll(ts, ".", "")
	if bn > sm.BuildNumber {
		sm.BuildNumber = bn
		sm.Timestamp = ts
		sm.Updated = updated
	}
	h.upsertVersion(&sm, snapshotVersion{Extension: ext, Value: value, Updated: updated})
	c.Meta.PutJSON(h.snapNS(c), snapshotPath, sm)
}

// upsertVersion adds or replaces the snapshotVersion entry for the given
// extension+classifier, keeping the entry with the higher build number.
func (h *Handler) upsertVersion(sm *snapshotMeta, sv snapshotVersion) {
	_, svBN, _ := extractTimestamp(sv.Value)
	for i, existing := range sm.Versions {
		if existing.Extension == sv.Extension && existing.Classifier == sv.Classifier {
			_, existBN, _ := extractTimestamp(existing.Value)
			if svBN >= existBN {
				sm.Versions[i] = sv
			}
			return
		}
	}
	sm.Versions = append(sm.Versions, sv)
}

func (h *Handler) generateSnapshotMetadata(c *format.Context) ([]byte, bool) {
	snapshotPath := strings.TrimSuffix(c.Sub, "/maven-metadata.xml")
	var sm snapshotMeta
	if ok, _ := c.Meta.GetJSON(h.snapNS(c), snapshotPath, &sm); !ok {
		return nil, false
	}
	return buildSnapshotMetadataXML(sm), true
}

func buildSnapshotMetadataXML(sm snapshotMeta) []byte {
	// Sort versions for deterministic output.
	sort.Slice(sm.Versions, func(i, j int) bool {
		if sm.Versions[i].Extension != sm.Versions[j].Extension {
			return sm.Versions[i].Extension < sm.Versions[j].Extension
		}
		return sm.Versions[i].Classifier < sm.Versions[j].Classifier
	})

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, "<metadata>\n  <groupId>%s</groupId>\n  <artifactId>%s</artifactId>\n  <version>%s</version>\n",
		sm.GroupID, sm.ArtifactID, sm.Version)
	b.WriteString("  <versioning>\n")
	if sm.Timestamp != "" {
		fmt.Fprintf(&b, "    <snapshot>\n      <timestamp>%s</timestamp>\n      <buildNumber>%d</buildNumber>\n    </snapshot>\n",
			sm.Timestamp, sm.BuildNumber)
	}
	if sm.Updated != "" {
		fmt.Fprintf(&b, "    <lastUpdated>%s</lastUpdated>\n", sm.Updated)
	}
	if len(sm.Versions) > 0 {
		b.WriteString("    <snapshotVersions>\n")
		for _, sv := range sm.Versions {
			b.WriteString("      <snapshotVersion>\n")
			if sv.Classifier != "" {
				fmt.Fprintf(&b, "        <classifier>%s</classifier>\n", sv.Classifier)
			}
			fmt.Fprintf(&b, "        <extension>%s</extension>\n        <value>%s</value>\n        <updated>%s</updated>\n",
				sv.Extension, sv.Value, sv.Updated)
			b.WriteString("      </snapshotVersion>\n")
		}
		b.WriteString("    </snapshotVersions>\n")
	}
	b.WriteString("  </versioning>\n</metadata>\n")
	return []byte(b.String())
}

// --- filename parsing ------------------------------------------------------

// parseArtifactFilename extracts extension and version value from a Maven
// artifact filename. "lib-1.0-20240115.123456-1.jar" with artifactID "lib"
// yields ext="jar", value="1.0-20240115.123456-1".
func parseArtifactFilename(filename, artifactID string) (ext, value string, ok bool) {
	prefix := artifactID + "-"
	if !strings.HasPrefix(filename, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(filename, prefix)
	dotIdx := strings.LastIndex(rest, ".")
	if dotIdx < 0 {
		return "", "", false
	}
	return rest[dotIdx+1:], rest[:dotIdx], true
}

// extractTimestamp finds a Maven snapshot timestamp and build number in a
// versioned value string such as "1.0-20240115.123456-1".
// Returns ("20240115.123456", 1, true) for that input.
func extractTimestamp(value string) (timestamp string, buildNumber int, ok bool) {
	// Scan for the pattern: 8 digits, '.', 6 digits, '-', digits
	for i := 0; i+16 <= len(value); i++ {
		if value[i+8] != '.' {
			continue
		}
		if !isDigits(value[i:i+8]) || !isDigits(value[i+9:i+15]) {
			continue
		}
		if value[i+15] != '-' {
			continue
		}
		bnStr := value[i+16:]
		if !isDigits(bnStr) {
			continue
		}
		bn, err := strconv.Atoi(bnStr)
		if err != nil {
			continue
		}
		return value[i : i+15], bn, true
	}
	return "", 0, false
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// --- shared helpers --------------------------------------------------------

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
	case strings.HasSuffix(p, ".module"):
		return "application/vnd.gradle.module+json"
	default:
		return "application/octet-stream"
	}
}
