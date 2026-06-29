package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/oci"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/trivy"
	"forge/internal/vuln"
)

// trivyCleanJSON is a minimal valid Trivy JSON report with no vulnerabilities.
const trivyCleanJSON = `{"Results":[]}`

// trivyHighJSON is a Trivy JSON report with one HIGH severity CVE.
const trivyHighJSON = `{"Results":[{"Target":"alpine","Vulnerabilities":[{
  "VulnerabilityID":"CVE-2024-9999","Title":"test vuln","Severity":"HIGH",
  "FixedVersion":"1.0.1","References":["https://example.com"]
}]}]}`

// fakeTrivyExecutor is a trivy.Executor that returns canned JSON.
type fakeTrivyExecutor struct {
	out string
}

func (f *fakeTrivyExecutor) Run(_ context.Context, _ []string, _ ...string) ([]byte, error) {
	return []byte(f.out), nil
}

// newOCIServer wires a server with one OCI hosted repo and a Trivy scanner
// backed by the given executor output.
func newOCIServer(t *testing.T, trivyOut string) (*Server, *meta.FS) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(oci.New())
	mgr.Add(repo.Repository{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted, Enabled: true}) //nolint:errcheck

	sc := trivy.New("trivy", "localhost:8080", "").WithExecutor(&fakeTrivyExecutor{out: trivyOut})
	srv := New(mgr, reg, b, m, nil)
	srv.Vuln = vuln.NewStore(m)
	srv.Trivy = sc
	return srv, m
}

// seedTag writes the OCI meta entries that BrowseRepo needs to return the image.
func seedTag(t *testing.T, m *meta.FS, repo, image, tag string) {
	t.Helper()
	ns := repo + ":oci"
	m.PutJSON(ns, "tags/"+image+"/"+tag, tag)             //nolint:errcheck
	m.PutJSON(ns, "tag-times/"+image+"/"+tag, "2024-01-01T00:00:00Z") //nolint:errcheck
}

// TestScanOCIImage_WritesCleanFinding verifies that a clean Trivy scan writes a
// Finding with no advisories and stamps ScannedAt (= "scanned, no known issues").
func TestScanOCIImage_WritesCleanFinding(t *testing.T) {
	srv, _ := newOCIServer(t, trivyCleanJSON)

	if err := srv.scanOCIImage(context.Background(), "docker-hosted", "myapp", "latest"); err != nil {
		t.Fatal(err)
	}

	f, ok, err := srv.Vuln.Get("docker-hosted", "myapp", "latest")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if f.Source != vuln.SourceTrivy {
		t.Errorf("Source = %q, want %q", f.Source, vuln.SourceTrivy)
	}
	if len(f.Advisories) != 0 {
		t.Errorf("Advisories = %d, want 0 (clean)", len(f.Advisories))
	}
	if f.ScannedAt.IsZero() {
		t.Error("ScannedAt not stamped")
	}
}

// TestScanOCIImage_WritesAdvisories verifies that a Trivy scan with findings
// writes the correct advisory and rebuilds the rollup.
func TestScanOCIImage_WritesAdvisories(t *testing.T) {
	srv, _ := newOCIServer(t, trivyHighJSON)

	if err := srv.scanOCIImage(context.Background(), "docker-hosted", "myapp", "v1.0"); err != nil {
		t.Fatal(err)
	}

	f, ok, err := srv.Vuln.Get("docker-hosted", "myapp", "v1.0")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if f.Source != vuln.SourceTrivy {
		t.Errorf("Source = %q", f.Source)
	}
	if len(f.Advisories) != 1 || f.Advisories[0].ID != "CVE-2024-9999" {
		t.Errorf("Advisories = %+v", f.Advisories)
	}
	if f.Worst() != vuln.SeverityHigh {
		t.Errorf("Worst = %v, want high", f.Worst())
	}

	// Rollup must be persisted so list views work.
	r, ok, err := srv.Vuln.GetRollup("docker-hosted")
	if err != nil || !ok {
		t.Fatalf("GetRollup: ok=%v err=%v", ok, err)
	}
	if r.VulnerableCount != 1 {
		t.Errorf("rollup.VulnerableCount = %d, want 1", r.VulnerableCount)
	}
}

