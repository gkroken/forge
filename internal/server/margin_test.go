package server

import (
	"net/http"
	"testing"

	"forge/internal/integrity"
	"forge/internal/obs"
)

// TestVerifyRepoIntegrity runs the synchronous integrity check over a repo with
// real content in both quick and full modes, then confirms the report is stored
// and served by the verify GET endpoint.
func TestVerifyRepoIntegrity(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-hosted", "npm")
	seedBlobBytes(t, s, http.MethodPut, "npm-hosted", "leftpad", npmPublishBody("leftpad", "1.0.0", []byte("tgz-bytes")))

	s.verifyRepoIntegrity("npm-hosted", integrity.ModeQuick)
	s.verifyRepoIntegrity("npm-hosted", integrity.ModeFull)

	rep, ok, _ := integrity.NewStore(s.Meta).Get("npm-hosted")
	if !ok || rep.Status != integrity.StatusComplete {
		t.Fatalf("expected a completed integrity report, got ok=%v status=%q", ok, rep.Status)
	}

	// The verify GET endpoint now returns the stored report.
	rw := uiGet(t, s.Routes(), "/api/v1/repos/npm-hosted/verify")
	if rw.Code != http.StatusOK {
		t.Errorf("verify GET: status %d", rw.Code)
	}
}

// TestDashboardWithRecordedStats seeds GlobalStats with real request + cache
// activity, then renders the dashboard and observability pages so the
// request/metrics/health bar builders take their with-data branches.
func TestDashboardWithRecordedStats(t *testing.T) {
	srv := newRichUIServer(t)
	gs := obs.NewGlobalStats()
	for i := 0; i < 30; i++ {
		gs.RecordRequest(200, int64(5+i))
	}
	gs.RecordRequest(404, 3)
	gs.RecordRequest(500, 9)
	gs.RecordCacheHit()
	gs.RecordCacheHit()
	gs.RecordCacheMiss()
	srv.WithGlobalStats(gs)
	h := srv.Routes()

	for _, p := range []string{"/ui/dashboard", "/ui/admin/observability"} {
		rw := uiGet(t, h, p)
		if rw.Code != http.StatusOK {
			t.Errorf("GET %s with stats: status %d", p, rw.Code)
		}
	}

	// The system chart endpoints now return non-empty snapshots.
	for _, res := range []string{"request-chart", "metrics-chart", "status-breakdown"} {
		rw := uiGet(t, h, "/api/v1/system/"+res)
		if rw.Code != http.StatusOK {
			t.Errorf("system/%s: status %d", res, rw.Code)
		}
	}
}
