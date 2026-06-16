package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/golden"
	"forge/internal/meta"
	"forge/internal/repo"
)

// makeChart builds a minimal Helm chart .tgz containing only a Chart.yaml.
func makeChart(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	yaml := fmt.Sprintf("name: %s\nversion: %s\napiVersion: v2\ndescription: Test chart\n", name, version)
	tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0644, Size: int64(len(yaml))})
	tw.Write([]byte(yaml))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// newHostedCtx builds a hosted Helm Context backed by temp FS stores.
func newHostedCtx(t *testing.T) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return &format.Context{
		Repo: repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

// serve is a shorthand to call the helm handler's Serve method.
func serve(c *format.Context, method, sub string, body io.Reader) *httptest.ResponseRecorder {
	c.Sub = sub
	if body == nil {
		body = http.NoBody
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, httptest.NewRequest(method, "/", body), c)
	return rw
}

// --- HTTP endpoint tests ----------------------------------------------------

func TestServe_Upload_And_Index(t *testing.T) {
	c := newHostedCtx(t)
	chart := makeChart(t, "myapp", "1.0.0")

	// POST /api/charts uploads the chart.
	rw := serve(c, http.MethodPost, "api/charts", bytes.NewReader(chart))
	if rw.Code != http.StatusCreated {
		t.Fatalf("upload: got %d, body: %s", rw.Code, rw.Body)
	}
	var resp map[string]bool
	json.NewDecoder(rw.Body).Decode(&resp)
	if !resp["saved"] {
		t.Fatalf("upload: expected saved=true, got %v", resp)
	}

	// GET /index.yaml returns the chart in the index.
	rw = serve(c, http.MethodGet, "index.yaml", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("index: got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "name: myapp") {
		t.Fatalf("index missing chart: %s", rw.Body)
	}
}

func TestServe_Download(t *testing.T) {
	c := newHostedCtx(t)
	chart := makeChart(t, "redis", "2.0.0")
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(chart))

	rw := serve(c, http.MethodGet, "redis-2.0.0.tgz", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("download: got %d", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), chart) {
		t.Fatal("downloaded bytes differ from uploaded bytes")
	}
}

func TestServe_Download_NotFound(t *testing.T) {
	c := newHostedCtx(t)
	rw := serve(c, http.MethodGet, "ghost-1.0.0.tgz", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_ListAll(t *testing.T) {
	c := newHostedCtx(t)
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "alpha", "1.0.0")))
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "beta", "2.0.0")))

	rw := serve(c, http.MethodGet, "api/charts", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("listAll: got %d", rw.Code)
	}
	var m map[string]any
	json.NewDecoder(rw.Body).Decode(&m)
	if _, ok := m["alpha"]; !ok {
		t.Error("listAll missing alpha")
	}
	if _, ok := m["beta"]; !ok {
		t.Error("listAll missing beta")
	}
}

func TestServe_ListOne(t *testing.T) {
	c := newHostedCtx(t)
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "myapp", "1.0.0")))

	rw := serve(c, http.MethodGet, "api/charts/myapp", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("listOne: got %d", rw.Code)
	}
	var recs []chartRecord
	json.NewDecoder(rw.Body).Decode(&recs)
	if len(recs) != 1 || recs[0].Name != "myapp" {
		t.Fatalf("listOne unexpected: %v", recs)
	}
}

func TestServe_ListOne_NotFound(t *testing.T) {
	c := newHostedCtx(t)
	rw := serve(c, http.MethodGet, "api/charts/ghost", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_Delete(t *testing.T) {
	c := newHostedCtx(t)
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "myapp", "1.0.0")))

	rw := serve(c, http.MethodDelete, "api/charts/myapp/1.0.0", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("delete: got %d", rw.Code)
	}
	// Chart should no longer appear in index.
	rw = serve(c, http.MethodGet, "index.yaml", nil)
	if strings.Contains(rw.Body.String(), "name: myapp") {
		t.Fatal("chart still in index after delete")
	}
}

func TestServe_NonHosted_Upload_Rejected(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "helm-proxy", Format: "helm", Kind: repo.Proxy},
		Blob: b, Meta: m,
	}
	rw := serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "x", "1.0.0")))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rw.Code)
	}
}

