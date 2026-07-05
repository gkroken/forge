package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"forge/internal/queue"
)

func job(t *testing.T, typ string, payload any) queue.Job {
	t.Helper()
	b, _ := json.Marshal(payload)
	return queue.Job{Type: typ, Payload: b}
}

// TestJobHandlers_BestEffort verifies the worker job handlers are best-effort:
// a malformed payload and a valid-but-work-failing payload both return nil so
// the queue does not spin on immediate retries.
func TestJobHandlers_BestEffort(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-hosted", "npm")
	ctx := context.Background()

	// vuln.scan — bad payload and a real (empty) repo both return nil.
	if err := s.handleVulnScanJob(ctx, queue.Job{Type: "vuln.scan", Payload: []byte("not json")}); err != nil {
		t.Errorf("vuln bad payload: %v", err)
	}
	if err := s.handleVulnScanJob(ctx, job(t, "vuln.scan", vulnScanPayload{Repo: "npm-hosted"})); err != nil {
		t.Errorf("vuln valid payload: %v", err)
	}

	// trivy repo scan — bad payload + unknown repo, both nil.
	if err := s.handleTrivyRepoScanJob(ctx, queue.Job{Type: "trivy.repo", Payload: []byte("{")}); err != nil {
		t.Errorf("trivy bad payload: %v", err)
	}
	if err := s.handleTrivyRepoScanJob(ctx, job(t, "trivy.repo", trivyRepoScanPayload{Repo: "no-such"})); err != nil {
		t.Errorf("trivy valid payload: %v", err)
	}

	// migration job with no spec/plan set → logs and returns nil.
	if err := s.handleMigrationJob(ctx, queue.Job{Type: "migration.run"}); err != nil {
		t.Errorf("migration job no-spec: %v", err)
	}
}

func TestRepoMigStateFail(t *testing.T) {
	st := &repoMigState{}
	for i := 0; i < maxStoredFailures+5; i++ {
		st.fail(fmt.Sprintf("path/%d", i), fmt.Errorf("boom %d", i))
	}
	if st.Failed != maxStoredFailures+5 {
		t.Errorf("Failed = %d, want %d", st.Failed, maxStoredFailures+5)
	}
	if len(st.Failures) != maxStoredFailures {
		t.Errorf("stored failures = %d, want capped at %d", len(st.Failures), maxStoredFailures)
	}
}

func TestMemRecorderErr(t *testing.T) {
	m := newMemRecorder()
	m.WriteHeader(http.StatusInternalServerError)
	m.Write([]byte("upstream exploded")) //nolint:errcheck
	if m.ok() {
		t.Error("500 should not be ok()")
	}
	if err := m.err(); err == nil || !contains(err.Error(), "500") {
		t.Errorf("err() = %v, want a message mentioning 500", err)
	}
}

func TestRowCursor(t *testing.T) {
	got := rowCursor("npm-hosted", "lodash", "4.17.21")
	want := "npm-hosted" + cursorSep + "lodash" + cursorSep + "4.17.21"
	if got != want {
		t.Errorf("rowCursor = %q, want %q", got, want)
	}
}
