package maven

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/golden"
	"forge/internal/meta"
	"forge/internal/repo"
)

// serve calls the maven handler's Serve method via httptest and returns the recorder.
func serveReq(c *format.Context, method, sub string, body io.Reader) *httptest.ResponseRecorder {
	c.Sub = sub
	if body == nil {
		body = http.NoBody
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(method, "/", body), c)
	return rw
}

// --- HTTP endpoint tests ----------------------------------------------------

func TestServe_Put_Hosted(t *testing.T) {
	c := ctxWith(t, "")
	rw := serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar",
		strings.NewReader("artifact-bytes"))
	if rw.Code != http.StatusCreated {
		t.Fatalf("put: got %d, body: %s", rw.Code, rw.Body)
	}
}

func TestServe_Put_NonHosted_Rejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "maven-central", Format: "maven", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	rw := serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar",
		strings.NewReader("bytes"))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestServe_Get_Artifact(t *testing.T) {
	c := ctxWith(t, "", "maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar")
	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("get: got %d", rw.Code)
	}
}

func TestServe_Get_NotFound(t *testing.T) {
	c := ctxWith(t, "")
	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_Get_MetadataXML_Generated(t *testing.T) {
	c := ctxWith(t, "",
		"maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar",
		"maven-hosted/com/acme/lib/1.1.0/lib-1.1.0.jar",
	)
	rw := serveReq(c, http.MethodGet, "com/acme/lib/maven-metadata.xml", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("metadata: got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<version>1.1.0</version>") {
		t.Fatalf("metadata missing versions: %s", body)
	}
}

func TestServe_Get_MetadataXML_NotFound(t *testing.T) {
	c := ctxWith(t, "")
	rw := serveReq(c, http.MethodGet, "com/acme/absent/maven-metadata.xml", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_Get_ChecksumSynthesized(t *testing.T) {
	// SHA1 sidecar doesn't exist but the base artifact does; forge synthesizes it.
	c := ctxWith(t, "", "maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar")
	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar.sha1", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("sidecar: got %d", rw.Code)
	}
	sha1 := strings.TrimSpace(rw.Body.String())
	if len(sha1) != 40 {
		t.Fatalf("expected 40-char sha1, got %q", sha1)
	}
}

func TestServe_Get_ChecksumSidecar_NotFound(t *testing.T) {
	// Base artifact also missing — nothing to synthesize from.
	c := ctxWith(t, "")
	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar.sha1", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_Get_SnapshotMetadata(t *testing.T) {
	c := ctxWith(t, "")
	putArtifact(t, c, "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20240115.123456-1.jar")

	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("snapshot metadata: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "snapshotVersion") {
		t.Fatalf("snapshot metadata missing snapshotVersion: %s", rw.Body)
	}
}

func TestServe_MethodNotAllowed(t *testing.T) {
	c := ctxWith(t, "")
	rw := serveReq(c, http.MethodDelete, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestServe_Head(t *testing.T) {
	c := ctxWith(t, "", "maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar")
	rw := serveReq(c, http.MethodHead, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("HEAD: got %d", rw.Code)
	}
}

func TestServe_Proxy_Fetch(t *testing.T) {
	// Spin up a fake upstream serving a small artifact.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		io.WriteString(w, "upstream-artifact")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "maven-proxy", Format: "maven", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}

	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy fetch: got %d, body: %s", rw.Code, rw.Body)
	}
	if rw.Body.String() != "upstream-artifact" {
		t.Fatalf("unexpected proxy body: %q", rw.Body.String())
	}
}

func TestServe_Proxy_NotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "maven-proxy", Format: "maven", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}

	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from proxy, got %d", rw.Code)
	}
}

func TestServe_Group_Get(t *testing.T) {
	b, m := sharedStores(t)
	b.Put("maven-a/com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader("bytes"))

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "maven-a", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-group", Format: "maven", Kind: repo.Group, Members: []string{"maven-a"}},
	} {
		mgr.Add(r)
	}
	groupRepo, _ := mgr.Get("maven-group")
	c := &format.Context{
		Repo: groupRepo, Blob: b, Meta: m,
		Repos: mgr,
	}

	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.jar", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group get: got %d", rw.Code)
	}
}

func TestServe_Group_MetadataXML(t *testing.T) {
	b, m := sharedStores(t)
	b.Put("maven-a/com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader("bytes"))
	b.Put("maven-b/com/acme/lib/2.0.0/lib-2.0.0.jar", strings.NewReader("bytes"))

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "maven-a", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-b", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-group", Format: "maven", Kind: repo.Group, Members: []string{"maven-a", "maven-b"}},
	} {
		mgr.Add(r)
	}
	groupRepo, _ := mgr.Get("maven-group")
	c := &format.Context{
		Repo: groupRepo, Blob: b, Meta: m,
		Repos: mgr,
	}

	rw := serveReq(c, http.MethodGet, "com/acme/lib/maven-metadata.xml", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group metadata: got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "<version>1.0.0</version>") || !strings.Contains(body, "<version>2.0.0</version>") {
		t.Fatalf("group metadata missing versions: %s", body)
	}
}

