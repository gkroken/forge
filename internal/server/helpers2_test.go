package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"forge/internal/obs"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                      "0 B",
		512:                    "512 B",
		2048:                   "2 KB",
		5 * 1024 * 1024:        "5.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := humanAgo(c.t); got != c.want {
			t.Errorf("humanAgo(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestAuditHistoryURL(t *testing.T) {
	// No cursor → filters only.
	u := auditHistoryURL("alice", "/npm-hosted/", obs.AuditCursor{})
	if !strings.Contains(u, "actor=alice") || strings.Contains(u, "before_id") {
		t.Errorf("no-cursor url = %q", u)
	}
	// With cursor → keyset params present.
	u = auditHistoryURL("alice", "", obs.AuditCursor{Timestamp: time.Now(), ID: 99})
	if !strings.Contains(u, "before_id=99") || !strings.Contains(u, "before_ts=") {
		t.Errorf("cursor url = %q, want before_id/before_ts", u)
	}
}

// TestUIAuditHistoryPage_Filters drives the in-memory audit-history page with
// actor + path filters.
func TestUIAuditHistoryPage_Filters(t *testing.T) {
	srv := newRichUIServer(t) // in-mem AuditLog with alice/anonymous entries
	h := srv.Routes()
	rw := uiGet(t, h, "/ui/admin/audit?actor=alice&path=npm-hosted")
	if rw.Code != http.StatusOK {
		t.Fatalf("audit history filtered: status %d", rw.Code)
	}
	// A non-matching actor filter still renders 200 with no rows.
	rw = uiGet(t, h, "/ui/admin/audit?actor=nobody")
	if rw.Code != http.StatusOK {
		t.Errorf("audit history empty: status %d", rw.Code)
	}
}

// TestUIBrowseVersions drives the center-pane version list for a seeded package.
func TestUIBrowseVersions(t *testing.T) {
	srv := newUIServer(t) // npm-hosted:lodash with two versions
	rw := uiGet(t, srv.Routes(), "/ui/browse/npm-hosted/versions?pkg=lodash")
	if rw.Code != http.StatusOK {
		t.Fatalf("browse versions: status %d (%.200s)", rw.Code, rw.Body.String())
	}
}