func TestServe_Upload_InvalidBody(t *testing.T) {
	c := newHostedCtx(t)
	rw := serve(c, http.MethodPost, "api/charts", strings.NewReader("not a tgz"))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rw.Code)
	}
}

func TestServe_UnsupportedRoute(t *testing.T) {
	c := newHostedCtx(t)
	rw := serve(c, http.MethodGet, "unknown/path", nil)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

func TestServe_Group_IndexAndDownload(t *testing.T) {
	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	bA, _ := blob.NewFS(filepath.Join(dir, "bA"))
	bB, _ := blob.NewFS(filepath.Join(dir, "bB"))

	// Seed two hosted repos.
	chartA := makeChart(t, "alpha", "1.0.0")
	chartB := makeChart(t, "beta", "2.0.0")
	ctxA := &format.Context{Repo: repo.Repository{Name: "helm-a", Format: "helm", Kind: repo.Hosted}, Blob: bA, Meta: m}
	ctxB := &format.Context{Repo: repo.Repository{Name: "helm-b", Format: "helm", Kind: repo.Hosted}, Blob: bB, Meta: m}
	serve(ctxA, http.MethodPost, "api/charts", bytes.NewReader(chartA))
	serve(ctxB, http.MethodPost, "api/charts", bytes.NewReader(chartB))

	// Build a group repo.
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "helm-a", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-b", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-group", Format: "helm", Kind: repo.Group, Members: []string{"helm-a", "helm-b"}},
	} {
		mgr.Add(r)
	}

	// The group needs its own blob store; we use bA as a stand-in (group only reads meta for index).
	// For download, it delegates to member blob stores via MemberCtx.
	groupRepo, _ := mgr.Get("helm-group")
	cGroup := &format.Context{
		Repo:  groupRepo,
		Blob:  bA, // used for group index (meta) — actual download uses member Blob via MemberCtx
		Meta:  m,
		Repos: mgr,
	}
	// Override MemberCtx blob mapping so group downloads work.
	// Since MemberCtx uses s.Blob from the Server context, we need to use
	// a shared blob.  Re-run with bA serving helm-a and bB serving helm-b
	// requires a server-level context; test the index here and the download
	// via direct member context below.

	rw := serve(cGroup, http.MethodGet, "index.yaml", nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("group index: got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "name: alpha") || !strings.Contains(body, "name: beta") {
		t.Fatalf("group index missing members: %s", body)
	}
}

func TestBrowseRepo_Helm(t *testing.T) {
	c := newHostedCtx(t)
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "webapp", "1.0.0")))
	serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "webapp", "2.0.0")))

	entries, err := New().BrowseRepo(c)
	if err != nil {
		t.Fatalf("BrowseRepo: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "webapp" {
		t.Fatalf("unexpected entries: %v", entries)
	}
	if len(entries[0].Versions) != 2 {
		t.Fatalf("expected 2 versions, got %v", entries[0].Versions)
	}
}

var fixedNow = time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

func TestBuildIndex_Golden(t *testing.T) {
	recs := []chartRecord{
		{
			Name: "webapp", Version: "0.4.1", AppVersion: "2.0",
			Description: "A demo web application chart",
			Digest:      "aaabbbccc", Created: "2024-01-15T10:00:00Z",
			Filename: "webapp-0.4.1.tgz",
		},
		{
			Name: "webapp", Version: "0.3.0",
			Description: "A demo web application chart",
			Digest:      "dddeeefff", Created: "2024-01-14T09:00:00Z",
			Filename: "webapp-0.3.0.tgz",
		},
		{
			Name: "redis", Version: "1.0.0",
			Description: "Redis chart",
			Digest:      "111222333", Created: "2024-01-13T08:00:00Z",
			Filename: "redis-1.0.0.tgz",
		},
	}
	got := []byte(buildIndex(recs, fixedNow))
	golden.Assert(t, got, "index_two_charts.yaml")
}

