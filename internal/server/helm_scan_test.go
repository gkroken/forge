package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/helm"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/trivy"
	"forge/internal/vuln"
)

// trivyConfigJSON is a minimal `trivy config` report with two failing checks
// (real-shaped: Misconfigurations[] with FAIL status and KSV rule IDs).
const trivyConfigJSON = `{"Results":[{"Target":"templates/pod.yaml","Class":"config",
  "Misconfigurations":[
    {"ID":"KSV-0017","Title":"Privileged container","Severity":"HIGH","Status":"FAIL",
     "PrimaryURL":"https://avd.aquasec.com/misconfig/ksv017"},
    {"ID":"KSV-0001","Title":"Can elevate privileges","Severity":"MEDIUM","Status":"FAIL",
     "PrimaryURL":"https://avd.aquasec.com/misconfig/ksv001"}
  ]}]}`

// makeChartTGZ builds a minimal valid chart .tgz (Chart.yaml only) so helm.upload
// can parse its name/version.
func makeChartTGZ(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	yaml := fmt.Sprintf("name: %s\nversion: %s\napiVersion: v2\ndescription: test\n", name, version)
	_ = tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0644, Size: int64(len(yaml))})
	_, _ = tw.Write([]byte(yaml))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// newHelmServer wires a server with one Helm hosted repo and a Trivy scanner
// backed by the given executor output.
func newHelmServer(t *testing.T, trivyOut string) (*Server, blob.Store, *meta.FS) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(helm.New())
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted, Enabled: true}) //nolint:errcheck

	sc := trivy.New("trivy", "", "").WithExecutor(&fakeTrivyExecutor{out: trivyOut})
	srv := New(mgr, reg, b, m, nil)
	srv.Vuln = vuln.NewStore(m)
	srv.Trivy = sc
	return srv, b, m
}

// seedChart writes the blob + meta record that scanHelmChart / BrowseRepo need.
func seedChart(t *testing.T, b blob.Store, m *meta.FS, repoName, name, version string) {
	t.Helper()
	filename := fmt.Sprintf("%s-%s.tgz", name, version)
	if _, err := b.Put(repoName+"/"+filename, bytes.NewReader(makeChartTGZ(t, name, version))); err != nil {
		t.Fatal(err)
	}
	m.PutJSON(repoName+":helm", name+"-"+version, map[string]any{ //nolint:errcheck
		"name": name, "version": version, "digest": "x",
		"created": "2024-01-01T00:00:00Z", "filename": filename,
	})
}

func TestScanHelmChart_WritesMisconfigFinding(t *testing.T) {
	srv, b, m := newHelmServer(t, trivyConfigJSON)
	seedChart(t, b, m, "helm-hosted", "mychart", "1.0.0")

	if err := srv.scanHelmChart(context.Background(), "helm-hosted", "mychart", "1.0.0"); err != nil {
		t.Fatal(err)
	}

	f, ok, err := srv.Vuln.Get("helm-hosted", "mychart", "1.0.0")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if f.Source != vuln.SourceTrivyConfig {
		t.Errorf("Source = %q, want %q", f.Source, vuln.SourceTrivyConfig)
	}
	if len(f.Advisories) != 2 {
		t.Fatalf("Advisories = %d, want 2", len(f.Advisories))
	}
	if f.Worst() != vuln.SeverityHigh {
		t.Errorf("Worst = %v, want high", f.Worst())
	}
	if f.ScannedAt.IsZero() {
		t.Error("ScannedAt not stamped")
	}

	// Rollup must be persisted for list surfaces.
	r, ok, err := srv.Vuln.GetRollup("helm-hosted")
	if err != nil || !ok {
		t.Fatalf("GetRollup: ok=%v err=%v", ok, err)
	}
	if r.VulnerableCount != 1 || r.WorstByComponent["mychart"] != vuln.SeverityHigh {
		t.Errorf("rollup = %+v", r)
	}
}

func TestScanHelmChart_Clean(t *testing.T) {
	srv, b, m := newHelmServer(t, `{"Results":[]}`)
	seedChart(t, b, m, "helm-hosted", "okchart", "2.0.0")

	if err := srv.scanHelmChart(context.Background(), "helm-hosted", "okchart", "2.0.0"); err != nil {
		t.Fatal(err)
	}
	f, ok, _ := srv.Vuln.Get("helm-hosted", "okchart", "2.0.0")
	if !ok || f.Source != vuln.SourceTrivyConfig || len(f.Advisories) != 0 {
		t.Errorf("clean finding = %+v ok=%v", f, ok)
	}
}

