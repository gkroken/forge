package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/helm"
	"forge/internal/format/npm"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
)

// newIntegrityServer wires a server with a helm hosted repo containing one
// valid chart and one record whose blob is missing, plus a group repo.
func newIntegrityServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(helm.New())
	reg.Register(npm.New())
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted})                                 //nolint:errcheck
	mgr.Add(repo.Repository{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted})                                   //nolint:errcheck
	mgr.Add(repo.Repository{Name: "helm-group", Format: "helm", Kind: repo.Group, Members: []string{"helm-hosted"}}) //nolint:errcheck

	// Valid chart: blob + record with the right digest.
	body := []byte("chart-bytes")
	b.Put("helm-hosted/ok-1.0.0.tgz", strings.NewReader(string(body))) //nolint:errcheck
	m.PutJSON("helm-hosted:helm", "ok-1.0.0", map[string]any{          //nolint:errcheck
		"name": "ok", "version": "1.0.0", "digest": blob.SHA256(body), "filename": "ok-1.0.0.tgz",
	})
	// Record whose blob is gone → missing finding.
	m.PutJSON("helm-hosted:helm", "gone-2.0.0", map[string]any{ //nolint:errcheck
		"name": "gone", "version": "2.0.0", "digest": "abc", "filename": "gone-2.0.0.tgz",
	})
	return New(mgr, reg, b, m, nil)
}

func TestVerifyRepoIntegrity_WritesReport(t *testing.T) {
	srv := newIntegrityServer(t)
	srv.verifyRepoIntegrity("helm-hosted", integrity.ModeFull)

	rep, ok, err := integrity.NewStore(srv.Meta).Get("helm-hosted")
	if err != nil || !ok {
		t.Fatalf("report: ok=%v err=%v", ok, err)
	}
	if rep.Status != integrity.StatusComplete || rep.Mode != integrity.ModeFull {
		t.Fatalf("report = %+v", rep)
	}
	if rep.TotalFindings != 1 || rep.Findings[0].Kind != integrity.KindMissing {
		t.Fatalf("findings = %+v", rep.Findings)
	}
	if rep.Counts[integrity.KindMissing] != 1 || rep.BlobsChecked != 1 || rep.BytesRead == 0 {
		t.Errorf("stats wrong: %+v", rep)
	}

	// Group repos are skipped with an explanatory note.
	srv.verifyRepoIntegrity("helm-group", integrity.ModeQuick)
	grep, ok, _ := integrity.NewStore(srv.Meta).Get("helm-group")
	if !ok || !grep.Clean() || !strings.Contains(grep.Note, "own no storage") {
		t.Errorf("group report = %+v", grep)
	}
}

func TestVerifyAPI_PostEnqueuesAndDedupes(t *testing.T) {
	srv := newIntegrityServer(t)
	q := &fakeQueue{}
	srv.Queue = q
	h := srv.Routes()

	// POST → 202 + queued marker + job enqueued.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/verify?mode=quick", nil))
	if rw.Code != http.StatusAccepted {
		t.Fatalf("POST verify = %d: %s", rw.Code, rw.Body)
	}
	jobs := q.snapshot()
	if len(jobs) != 1 || jobs[0].typ != integrityJobType {
		t.Fatalf("jobs = %+v", jobs)
	}
	if !strings.Contains(jobs[0].payload, `"quick"`) {
		t.Errorf("payload = %s", jobs[0].payload)
	}

	// Second POST while queued → 202 but no new job.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/verify", nil))
	if rw.Code != http.StatusAccepted {
		t.Fatalf("dedupe POST = %d", rw.Code)
	}
	if len(q.snapshot()) != 1 {
		t.Fatalf("expected dedupe, jobs = %+v", q.snapshot())
	}

	// Stale queued marker → re-enqueue allowed.
	store := integrity.NewStore(srv.Meta)
	store.Put(integrity.Report{ //nolint:errcheck
		Repo: "helm-hosted", Status: integrity.StatusQueued, Mode: integrity.ModeFull,
		QueuedAt: time.Now().Add(-3 * time.Hour),
	})
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/verify", nil))
	if rw.Code != http.StatusAccepted || len(q.snapshot()) != 2 {
		t.Fatalf("stale re-enqueue: code=%d jobs=%d", rw.Code, len(q.snapshot()))
	}

	// Bad mode → 400. Unknown repo → 404.
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/verify?mode=deep", nil))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad mode = %d", rw.Code)
	}
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/nope/verify", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown repo = %d", rw.Code)
	}
}

