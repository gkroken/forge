package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"forge/internal/obs"
	"forge/internal/queue"
)

// TestUIComponentPage renders the component detail page for a seeded npm package.
func TestUIComponentPage(t *testing.T) {
	srv := newUIServer(t) // seeds npm-hosted:lodash
	rw := uiGet(t, srv.Routes(), "/ui/repos/npm-hosted/lodash")
	if rw.Code != http.StatusOK {
		t.Fatalf("component page: status %d (%.200s)", rw.Code, rw.Body.String())
	}

	// Unknown component → 404.
	rw = uiGet(t, srv.Routes(), "/ui/repos/npm-hosted/does-not-exist")
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown component: status %d, want 404", rw.Code)
	}
}

// TestRepoSettings_ActivityCard renders the repo Settings page, exercising
// buildRepoActivity across several audit shapes (verb from method, verb from
// Detail, and a denied 4xx row).
func TestRepoSettings_ActivityCard(t *testing.T) {
	srv := newRichUIServer(t)
	al := obs.NewAuditLog(20)
	now := time.Now()
	al.Append(obs.AuditEntry{Timestamp: now, Actor: "alice", Method: "PUT", Path: "/repository/npm-hosted/lodash", Status: 201})
	al.Append(obs.AuditEntry{Timestamp: now, Actor: "bob", Method: "POST", Path: "/api/v1/repos/npm-hosted/trash/restore", Status: 200, Detail: "restored lodash@1.0.0"})
	al.Append(obs.AuditEntry{Timestamp: now, Actor: "anon", Method: "GET", Path: "/repository/npm-hosted/lodash", Status: 403})
	srv.WithAuditLog(al)

	rw := uiGet(t, srv.Routes(), "/ui/admin/repos/npm-hosted/edit")
	if rw.Code != http.StatusOK {
		t.Fatalf("repo settings page: status %d (%.200s)", rw.Code, rw.Body.String())
	}
}

// TestDashboard_TaskRows populates the task ring and renders the dashboard so
// buildTaskRows converts both a done and a failed task.
func TestDashboard_TaskRows(t *testing.T) {
	srv := newRichUIServer(t)
	srv.TaskRing = queue.NewTaskRing(10)
	wrap := srv.TaskRing.Wrap
	ctx := context.Background()
	wrap(func(context.Context, queue.Job) error { return nil })(ctx, queue.Job{Type: "vuln.scan"})               //nolint:errcheck
	wrap(func(context.Context, queue.Job) error { return context.Canceled })(ctx, queue.Job{Type: "npm.regen"}) //nolint:errcheck

	rw := uiGet(t, srv.Routes(), "/ui/dashboard")
	if rw.Code != http.StatusOK {
		t.Fatalf("dashboard: status %d", rw.Code)
	}
	if got := srv.TaskRing.Recent(5); len(got) < 2 {
		t.Errorf("task ring has %d entries, want >= 2", len(got))
	}
}