func TestBrowseRepo_Maven(t *testing.T) {
	c := ctxWith(t, "",
		"maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar",
		"maven-hosted/com/acme/lib/2.0.0/lib-2.0.0.jar",
	)
	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "com.acme:lib" {
		t.Fatalf("unexpected entries: %v", entries)
	}
	if len(entries[0].Versions) != 2 {
		t.Fatalf("expected 2 versions, got %v", entries[0].Versions)
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"lib.jar":    "application/java-archive",
		"lib.pom":    "application/xml",
		"lib.xml":    "application/xml",
		"lib.module": "application/vnd.gradle.module+json",
		"lib.zip":    "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestChecksumExt(t *testing.T) {
	cases := map[string]string{
		"lib.jar.md5":    "md5",
		"lib.jar.sha1":   "sha1",
		"lib.jar.sha256": "sha256",
		"lib.jar":        "",
	}
	for name, want := range cases {
		if got := checksumExt(name); got != want {
			t.Errorf("checksumExt(%q) = %q, want %q", name, got, want)
		}
	}
}

// putArtifact simulates a PUT of a SNAPSHOT artifact, updating snapshot meta.
func putArtifact(t *testing.T, c *format.Context, sub string) {
	t.Helper()
	c.Blob.Put(c.Key(sub), strings.NewReader("fake"))
	old := c.Sub
	c.Sub = sub
	New().maybeUpdateSnapshotMeta(c)
	c.Sub = old
}

// ctxWith builds a Context backed by temp FS stores, pre-seeded with blob keys.
func ctxWith(t *testing.T, sub string, blobKeys ...string) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	for _, k := range blobKeys {
		if _, err := b.Put(k, strings.NewReader("artifact-bytes")); err != nil {
			t.Fatal(err)
		}
	}
	return &format.Context{
		Repo: repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
		Blob: b, Meta: m, Sub: sub,
	}
}

// sharedStores returns a shared blob+meta store pair backed by the same temp dir.
func sharedStores(t *testing.T) (blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return b, m
}

func TestGenerateMetadata_Golden(t *testing.T) {
	c := ctxWith(t, "com/acme/lib/maven-metadata.xml",
		"maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar",
		"maven-hosted/com/acme/lib/1.3.0/lib-1.3.0.jar",
	)
	got, ok := New().generateMetadata(c)
	if !ok {
		t.Fatal("expected metadata to generate")
	}
	golden.Assert(t, got, "metadata_two_versions.xml")
}

func TestGenerateMetadata_EmptyArtifact(t *testing.T) {
	c := ctxWith(t, "com/acme/none/maven-metadata.xml") // no blobs
	if _, ok := New().generateMetadata(c); ok {
		t.Fatal("expected no metadata for absent artifact")
	}
}

// A requested .sha1 sidecar with no stored file is synthesized from the base
// artifact; verify it matches a direct checksum of the same bytes.
func TestChecksumHelpersMatch(t *testing.T) {
	body := []byte("artifact-bytes")
	if blob.SHA1(body) != checksumOf("sha1", body) {
		t.Fatal("sha1 helper mismatch")
	}
	if blob.MD5(body) != checksumOf("md5", body) {
		t.Fatal("md5 helper mismatch")
	}
	if blob.SHA256(body) != checksumOf("sha256", body) {
		t.Fatal("sha256 helper mismatch")
	}
}

// TestGroup_MetadataMerge verifies that a group repo merges versions from all
// member repos and deduplicates overlapping versions.
func TestGroup_MetadataMerge(t *testing.T) {
	b, m := sharedStores(t)

	seed := func(repoName, version string) {
		key := repoName + "/com/acme/lib/" + version + "/lib-" + version + ".jar"
		if _, err := b.Put(key, strings.NewReader("bytes")); err != nil {
			t.Fatal(err)
		}
	}
	seed("maven-a", "1.0.0")
	seed("maven-a", "1.1.0")
	seed("maven-b", "1.1.0") // duplicate; group should deduplicate
	seed("maven-b", "2.0.0")

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "maven-a", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-b", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-group", Format: "maven", Kind: repo.Group, Members: []string{"maven-a", "maven-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	groupRepo, _ := mgr.Get("maven-group")
	c := &format.Context{
		Repo:  groupRepo,
		Blob:  b,
		Meta:  m,
		Sub:   "com/acme/lib/maven-metadata.xml",
		Repos: mgr,
	}

	xml, ok := New().groupMetadataBytes(c)
	if !ok {
		t.Fatal("expected group metadata to generate")
	}
	body := string(xml)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if !strings.Contains(body, "<version>"+v+"</version>") {
			t.Errorf("expected version %s in merged metadata", v)
		}
	}
	// 1.1.0 should appear exactly once (deduplicated).
	if count := strings.Count(body, "<version>1.1.0</version>"); count != 1 {
		t.Errorf("1.1.0 appears %d times, want 1", count)
	}
}