func TestVerifyAPI_GetReport(t *testing.T) {
	srv := newIntegrityServer(t)
	h := srv.Routes()

	// Never verified → status "never".
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/helm-hosted/verify", nil))
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"never"`) {
		t.Fatalf("GET = %d: %s", rw.Code, rw.Body)
	}

	srv.verifyRepoIntegrity("helm-hosted", integrity.ModeFull)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/repos/helm-hosted/verify", nil))
	var rep integrity.Report
	if err := json.Unmarshal(rw.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != integrity.StatusComplete || rep.TotalFindings != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestIntegrityJob_RunsVerify(t *testing.T) {
	srv := newIntegrityServer(t)
	job := queue.Job{Type: integrityJobType, Payload: []byte(`{"repo":"helm-hosted","mode":"full"}`)}
	if err := srv.handleIntegrityJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	rep, ok, _ := integrity.NewStore(srv.Meta).Get("helm-hosted")
	if !ok || rep.Status != integrity.StatusComplete {
		t.Fatalf("report = %+v ok=%v", rep, ok)
	}
}

func TestIntegrityRollup(t *testing.T) {
	srv := newIntegrityServer(t)
	srv.verifyRepoIntegrity("helm-hosted", integrity.ModeQuick)
	h := srv.Routes()

	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/v1/integrity", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("rollup = %d: %s", rw.Code, rw.Body)
	}
	var rows []integrityRollupRow
	if err := json.Unmarshal(rw.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	byRepo := map[string]integrityRollupRow{}
	for _, r := range rows {
		byRepo[r.Repo] = r
	}
	if byRepo["helm-hosted"].Report == nil || byRepo["helm-hosted"].Report.TotalFindings != 1 {
		t.Errorf("helm-hosted row wrong: %+v", byRepo["helm-hosted"])
	}
	if byRepo["npm-hosted"].Report != nil {
		t.Errorf("npm-hosted must be never-verified: %+v", byRepo["npm-hosted"])
	}
}

func TestReindex_Honest(t *testing.T) {
	srv := newIntegrityServer(t)
	h := srv.Routes()

	// helm has no materialized index → honest noop.
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-hosted/reindex", nil))
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"noop"`) {
		t.Fatalf("helm reindex = %d: %s", rw.Code, rw.Body)
	}

	// npm: seed a version record + corrupted packument; reindex rebuilds it.
	srv.Meta.PutJSON("npm-hosted:npm:v", "is-odd:1.0.0", map[string]any{ //nolint:errcheck
		"name": "is-odd", "version": "1.0.0", "dist": map[string]any{},
	})
	srv.Meta.PutJSON("npm-hosted:npm", "is-odd", map[string]any{ //nolint:errcheck
		"name": "is-odd", "versions": map[string]any{},
	})
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/reindex", nil))
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"rebuilt":1`) {
		t.Fatalf("npm reindex = %d: %s", rw.Code, rw.Body)
	}
	var pack struct {
		Versions map[string]any `json:"versions"`
	}
	ok, _ := srv.Meta.GetJSON("npm-hosted:npm", "is-odd", &pack)
	if !ok || len(pack.Versions) != 1 {
		t.Fatalf("packument not rebuilt: %+v", pack)
	}
}
