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
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/proxy"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "maven" }

// --- SNAPSHOT tracking types -----------------------------------------------

// snapshotMeta is assembled on-demand from snapArtifact records and passed to
// buildSnapshotMetadataXML. It is no longer written to the meta store.
type snapshotMeta struct {
	GroupID     string            `json:"groupId"`
	ArtifactID  string            `json:"artifactId"`
	Version     string            `json:"version"`
	Timestamp   string            `json:"timestamp"`
	BuildNumber int               `json:"buildNumber"`
	Updated     string            `json:"updated"`
	Versions    []snapshotVersion `json:"versions"`
}

type snapshotVersion struct {
	Classifier string `json:"classifier,omitempty"`
	Extension  string `json:"extension"`
	Value      string `json:"value"`
	Updated    string `json:"updated"`
}

// snapArtifact is stored per published SNAPSHOT artifact in the
// "{repo}:maven:snap:v" namespace.
// Key: snapshotPath + ":" + ext + ":" + classifier
// (e.g. "com/acme/lib/1.0-SNAPSHOT:jar:")
//
// Using per-artifact keys means concurrent PUTs to different artifacts
// (jar vs pom vs sources) write to distinct keys and never conflict.
// Two PUTs to the same artifact (same ext+classifier) are last-writer-wins,
// which is correct: the latest build's record replaces the previous one.
type snapArtifact struct {
	GroupID    string    `json:"groupId"`
	ArtifactID string    `json:"artifactId"`
	Version    string    `json:"version"`
	Classifier string    `json:"classifier,omitempty"`
	Extension  string    `json:"extension"`
	Value      string    `json:"value"`
	Updated    string    `json:"updated"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func (h *Handler) snapNS(c *format.Context) string    { return c.Repo.Name + ":maven:snap" }
func (h *Handler) snapVersNS(c *format.Context) string { return c.Repo.Name + ":maven:snap:v" }

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
	case http.MethodDelete:
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot delete from a non-hosted repository", http.StatusMethodNotAllowed)
			return
		}
		h.deleteArtifact(w, c)
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
	cfg := proxy.Config{TTL: c.Repo.ProxyTTL, Auth: c.Repo.ProxyAuth}
	if c.Metrics != nil {
		m, repo := c.Metrics, c.Repo.Name
		cfg.RecordHit = func() { m.CacheHits.WithLabelValues(repo).Inc() }
		cfg.RecordMiss = func() { m.CacheMisses.WithLabelValues(repo).Inc() }
	}
	f := proxy.New(c.HTTP, cfg)
	rc, ct, err := f.Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if errors.Is(err, proxy.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer rc.Close()

	// Eagerly prefetch parent POM after a cache miss (data is freshly fetched).
	if strings.HasSuffix(c.Sub, ".pom") {
		body, _ := io.ReadAll(rc)
		if ct == "" {
			ct = contentType(c.Sub)
		}
		w.Header().Set("Content-Type", ct)
		w.Write(body)
		h.prefetchParentPOM(c, body)
		return
	}

	if ct == "" {
		ct = contentType(c.Sub)
	}
	w.Header().Set("Content-Type", ct)
	io.Copy(w, rc)
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

// maybeUpdateSnapshotMeta stores one snapArtifact record per published
// SNAPSHOT artifact under a unique key, eliminating the read-modify-write
// race that existed when a single assembled snapshotMeta was shared across
// concurrent artifact PUTs.
//
// Key: snapshotPath + ":" + ext + ":" + classifier
// Two PUTs for the same ext+classifier (same artifact, newer build) overwrite
// the previous record — correct, since only the latest build matters.
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

	ts, _, hasTimestamp := extractTimestamp(value)
	var updated string
	if hasTimestamp {
		updated = strings.ReplaceAll(ts, ".", "")
	}

	rec := snapArtifact{
		GroupID: groupID, ArtifactID: artifactID, Version: versionDir,
		Extension: ext, Value: value, Updated: updated,
		UploadedAt: time.Now().UTC(),
	}
	// Key is unique per artifact type; concurrent PUTs to different types
	// (jar, pom, sources) never conflict.
	key := snapshotPath + ":" + ext + ":"
	c.Meta.PutJSON(h.snapVersNS(c), key, rec) //nolint:errcheck
}

func (h *Handler) generateSnapshotMetadata(c *format.Context) ([]byte, bool) {
	snapshotPath := strings.TrimSuffix(c.Sub, "/maven-metadata.xml")

	// Assemble snapshotMeta from per-artifact records (new format).
	versNS := h.snapVersNS(c)
	allKeys, _ := c.Meta.List(versNS)
	prefix := snapshotPath + ":"
	var sm snapshotMeta
	for _, k := range allKeys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		var rec snapArtifact
		if ok, _ := c.Meta.GetJSON(versNS, k, &rec); !ok {
			continue
		}
		if sm.GroupID == "" {
			sm = snapshotMeta{GroupID: rec.GroupID, ArtifactID: rec.ArtifactID, Version: rec.Version}
		}
		ts, bn, hasTS := extractTimestamp(rec.Value)
		if hasTS && bn > sm.BuildNumber {
			sm.BuildNumber = bn
			sm.Timestamp = ts
			sm.Updated = rec.Updated
		}
		sm.Versions = append(sm.Versions, snapshotVersion{
			Classifier: rec.Classifier, Extension: rec.Extension,
			Value: rec.Value, Updated: rec.Updated,
		})
	}
	if sm.GroupID != "" {
		return buildSnapshotMetadataXML(sm), true
	}

	// Fall back to old assembled record for backward compatibility.
	if ok, _ := c.Meta.GetJSON(h.snapNS(c), snapshotPath, &sm); ok {
		return buildSnapshotMetadataXML(sm), true
	}
	return nil, false
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

// --- delete ----------------------------------------------------------------

// deleteArtifact removes the artifact blob at the requested path and cleans
// up any associated SNAPSHOT meta record.
func (h *Handler) deleteArtifact(w http.ResponseWriter, c *format.Context) {
	key := c.Key(c.Sub)
	if _, exists, _ := c.Blob.Stat(key); !exists {
		http.NotFound(w, nil)
		return
	}
	if err := c.Blob.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.maybeDeleteSnapshotMeta(c)
	w.WriteHeader(http.StatusNoContent)
}

// maybeDeleteSnapshotMeta removes the snapArtifact meta record that
// corresponds to the just-deleted SNAPSHOT artifact file.
func (h *Handler) maybeDeleteSnapshotMeta(c *format.Context) {
	parts := strings.Split(c.Sub, "/")
	if len(parts) < 3 {
		return
	}
	versionDir := parts[len(parts)-2]
	if !strings.HasSuffix(versionDir, "-SNAPSHOT") {
		return
	}
	filename := parts[len(parts)-1]
	if filename == "maven-metadata.xml" || checksumExt(filename) != "" || strings.HasSuffix(filename, ".asc") {
		return
	}
	artifactID := parts[len(parts)-3]
	snapshotPath := strings.Join(parts[:len(parts)-1], "/")
	ext, _, ok := parseArtifactFilename(filename, artifactID)
	if !ok {
		return
	}
	key := snapshotPath + ":" + ext + ":"
	c.Meta.Delete(h.snapVersNS(c), key) //nolint:errcheck
}

// BrowseRepo implements format.Browsable.
// Maven blobs live at {repo}/{group/as/path}/{artifactId}/{version}/{file}.
// We detect the version segment (first part that starts with a digit) to
// derive groupId and artifactId without a separate metadata store.
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	if c.Repo.Kind == repo.Group {
		return format.GroupBrowse(h, c)
	}
	prefix := c.Repo.Name + "/"
	keys, err := c.Blob.List(prefix)
	if err != nil {
		return nil, err
	}
	type versionSet = map[string]struct{}
	byComp := map[string]versionSet{}
	for _, k := range keys {
		parts := strings.Split(strings.TrimPrefix(k, prefix), "/")
		// Minimum: groupPart…/artifactId/version/file → at least 4 segments
		if len(parts) < 4 {
			continue
		}
		verIdx := -1
		for i, p := range parts {
			if len(p) > 0 && p[0] >= '0' && p[0] <= '9' {
				verIdx = i
				break
			}
		}
		if verIdx < 1 {
			continue
		}
		comp := strings.Join(parts[:verIdx-1], ".") + ":" + parts[verIdx-1]
		version := parts[verIdx]
		if byComp[comp] == nil {
			byComp[comp] = versionSet{}
		}
		byComp[comp][version] = struct{}{}
	}
	entries := make([]format.BrowseEntry, 0, len(byComp))
	for comp, vset := range byComp {
		versions := make([]string, 0, len(vset))
		for v := range vset {
			versions = append(versions, v)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		entries = append(entries, format.BrowseEntry{Name: comp, Versions: versions})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