func TestScanHelmChart_MissingBlob(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	// No chart stored → blob read fails.
	if err := srv.scanHelmChart(context.Background(), "helm-hosted", "ghost", "0.0.1"); err == nil {
		t.Fatal("expected error when chart blob is missing")
	}
}

func TestScanHelmRepo_ScansAllCharts(t *testing.T) {
	srv, b, m := newHelmServer(t, trivyConfigJSON)
	seedChart(t, b, m, "helm-hosted", "alpha", "1.0.0")
	seedChart(t, b, m, "helm-hosted", "alpha", "1.1.0")
	seedChart(t, b, m, "helm-hosted", "beta", "0.5.0")

	if err := srv.scanHelmRepo(context.Background(), "helm-hosted"); err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"alpha", "1.0.0"}, {"alpha", "1.1.0"}, {"beta", "0.5.0"}} {
		_, ok, err := srv.Vuln.Get("helm-hosted", p[0], p[1])
		if err != nil || !ok {
			t.Errorf("missing finding for %s@%s (ok=%v err=%v)", p[0], p[1], ok, err)
		}
	}
}

func TestEnqueueHelmScan_EnqueuesJob(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	q := &fakeQueue{}
	srv.Queue = q

	srv.enqueueHelmScan("helm-hosted")
	for i := 0; i < 100; i++ {
		if len(q.snapshot()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	jobs := q.snapshot()
	if len(jobs) != 1 || jobs[0].typ != helmRepoScanJobType {
		t.Fatalf("jobs = %+v, want 1 %s", jobs, helmRepoScanJobType)
	}
	var p helmRepoScanPayload
	if err := json.Unmarshal([]byte(jobs[0].payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Repo != "helm-hosted" {
		t.Errorf("payload = %+v", p)
	}
}

func TestEnqueueHelmScan_NoopWhenUnconfigured(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	srv.Trivy = nil // not configured
	q := &fakeQueue{}
	srv.Queue = q
	srv.enqueueHelmScan("helm-hosted")
	time.Sleep(20 * time.Millisecond)
	if len(q.snapshot()) != 0 {
		t.Error("expected no enqueue when Trivy unconfigured")
	}
}

func TestHandleVulnScan_Helm(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	q := &fakeQueue{}
	srv.Queue = q

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/scan", nil)
	srv.handleVulnScan(rw, req, "helm-hosted")
	if rw.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rw.Code)
	}
	jobs := q.snapshot()
	if len(jobs) != 1 || jobs[0].typ != helmRepoScanJobType {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestHandleVulnScan_HelmUnconfigured(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	srv.Trivy = nil // no scanner
	srv.Queue = &fakeQueue{}

	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/scan", nil)
	srv.handleVulnScan(rw, req, "helm-hosted")
	if rw.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when Trivy not configured", rw.Code)
	}
}

// TestMiddleware_HelmUpload_EnqueuesScan verifies a successful chart upload
// (POST .../api/charts) enqueues a helm.scan.config job.
func TestMiddleware_HelmUpload_EnqueuesScan(t *testing.T) {
	srv, _, _ := newHelmServer(t, trivyConfigJSON)
	q := &fakeQueue{}
	srv.Queue = q
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/repository/helm-hosted/api/charts",
		bytes.NewReader(makeChartTGZ(t, "mychart", "1.0.0")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	for i := 0; i < 100; i++ {
		if len(q.snapshot()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var helmJobs int
	for _, j := range q.snapshot() {
		if j.typ == helmRepoScanJobType {
			helmJobs++
		}
	}
	if helmJobs != 1 {
		t.Errorf("want 1 helm scan job after upload, got %d (all: %+v)", helmJobs, q.snapshot())
	}
}

// compile-time: helm handler satisfies the interfaces the scan path relies on.
var _ format.Browsable = (*helm.Handler)(nil)

func TestHandleHelmRepoScanJob_Dispatches(t *testing.T) {
	srv, b, m := newHelmServer(t, trivyConfigJSON)
	seedChart(t, b, m, "helm-hosted", "mychart", "1.0.0")

	payload, _ := json.Marshal(helmRepoScanPayload{Repo: "helm-hosted"})
	j := queue.Job{Type: helmRepoScanJobType, Payload: json.RawMessage(payload)}
	if err := srv.handleHelmRepoScanJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := srv.Vuln.Get("helm-hosted", "mychart", "1.0.0"); !ok {
		t.Error("finding not written by job handler")
	}
}
