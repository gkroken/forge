package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/helm"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/vuln"
)

// osvTestServer reports one HIGH advisory (vulnID) for every queried coordinate.
func osvTestServer(t *testing.T, vulnID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []json.RawMessage `json:"queries"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := make([]map[string]any, len(req.Queries))
		for i := range req.Queries {
			results[i] = map[string]any{"vulns": []map[string]string{{"id": vulnID, "modified": "2024-01-01T00:00:00Z"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/v1/vulns/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"summary":"test advisory","database_specific":{"severity":"HIGH"}}`, vulnID)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeQueue records enqueued jobs without running a worker.
type fakeQueue struct {
	mu   sync.Mutex
	jobs []fakeJob
}

type fakeJob struct {
	typ     string
	payload string
}

func (q *fakeQueue) Enqueue(ctx context.Context, typ string, payload any) error {
	b, _ := json.Marshal(payload)
	q.mu.Lock()
	q.jobs = append(q.jobs, fakeJob{typ, string(b)})
	q.mu.Unlock()
	return nil
}

func (q *fakeQueue) EnqueueAfter(ctx context.Context, typ string, payload any, _ time.Duration) error {
	return q.Enqueue(ctx, typ, payload)
}

func (q *fakeQueue) Work(ctx context.Context, _ func(context.Context, queue.Job) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (q *fakeQueue) snapshot() []fakeJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]fakeJob(nil), q.jobs...)
}

// newVulnServer wires a server with npm + helm hosted repos and a vuln store +
// OSV client pointed at osvURL.
func newVulnServer(t *testing.T, osvURL string) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(npm.New())
	reg.Register(helm.New())
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})   //nolint:errcheck
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted}) //nolint:errcheck

	m.PutJSON("npm-hosted:npm", "lodash", map[string]any{ //nolint:errcheck
		"name":     "lodash",
		"versions": map[string]any{"4.17.20": map[string]any{}},
	})
	m.PutJSON("helm-hosted:helm", "mychart-1.0.0", map[string]any{ //nolint:errcheck
		"name": "mychart", "version": "1.0.0", "digest": "x", "created": "2024-01-01", "filename": "mychart-1.0.0.tgz",
	})

	srv := New(mgr, reg, b, m, nil)
	srv.Vuln = vuln.NewStore(m)
	srv.OSV = vuln.NewClient(http.DefaultClient, vuln.WithBaseURL(osvURL))
	return srv
}

func TestScanRepo_WritesFindings(t *testing.T) {
	osv := osvTestServer(t, "GHSA-test-0001")
	srv := newVulnServer(t, osv.URL)

	if err := srv.scanRepo(context.Background(), "npm-hosted"); err != nil {
		t.Fatal(err)
	}

	f, ok, err := srv.Vuln.Get("npm-hosted", "lodash", "4.17.20")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if f.Source != vuln.SourceOSV {
		t.Errorf("Source = %q", f.Source)
	}
	if len(f.Advisories) != 1 || f.Advisories[0].ID != "GHSA-test-0001" {
		t.Fatalf("advisories = %+v", f.Advisories)
	}
	if f.Worst() != vuln.SeverityHigh {
		t.Errorf("Worst = %v, want high", f.Worst())
	}
	if f.ScannedAt.IsZero() {
		t.Error("ScannedAt not stamped")
	}

	// The scan must also persist the per-repo rollup that list surfaces read.
	r, ok, err := srv.Vuln.GetRollup("npm-hosted")
	if err != nil || !ok {
		t.Fatalf("GetRollup: ok=%v err=%v", ok, err)
	}
	if r.WorstByComponent["lodash"] != vuln.SeverityHigh {
		t.Errorf("rollup worst for lodash = %v, want high", r.WorstByComponent["lodash"])
	}
	if r.VulnerableCount != 1 || r.BySeverity["high"] != 1 {
		t.Errorf("rollup count=%d bySeverity=%v, want 1 / high:1", r.VulnerableCount, r.BySeverity)
	}
}