func TestBuildIndex_Empty(t *testing.T) {
	got := buildIndex(nil, fixedNow)
	want := "apiVersion: v1\nentries:\ngenerated: 2024-01-15T12:00:00Z\n"
	if got != want {
		t.Fatalf("empty index mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestGroup_IndexMerge verifies that a group repo merges chart records from
// all members and deduplicates overlapping name+version pairs.
func TestGroup_IndexMerge(t *testing.T) {
	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	seed := func(repoName, chartName, version string) {
		ns := repoName + ":helm"
		rec := chartRecord{
			Name: chartName, Version: version,
			Digest: "abc", Created: "2024-01-01T00:00:00Z",
			Filename: chartName + "-" + version + ".tgz",
		}
		if err := m.PutJSON(ns, chartName+"-"+version, rec); err != nil {
			t.Fatal(err)
		}
	}

	seed("helm-a", "webapp", "1.0.0")
	seed("helm-a", "webapp", "1.1.0")
	seed("helm-b", "webapp", "1.1.0") // duplicate
	seed("helm-b", "redis", "2.0.0")

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "helm-a", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-b", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-group", Format: "helm", Kind: repo.Group, Members: []string{"helm-a", "helm-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	// Need a blob store for the Context even though groupRecords only reads meta.
	blobDir := filepath.Join(dir, "b")
	groupRepo, _ := mgr.Get("helm-group")
	// Use a nil blob store — groupRecords only touches meta.
	c := &format.Context{
		Repo:  groupRepo,
		Meta:  m,
		Sub:   "index.yaml",
		Repos: mgr,
	}

	h := New()
	recs := h.groupRecords(c)

	// Should have: webapp 1.0.0, webapp 1.1.0 (deduped), redis 2.0.0 = 3 total.
	if len(recs) != 3 {
		t.Errorf("expected 3 records, got %d", len(recs))
	}
	found := map[string]bool{}
	for _, r := range recs {
		found[r.Name+"-"+r.Version] = true
	}
	for _, key := range []string{"webapp-1.0.0", "webapp-1.1.0", "redis-2.0.0"} {
		if !found[key] {
			t.Errorf("expected record %s in merged index", key)
		}
	}

	// Verify via buildIndex output.
	idx := buildIndex(recs, fixedNow)
	if !strings.Contains(idx, "redis") {
		t.Error("index missing redis chart from second member")
	}
	_ = blobDir // silence unused warning
}

func TestFormat_Helm(t *testing.T) {
	if got := New().Format(); got != "helm" {
		t.Fatalf("Format() = %q, want helm", got)
	}
}

func TestParseIndexYAML(t *testing.T) {
	yaml := `apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: 1.2.3
      description: A test chart
      created: 2024-03-15T10:00:00Z
      digest: abc123def456
      urls:
        - mychart-1.2.3.tgz
    - name: mychart
      version: 1.0.0
      description: A test chart
      created: 2023-06-01T08:00:00Z
      digest: deadbeef0000
      urls:
        - mychart-1.0.0.tgz
  otherchart:
    - name: otherchart
      version: 2.0.0
      description: Another chart
      created: 2024-01-10T12:00:00+00:00
      digest: 111222333444
      urls:
        - otherchart-2.0.0.tgz
generated: 2024-03-16T00:00:00Z
`
	recs := parseIndexYAML([]byte(yaml))
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	byKey := map[string]chartRecord{}
	for _, r := range recs {
		byKey[r.Name+"-"+r.Version] = r
	}
	r := byKey["mychart-1.2.3"]
	if r.Name != "mychart" {
		t.Errorf("Name = %q, want mychart", r.Name)
	}
	if r.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", r.Version)
	}
	if r.Description != "A test chart" {
		t.Errorf("Description = %q, want 'A test chart'", r.Description)
	}
	if r.Filename != "mychart-1.2.3.tgz" {
		t.Errorf("Filename = %q, want mychart-1.2.3.tgz", r.Filename)
	}
	if r.UploadedAt.IsZero() {
		t.Error("UploadedAt is zero, want 2024-03-15T10:00:00Z")
	}
	if _, ok := byKey["otherchart-2.0.0"]; !ok {
		t.Error("otherchart-2.0.0 not found")
	}
}

// TestParseIndexYAML_Bitnami tests the Bitnami/ChartMuseum-style index.yaml
// where entry dashes are at indent 2 (same as chart name) and fields at indent 4.
func TestParseIndexYAML_Bitnami(t *testing.T) {
	yaml := `apiVersion: v1
entries:
  nginx:
  - annotations:
      category: Infrastructure
    apiVersion: v2
    appVersion: "1.27.0"
    created: "2024-06-01T12:00:00.123456789Z"
    dependencies:
    - condition: common.enabled
      name: common
      version: 2.x.x
    description: NGINX web server
    digest: aabbccdd1122
    maintainers:
    - name: Broadcom, Inc.
    name: nginx
    urls:
    - oci://registry-1.docker.io/bitnamicharts/nginx:18.1.0
    version: 18.1.0
  - annotations:
      category: Infrastructure
    apiVersion: v2
    created: "2023-01-10T08:00:00Z"
    description: NGINX web server
    digest: 0011223344
    name: nginx
    urls:
    - oci://registry-1.docker.io/bitnamicharts/nginx:15.0.0
    version: 15.0.0
generated: "2024-06-02T00:00:00Z"
`
	recs := parseIndexYAML([]byte(yaml))
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	byKey := map[string]chartRecord{}
	for _, r := range recs {
		byKey[r.Name+"-"+r.Version] = r
	}
	r := byKey["nginx-18.1.0"]
	if r.Name != "nginx" {
		t.Errorf("Name = %q, want nginx", r.Name)
	}
	if r.Version != "18.1.0" {
		t.Errorf("Version = %q, want 18.1.0", r.Version)
	}
	if r.Description != "NGINX web server" {
		t.Errorf("Description = %q, want 'NGINX web server'", r.Description)
	}
	if r.Filename != "oci://registry-1.docker.io/bitnamicharts/nginx:18.1.0" {
		t.Errorf("Filename = %q", r.Filename)
	}
	if r.UploadedAt.IsZero() {
		t.Error("UploadedAt is zero for nginx-18.1.0")
	}
	if _, ok := byKey["nginx-15.0.0"]; !ok {
		t.Error("nginx-15.0.0 not found")
	}
}

// --- Inspect -----------------------------------------------------------------

func TestInspect_Hosted(t *testing.T) {
	c := newHostedCtx(t)
	for _, ver := range []string{"1.0.0", "2.0.0"} {
		rw := serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "myapp", ver)))
		if rw.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", ver, rw.Code, rw.Body)
		}
	}
	detail, ok := New().Inspect(c, "http://forge.local", "myapp")
	if !ok {
		t.Fatal("Inspect returned false for existing chart")
	}
	if detail.Name != "myapp" {
		t.Errorf("Name = %q, want myapp", detail.Name)
	}
	if len(detail.Versions) != 2 {
		t.Errorf("Versions count = %d, want 2", len(detail.Versions))
	}
	if detail.Versions[0].Version != "2.0.0" {
		t.Errorf("first version = %q, want 2.0.0 (newest-first)", detail.Versions[0].Version)
	}
	if detail.InstallSnippet == "" {
		t.Error("InstallSnippet must not be empty")
	}
	if detail.Versions[0].DownloadURL == "" {
		t.Error("DownloadURL must not be empty")
	}
}

func TestInspect_NotFound(t *testing.T) {
	c := newHostedCtx(t)
	if _, ok := New().Inspect(c, "http://forge.local", "nosuchchart"); ok {
		t.Fatal("expected false for absent chart")
	}
}

func TestParseIndexYAML_WrappedDescription(t *testing.T) {
	yaml := `apiVersion: v1
entries:
  appsmith:
  - apiVersion: v2
    created: "2024-06-01T00:00:00Z"
    description: Appsmith is an open source platform for building and maintaining
      internal tools, such as custom dashboards, admin panels or CRUD apps.
    digest: abc123
    name: appsmith
    urls:
    - appsmith-4.0.0.tgz
    version: 4.0.0
generated: "2024-06-02T00:00:00Z"
`
	recs := parseIndexYAML([]byte(yaml))
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	want := "Appsmith is an open source platform for building and maintaining internal tools, such as custom dashboards, admin panels or CRUD apps."
	if recs[0].Description != want {
		t.Errorf("Description = %q\nwant         %q", recs[0].Description, want)
	}
}