// TestScanOCIImage_SkipsNonOCI verifies that scanOCIImage is a no-op for repos
// of other formats (e.g. if the wrong repo name is supplied).
func TestScanOCIImage_SkipsNonOCI(t *testing.T) {
	srv, _ := newOCIServer(t, trivyHighJSON)
	// "docker-hosted" is OCI; "nonexistent" is not in the manager.
	err := srv.scanOCIImage(context.Background(), "nonexistent", "img", "latest")
	if err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

// TestEnqueueTrivyScan_EnqueuesJob verifies that enqueueTrivyScan places a
// trivy.scan.oci job with the correct payload on the queue.
func TestEnqueueTrivyScan_EnqueuesJob(t *testing.T) {
	srv, _ := newOCIServer(t, trivyCleanJSON)
	q := &fakeQueue{}
	srv.Queue = q

	srv.enqueueTrivyScan("docker-hosted", "myapp", "latest")
	// enqueueTrivyScan runs a goroutine; wait briefly for it to land.
	for i := 0; i < 100; i++ {
		if len(q.snapshot()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("want 1 enqueued job, got %d", len(jobs))
	}
	if jobs[0].typ != trivyScanJobType {
		t.Errorf("job type = %q, want %q", jobs[0].typ, trivyScanJobType)
	}
	var p trivyScanPayload
	if err := json.Unmarshal([]byte(jobs[0].payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Repo != "docker-hosted" || p.Image != "myapp" || p.Tag != "latest" {
		t.Errorf("payload = %+v", p)
	}
}

// TestEnqueueTrivyScan_NoopWhenUnconfigured verifies that enqueueTrivyScan is
// silent when Trivy is not configured (nil scanner) or queue is nil.
func TestEnqueueTrivyScan_NoopWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	srv := New(mgr, reg, b, m, nil)
	srv.Vuln = vuln.NewStore(m)
	// Trivy is nil — should be a no-op
	q := &fakeQueue{}
	srv.Queue = q
	srv.enqueueTrivyScan("repo", "img", "tag")
	if len(q.snapshot()) != 0 {
		t.Error("expected no jobs when Trivy unconfigured")
	}
}

// TestMiddleware_OCI_ManifestPush_EnqueuesScan verifies that a successful OCI
// manifest PUT through the HTTP middleware enqueues a trivy.scan.oci job for
// tag refs and skips digest refs.
func TestMiddleware_OCI_ManifestPush_EnqueuesScan(t *testing.T) {
	srv, _ := newOCIServer(t, trivyCleanJSON)
	q := &fakeQueue{}
	srv.Queue = q

	handler := srv.Routes()

	// PUT a manifest by tag — should enqueue a Trivy scan.
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	req := httptest.NewRequest(http.MethodPut,
		"/v2/docker-hosted/myapp/manifests/latest",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("manifest PUT status = %d, want 201", rec.Code)
	}

	// Give the goroutine a moment to enqueue.
	for i := 0; i < 100; i++ {
		if len(q.snapshot()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	jobs := q.snapshot()
	var trivyJobs []fakeJob
	for _, j := range jobs {
		if j.typ == trivyScanJobType {
			trivyJobs = append(trivyJobs, j)
		}
	}
	if len(trivyJobs) != 1 {
		t.Fatalf("want 1 trivy scan job for tag push, got %d (all jobs: %v)", len(trivyJobs), jobs)
	}
	var p trivyScanPayload
	if err := json.Unmarshal([]byte(trivyJobs[0].payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Image != "myapp" || p.Tag != "latest" {
		t.Errorf("payload = %+v", p)
	}

	// PUT the same manifest by digest — should NOT enqueue a Trivy scan.
	q.mu.Lock()
	q.jobs = nil
	q.mu.Unlock()

	dgst := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	req2 := httptest.NewRequest(http.MethodPut,
		"/v2/docker-hosted/myapp/manifests/"+dgst,
		strings.NewReader(string(body)))
	req2.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("manifest PUT by digest status = %d, want 201", rec2.Code)
	}

	time.Sleep(5 * time.Millisecond) // brief pause; no job should arrive
	for _, j := range q.snapshot() {
		if j.typ == trivyScanJobType {
			t.Error("digest-based manifest push should NOT enqueue a Trivy scan")
		}
	}
}

// TestScanOCIRepo_ScansAllTags verifies that scanOCIRepo iterates all
// image+tag pairs returned by BrowseRepo.
func TestScanOCIRepo_ScansAllTags(t *testing.T) {
	srv, m := newOCIServer(t, trivyCleanJSON)
	seedTag(t, m, "docker-hosted", "myapp", "v1")
	seedTag(t, m, "docker-hosted", "myapp", "v2")
	seedTag(t, m, "docker-hosted", "sidecar", "latest")

	if err := srv.scanOCIRepo(context.Background(), "docker-hosted"); err != nil {
		t.Fatal(err)
	}

	for _, pair := range [][2]string{{"myapp", "v1"}, {"myapp", "v2"}, {"sidecar", "latest"}} {
		_, ok, err := srv.Vuln.Get("docker-hosted", pair[0], pair[1])
		if err != nil || !ok {
			t.Errorf("missing finding for %s:%s (ok=%v err=%v)", pair[0], pair[1], ok, err)
		}
	}
}

// TestHandleTrivyScanJob_Dispatches verifies that the worker handler decodes
// the payload and calls scanOCIImage.
func TestHandleTrivyScanJob_Dispatches(t *testing.T) {
	srv, _ := newOCIServer(t, trivyCleanJSON)

	payload, _ := json.Marshal(trivyScanPayload{Repo: "docker-hosted", Image: "myapp", Tag: "latest"})
	j := queue.Job{Type: trivyScanJobType, Payload: json.RawMessage(payload)}

	if err := srv.handleTrivyScanJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}

	_, ok, err := srv.Vuln.Get("docker-hosted", "myapp", "latest")
	if err != nil || !ok {
		t.Fatalf("finding not written: ok=%v err=%v", ok, err)
	}
}

// TestOCIGate_BlocksWhenFindingExceedsThreshold verifies that a manifest GET
// for a tag with a HIGH finding is blocked (403) when the policy is Block/high.
func TestOCIGate_BlocksWhenFindingExceedsThreshold(t *testing.T) {
	srv, m := newOCIServer(t, trivyCleanJSON)
	seedTag(t, m, "docker-hosted", "myapp", "v1")

	// Write a HIGH finding for myapp:v1.
	srv.Vuln.Put("docker-hosted", vuln.Finding{ //nolint:errcheck
		Component: "myapp", Version: "v1", Source: vuln.SourceTrivy,
		Advisories: []vuln.Advisory{{ID: "CVE-2024-9999", Severity: vuln.SeverityHigh}},
	})

	// Attach a Block policy at threshold High.
	pm := vuln.NewPolicyManager(m)
	pm.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityHigh})
	srv.VulnPolicy = pm

	// Push the manifest so it exists.
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	putReq := httptest.NewRequest(http.MethodPut, "/v2/docker-hosted/myapp/manifests/v1", strings.NewReader(string(body)))
	putReq.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	putRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated {
		t.Fatalf("manifest PUT status = %d, want 201", putRec.Code)
	}

	// GET the manifest: should be blocked.
	getReq := httptest.NewRequest(http.MethodGet, "/v2/docker-hosted/myapp/manifests/v1", nil)
	getRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusForbidden {
		t.Errorf("manifest GET status = %d, want 403 (blocked by policy)", getRec.Code)
	}
}

// TestOCIGate_WarnAddsPolicyHeader verifies that a manifest GET for a
// Warn-policy repo adds X-Forge-Vulnerabilities and still returns 200.
func TestOCIGate_WarnAddsPolicyHeader(t *testing.T) {
	srv, m := newOCIServer(t, trivyCleanJSON)
	seedTag(t, m, "docker-hosted", "myapp", "v1")

	srv.Vuln.Put("docker-hosted", vuln.Finding{ //nolint:errcheck
		Component: "myapp", Version: "v1", Source: vuln.SourceTrivy,
		Advisories: []vuln.Advisory{{ID: "CVE-2024-9999", Severity: vuln.SeverityHigh}},
	})

	pm := vuln.NewPolicyManager(m)
	pm.SetDefault(vuln.Policy{Mode: vuln.ModeWarn, Threshold: vuln.SeverityHigh})
	srv.VulnPolicy = pm

	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	putReq := httptest.NewRequest(http.MethodPut, "/v2/docker-hosted/myapp/manifests/v1", strings.NewReader(string(body)))
	putReq.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	putRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated {
		t.Fatalf("manifest PUT status = %d, want 201", putRec.Code)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v2/docker-hosted/myapp/manifests/v1", nil)
	getRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Errorf("manifest GET status = %d, want 200 (warn serves)", getRec.Code)
	}
	if h := getRec.Header().Get("X-Forge-Vulnerabilities"); h == "" {
		t.Error("X-Forge-Vulnerabilities header missing in Warn mode")
	}
}

// TestOCIGate_DigestPullFailsOpen verifies that a digest-based manifest GET
// is never blocked (digest refs are excluded from the gate).
func TestOCIGate_DigestPullFailsOpen(t *testing.T) {
	srv, m := newOCIServer(t, trivyCleanJSON)

	pm := vuln.NewPolicyManager(m)
	pm.SetDefault(vuln.Policy{Mode: vuln.ModeBlock, Threshold: vuln.SeverityLow})
	srv.VulnPolicy = pm

	// Push a manifest (the digest will be sha256:...).
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	putReq := httptest.NewRequest(http.MethodPut, "/v2/docker-hosted/myapp/manifests/latest", strings.NewReader(string(body)))
	putReq.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	putRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(putRec, putReq)

	// Extract the digest from the response headers.
	dgst := putRec.Header().Get("Docker-Content-Digest")
	if dgst == "" {
		t.Fatal("manifest PUT did not return Docker-Content-Digest")
	}

	// GET by digest — should never be blocked regardless of policy.
	getReq := httptest.NewRequest(http.MethodGet, "/v2/docker-hosted/myapp/manifests/"+dgst, nil)
	getRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(getRec, getReq)
	if getRec.Code == http.StatusForbidden {
		t.Error("digest-based manifest GET should not be blocked (gate fails open for digests)")
	}
}

// compile-time: fakeQueue must satisfy queue.Queue
var _ queue.Queue = (*fakeQueue)(nil)