func TestBrowseSurfaces_CarrySeverity(t *testing.T) {
	osv := osvTestServer(t, "GHSA-test-0001")
	srv := newVulnServer(t, osv.URL)
	if err := srv.scanRepo(context.Background(), "npm-hosted"); err != nil {
		t.Fatal(err)
	}

	// Component list carries the worst severity per package.
	rec := httptest.NewRecorder()
	srv.handleComponents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components?limit=0", nil), "npm-hosted")
	var comps componentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &comps); err != nil {
		t.Fatalf("decode components: %v", err)
	}
	var found bool
	for _, c := range comps.Components {
		if c.Name == "lodash" {
			found = true
			if c.Severity != "high" {
				t.Errorf("lodash component severity = %q, want high", c.Severity)
			}
		}
	}
	if !found {
		t.Fatal("lodash not in component list")
	}

	// Version list carries per-version severity.
	rec = httptest.NewRecorder()
	srv.uiBrowseVersions(rec, httptest.NewRequest(http.MethodGet, "/ui/browse/npm-hosted/versions?pkg=lodash", nil), "npm-hosted")
	var vers browseVersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &vers); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(vers.Versions) == 0 || vers.Versions[0].Severity != "high" {
		t.Errorf("version severity = %+v, want first high", vers.Versions)
	}
}

func TestBrowseSurfaces_NoBadgeWhenUnconfigured(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	srv.Vuln = nil // scanning not configured → severity must be empty, never "unknown"

	rec := httptest.NewRecorder()
	srv.handleComponents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos/npm-hosted/components?limit=0", nil), "npm-hosted")
	var comps componentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &comps); err != nil {
		t.Fatal(err)
	}
	for _, c := range comps.Components {
		if c.Severity != "" {
			t.Errorf("component %q severity = %q, want empty when unconfigured", c.Name, c.Severity)
		}
	}
}

func TestScanRepo_SkipsNonScannableFormat(t *testing.T) {
	osv := osvTestServer(t, "GHSA-should-not-appear")
	srv := newVulnServer(t, osv.URL)

	// helm has no OSVCoordinates → scanRepo returns before querying OSV.
	if err := srv.scanRepo(context.Background(), "helm-hosted"); err != nil {
		t.Fatal(err)
	}
	findings, err := srv.Vuln.List("helm-hosted")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings for non-scannable format, got %+v", findings)
	}
}

func TestVulnRescanTick_EnqueuesPerScannableRepoAndRespectsInterval(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	fq := &fakeQueue{}
	srv.Queue = fq

	now := time.Now()
	lastRun := map[string]time.Time{}

	// First tick: npm is scannable → one enqueue; helm is not → skipped.
	srv.VulnRescanTick(now, lastRun)
	jobs := fq.snapshot()
	if len(jobs) != 1 || jobs[0].typ != vulnScanJobType || !strings.Contains(jobs[0].payload, "npm-hosted") {
		t.Fatalf("first tick jobs = %+v, want one npm-hosted vuln.scan", jobs)
	}
	if lastRun[vulnRescanKey("npm-hosted")].IsZero() {
		t.Error("npm-hosted lastRun not stamped")
	}
	if _, ok := lastRun[vulnRescanKey("helm-hosted")]; ok {
		t.Error("non-scannable helm-hosted should not be tracked")
	}

	// Second tick within the interval: no new enqueue.
	srv.VulnRescanTick(now.Add(time.Hour), lastRun)
	if got := len(fq.snapshot()); got != 1 {
		t.Errorf("within-interval tick enqueued more jobs: total %d, want 1", got)
	}

	// A tick past the interval re-enqueues.
	srv.VulnRescanTick(now.Add(vulnRescanInterval+time.Minute), lastRun)
	if got := len(fq.snapshot()); got != 2 {
		t.Errorf("post-interval tick should re-enqueue: total %d, want 2", got)
	}
}

func TestVulnRescanTick_NoopWhenUnconfigured(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	fq := &fakeQueue{}
	srv.Queue = fq
	srv.Vuln = nil // scanning off

	srv.VulnRescanTick(time.Now(), map[string]time.Time{})
	if got := len(fq.snapshot()); got != 0 {
		t.Errorf("enqueued %d jobs when scanning unconfigured, want 0", got)
	}
}

