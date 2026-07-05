package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"forge/internal/obs"
)

// fakeQuerier is an obs.AuditSink that also implements obs.AuditQuerier, so the
// audit-history page takes its durable (paginated) branch.
type fakeQuerier struct {
	rows []obs.AuditRecord
}

func (f *fakeQuerier) Append(obs.AuditEntry) {}
func (f *fakeQuerier) Recent(n int) []obs.AuditEntry {
	out := make([]obs.AuditEntry, 0, n)
	for _, r := range f.rows {
		out = append(out, r.AuditEntry)
	}
	return out
}
func (f *fakeQuerier) Query(_ context.Context, filter obs.AuditFilter) ([]obs.AuditRecord, error) {
	if filter.Limit > 0 && filter.Limit < len(f.rows) {
		return f.rows[:filter.Limit], nil
	}
	return f.rows, nil
}

// TestUIAuditHistory_DurableBranch drives the durable audit-history page with
// enough rows to trigger the HasMore / OlderURL pagination path.
func TestUIAuditHistory_DurableBranch(t *testing.T) {
	srv := newRichUIServer(t)
	// Build more than one page of rows so HasMore fires.
	q := &fakeQuerier{}
	now := time.Now().UTC()
	for i := 0; i < auditHistoryPageSize+5; i++ {
		q.rows = append(q.rows, obs.AuditRecord{
			AuditEntry: obs.AuditEntry{Timestamp: now, Actor: "alice", Method: "PUT", Path: "/repository/npm-hosted/p", Status: 201},
			ID:         int64(auditHistoryPageSize + 5 - i),
		})
	}
	srv.WithAuditLog(q)

	rw := uiGet(t, srv.Routes(), "/ui/admin/audit?actor=alice")
	if rw.Code != http.StatusOK {
		t.Fatalf("durable audit page: status %d", rw.Code)
	}
	// The rendered page should offer an "older" link (HasMore path).
	if !contains(rw.Body.String(), "before_id") {
		t.Error("expected an older-page cursor link in the durable audit page")
	}
}