// --- SNAPSHOT tests --------------------------------------------------------

func TestExtractTimestamp(t *testing.T) {
	cases := []struct {
		value string
		ts    string
		bn    int
		ok    bool
	}{
		{"1.0-20240115.123456-1", "20240115.123456", 1, true},
		{"1.0.0-20240115.123456-42", "20240115.123456", 42, true},
		{"1.0-SNAPSHOT", "", 0, false},
		{"1.0", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range cases {
		ts, bn, ok := extractTimestamp(tc.value)
		if ok != tc.ok || ts != tc.ts || bn != tc.bn {
			t.Errorf("extractTimestamp(%q): got (%q,%d,%v) want (%q,%d,%v)",
				tc.value, ts, bn, ok, tc.ts, tc.bn, tc.ok)
		}
	}
}

func TestIsSnapshotMetaPath(t *testing.T) {
	cases := []struct {
		sub  string
		want bool
	}{
		{"com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml", true},
		{"com/acme/lib/maven-metadata.xml", false},
		{"com/acme/lib/1.0/maven-metadata.xml", false},
	}
	for _, tc := range cases {
		if got := isSnapshotMetaPath(tc.sub); got != tc.want {
			t.Errorf("isSnapshotMetaPath(%q) = %v, want %v", tc.sub, got, tc.want)
		}
	}
}

func TestSnapshotMetadata_Golden(t *testing.T) {
	c := ctxWith(t, "")
	putArtifact(t, c, "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20240115.123456-1.jar")
	putArtifact(t, c, "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20240115.123456-1.pom")

	c.Sub = "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml"
	xml, ok := New().generateSnapshotMetadata(c)
	if !ok {
		t.Fatal("expected snapshot metadata to generate")
	}
	golden.Assert(t, xml, "snapshot_metadata.xml")
}

func TestSnapshotMetadata_BuildNumberAdvances(t *testing.T) {
	c := ctxWith(t, "")
	putArtifact(t, c, "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20240115.123456-1.jar")
	putArtifact(t, c, "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20240116.234567-2.jar")

	c.Sub = "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml"
	xml, ok := New().generateSnapshotMetadata(c)
	if !ok {
		t.Fatal("expected metadata")
	}
	body := string(xml)
	if !strings.Contains(body, "<buildNumber>2</buildNumber>") {
		t.Error("expected build number 2 (latest) in metadata")
	}
	if !strings.Contains(body, "20240116.234567") {
		t.Error("expected latest timestamp in metadata")
	}
	// Only one jar entry — latest build wins.
	if count := strings.Count(body, "<extension>jar</extension>"); count != 1 {
		t.Errorf("expected 1 jar snapshotVersion entry, got %d", count)
	}
}

func TestSnapshotMetadata_NotFound(t *testing.T) {
	c := ctxWith(t, "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml")
	if _, ok := New().generateSnapshotMetadata(c); ok {
		t.Fatal("expected no snapshot metadata for unseen path")
	}
}

func TestParseArtifactFilename(t *testing.T) {
	cases := []struct {
		filename   string
		artifactID string
		ext        string
		value      string
		ok         bool
	}{
		{"lib-1.0-20240115.123456-1.jar", "lib", "jar", "1.0-20240115.123456-1", true},
		{"lib-1.0-SNAPSHOT.jar", "lib", "jar", "1.0-SNAPSHOT", true},
		{"other-1.0.jar", "lib", "", "", false}, // wrong prefix
		{"lib.jar", "lib", "", "", false},        // no dash-version
	}
	for _, tc := range cases {
		ext, val, ok := parseArtifactFilename(tc.filename, tc.artifactID)
		if ok != tc.ok || ext != tc.ext || val != tc.value {
			t.Errorf("parseArtifactFilename(%q, %q): got (%q,%q,%v) want (%q,%q,%v)",
				tc.filename, tc.artifactID, ext, val, ok, tc.ext, tc.value, tc.ok)
		}
	}
}

// TestSnapshotMetadata_ConcurrentPuts verifies that concurrent artifact PUTs
// to the same SNAPSHOT version directory produce complete metadata with no
// lost entries.  Before the fix, a shared snapshotMeta record was updated
// via read-modify-write; last writer won and earlier entries were lost.
func TestSnapshotMetadata_ConcurrentPuts(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(dir + "/b")
	m, _ := meta.NewFS(dir + "/m")

	const N = 10 // different extensions / artifact types
	exts := make([]string, N)
	for i := range exts {
		exts[i] = fmt.Sprintf("ext%d", i)
	}

	var wg sync.WaitGroup
	for _, ext := range exts {
		wg.Add(1)
		go func(ext string) {
			defer wg.Done()
			// Each goroutine publishes an artifact with a distinct extension
			// to the same SNAPSHOT directory. These must all be preserved.
			filename := fmt.Sprintf("lib-1.0-20240115.123456-1.%s", ext)
			sub := "com/acme/lib/1.0-SNAPSHOT/" + filename
			c := &format.Context{
				Repo: repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
				Blob: b, Meta: m, Sub: sub,
			}
			b.Put(c.Key(sub), strings.NewReader("bytes")) //nolint:errcheck
			New().maybeUpdateSnapshotMeta(c)
		}(ext)
	}
	wg.Wait()

	c := &format.Context{
		Repo: repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
		Blob: b, Meta: m,
		Sub: "com/acme/lib/1.0-SNAPSHOT/maven-metadata.xml",
	}
	xml, ok := New().generateSnapshotMetadata(c)
	if !ok {
		t.Fatal("expected snapshot metadata to generate")
	}
	body := string(xml)
	for _, ext := range exts {
		if !strings.Contains(body, "<extension>"+ext+"</extension>") {
			t.Errorf("extension %q missing from metadata — concurrent write was lost", ext)
		}
	}
}

func TestGradleModuleContentType(t *testing.T) {
	got := contentType("lib-1.0.module")
	want := "application/vnd.gradle.module+json"
	if got != want {
		t.Errorf("contentType(.module) = %q, want %q", got, want)
	}
}

func TestFormat_Maven(t *testing.T) {
	if got := New().Format(); got != "maven" {
		t.Fatalf("Format() = %q, want maven", got)
	}
}

func TestChecksumOf_UnknownExt(t *testing.T) {
	if got := checksumOf("unknown", []byte("x")); got != "" {
		t.Fatalf("expected empty string for unknown ext, got %q", got)
	}
}

// TestServe_Proxy_PomWithParent exercises the POM branch of proxyFetch and
// prefetchParentPOM by serving a POM that references a parent POM.
func TestServe_Proxy_PomWithParent(t *testing.T) {
	parentPOM := `<project><groupId>com.parent</groupId><artifactId>parent-pom</artifactId><version>1.0</version></project>`
	childPOM := `<project>
		<parent><groupId>com.parent</groupId><artifactId>parent-pom</artifactId><version>1.0</version></parent>
		<groupId>com.acme</groupId><artifactId>lib</artifactId><version>1.0.0</version>
	</project>`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "parent-pom") {
			io.WriteString(w, parentPOM)
		} else {
			io.WriteString(w, childPOM)
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{
			Name: "maven-proxy", Format: "maven", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}

	// Fetching the child POM triggers prefetchParentPOM for the parent.
	rw := serveReq(c, http.MethodGet, "com/acme/lib/1.0.0/lib-1.0.0.pom", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy POM: got %d\n%s", rw.Code, rw.Body)
	}
	if !strings.Contains(rw.Body.String(), "com.acme") {
		t.Fatalf("unexpected body: %s", rw.Body)
	}
	// Parent POM must have been prefetched into the blob store.
	parentKey := "maven-proxy/com/parent/parent-pom/1.0/parent-pom-1.0.pom"
	if _, err := b.Get(parentKey); err != nil {
		t.Fatalf("parent POM not prefetched: %v", err)
	}
}

// TestServe_Proxy_PomWithParent_AlreadyCached checks that prefetchParentPOM
// skips the fetch when the parent is already in the blob store.
func TestServe_Proxy_PomAlreadyCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		childPOM := `<project>
			<parent><groupId>com.parent</groupId><artifactId>parent-pom</artifactId><version>1.0</version></parent>
			<groupId>com.acme</groupId><artifactId>lib</artifactId><version>2.0.0</version>
		</project>`
		io.WriteString(w, childPOM)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	// Pre-seed the parent in blob so prefetchParentPOM takes the early-return path.
	b.Put("maven-proxy/com/parent/parent-pom/1.0/parent-pom-1.0.pom",
		strings.NewReader("<project/>"))

	c := &format.Context{
		Repo: repo.Repository{
			Name: "maven-proxy", Format: "maven", Kind: repo.Proxy,
			Upstream: upstream.URL,
		},
		Blob: b, Meta: m,
		HTTP: upstream.Client(),
	}
	rw := serveReq(c, http.MethodGet, "com/acme/lib/2.0.0/lib-2.0.0.pom", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("proxy POM cached: got %d\n%s", rw.Code, rw.Body)
	}
}