func TestHandleVulnScan_Enqueues(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	q := &fakeQueue{}
	srv.Queue = q

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/scan", nil)
	srv.handleVulnScan(rw, req, "npm-hosted")

	if rw.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rw.Code)
	}
	jobs := q.snapshot()
	if len(jobs) != 1 || jobs[0].typ != vulnScanJobType {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !strings.Contains(jobs[0].payload, `"repo":"npm-hosted"`) {
		t.Errorf("payload = %s", jobs[0].payload)
	}
}

func TestHandleVulnScan_NonScannableFormat(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	srv.Queue = &fakeQueue{}

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/scan", nil)
	srv.handleVulnScan(rw, req, "helm-hosted")

	if rw.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 for non-scannable format", rw.Code)
	}
}

func TestHandleVulnScan_Unconfigured(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	srv.Vuln = nil // scanning not configured
	srv.Queue = &fakeQueue{}

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/scan", nil)
	srv.handleVulnScan(rw, req, "npm-hosted")

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when unconfigured", rw.Code)
	}
}

func TestEnqueueVulnScan_NoopWhenUnconfigured(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	srv.OSV = nil // disabled
	q := &fakeQueue{}
	srv.Queue = q

	srv.enqueueVulnScan("npm-hosted")
	time.Sleep(20 * time.Millisecond) // let any (errant) goroutine run

	if jobs := q.snapshot(); len(jobs) != 0 {
		t.Errorf("expected no enqueue when OSV disabled, got %+v", jobs)
	}
}

func TestBrowseDetail_VulnStates(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	srv.Vuln.Put("npm-hosted", vuln.Finding{ //nolint:errcheck
		Component: "lodash", Version: "4.17.20", Source: vuln.SourceOSV,
		Advisories: []vuln.Advisory{{
			ID: "GHSA-x", Severity: vuln.SeverityHigh, Summary: "bad", URL: "https://e/x", FixedIn: []string{"4.17.21"},
		}},
	})

	get := func(repoName, pkg, ver string) browseDetailResponse {
		t.Helper()
		rw := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ui/browse/"+repoName+"/detail?pkg="+pkg+"&ver="+ver, nil)
		srv.uiBrowseDetail(rw, req, repoName)
		if rw.Code != http.StatusOK {
			t.Fatalf("%s %s@%s: status %d body=%s", repoName, pkg, ver, rw.Code, rw.Body.String())
		}
		var d browseDetailResponse
		if err := json.Unmarshal(rw.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Scanned + vulnerable.
	d := get("npm-hosted", "lodash", "4.17.20")
	if d.Vuln == nil || !d.Vuln.Supported || !d.Vuln.Scanned {
		t.Fatalf("vulnerable: state = %+v", d.Vuln)
	}
	if d.Vuln.Severity != "high" || len(d.Vuln.Advisories) != 1 || d.Vuln.Advisories[0].ID != "GHSA-x" {
		t.Errorf("vulnerable: %+v", d.Vuln)
	}

	// Supported format, no finding for this version → scanned=false.
	d = get("npm-hosted", "lodash", "4.17.21")
	if d.Vuln == nil || !d.Vuln.Supported || d.Vuln.Scanned {
		t.Errorf("unscanned: state = %+v", d.Vuln)
	}

	// Unsupported format (helm has no OSV mapping).
	d = get("helm-hosted", "mychart", "1.0.0")
	if d.Vuln == nil || d.Vuln.Supported {
		t.Errorf("unsupported: state = %+v", d.Vuln)
	}
}

func TestEnqueueVulnScan_Enqueues(t *testing.T) {
	srv := newVulnServer(t, "http://unused.example")
	q := &fakeQueue{}
	srv.Queue = q

	srv.enqueueVulnScan("npm-hosted")

	// enqueue runs in a goroutine; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(q.snapshot()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	jobs := q.snapshot()
	if len(jobs) != 1 || jobs[0].typ != vulnScanJobType {
		t.Fatalf("jobs = %+v", jobs)
	}
}
