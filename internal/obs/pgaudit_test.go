//go:build integration

package obs_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/testutil"
)

// TestPGAuditSink_RoundTrip verifies Append→Recent persistence, newest-first
// ordering, and that the LIMIT is honoured. Append is async, so Recent is
// polled until the writes drain.
func TestPGAuditSink_RoundTrip(t *testing.T) {
	dsn := testutil.StartPostgres(t)

	// meta.NewPG runs migrate.Up, which creates the audit_log table.
	m, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink := obs.NewPGAuditSink(ctx, db, 0) // 0 = pruning disabled

	now := time.Now().Truncate(time.Millisecond)
	const total = 3
	for i := 0; i < total; i++ {
		sink.Append(obs.AuditEntry{
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Actor:     "alice",
			Method:    "PUT",
			Path:      fmt.Sprintf("/r/%d", i),
			Status:    201,
			Detail:    fmt.Sprintf("note-%d", i),
		})
	}

	// Wait for the async drain to land all entries.
	var got []obs.AuditEntry
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = sink.Recent(10)
		if len(got) >= total {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != total {
		t.Fatalf("expected %d entries, got %d", total, len(got))
	}

	// Newest first.
	if got[0].Path != "/r/2" {
		t.Errorf("expected newest-first /r/2, got %q", got[0].Path)
	}
	// Field round-trip.
	if got[0].Actor != "alice" || got[0].Method != "PUT" || got[0].Status != 201 || got[0].Detail != "note-2" {
		t.Errorf("field round-trip mismatch: %+v", got[0])
	}

	// LIMIT is honoured.
	if one := sink.Recent(1); len(one) != 1 {
		t.Errorf("expected LIMIT 1 to return 1 entry, got %d", len(one))
	}
}

// TestPGAuditSink_Prune verifies entries older than the retention window are
// deleted while fresher ones survive. Rows are inserted directly (synchronously)
// so the assertion doesn't race the async writer.
func TestPGAuditSink_Prune(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	m, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, row := range []struct {
		path string
		ts   time.Time
	}{
		{"/old", time.Now().Add(-48 * time.Hour)},
		{"/new", time.Now()},
	} {
		if _, err := db.Exec(
			`INSERT INTO audit_log (ts, actor, method, path, status) VALUES ($1, 'a', 'GET', $2, 200)`,
			row.ts, row.path); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink := obs.NewPGAuditSink(ctx, db, 24*time.Hour)

	if _, err := sink.Prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := sink.Recent(10)
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving entry after prune, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/new" {
		t.Errorf("expected /new to survive, got %q", got[0].Path)
	}
}

// TestPGAuditSink_QueryKeysetAndFilter verifies keyset pagination (no overlap
// across pages, strict newest-first order) and actor/path filters.
func TestPGAuditSink_QueryKeysetAndFilter(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	m, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink := obs.NewPGAuditSink(ctx, db, 0)

	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		sink.Append(obs.AuditEntry{Timestamp: base.Add(time.Duration(i) * time.Second), Actor: "alice", Method: "PUT", Path: fmt.Sprintf("/a/%d", i), Status: 201})
	}
	for i := 0; i < 2; i++ {
		sink.Append(obs.AuditEntry{Timestamp: base.Add(time.Duration(10+i) * time.Second), Actor: "bob", Method: "DELETE", Path: fmt.Sprintf("/b/%d", i), Status: 204})
	}

	// Wait for all 7 to drain.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.Recent(20)) >= 7 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Page 1: newest 3 (bob/1, bob/0, alice/4).
	p1, err := sink.Query(ctx, obs.AuditFilter{Limit: 3})
	if err != nil {
		t.Fatalf("query p1: %v", err)
	}
	if len(p1) != 3 || p1[0].Path != "/b/1" {
		t.Fatalf("page 1 wrong: %+v", p1)
	}

	// Page 2 via keyset cursor: must not overlap and must be strictly older.
	cur := obs.AuditCursor{Timestamp: p1[2].Timestamp, ID: p1[2].ID}
	p2, err := sink.Query(ctx, obs.AuditFilter{Limit: 3, Cursor: cur})
	if err != nil {
		t.Fatalf("query p2: %v", err)
	}
	if len(p2) != 3 {
		t.Fatalf("expected 3 on page 2, got %d", len(p2))
	}
	seen := map[int64]bool{}
	for _, r := range p1 {
		seen[r.ID] = true
	}
	for _, r := range p2 {
		if seen[r.ID] {
			t.Errorf("page 2 overlaps page 1 at id %d (%s)", r.ID, r.Path)
		}
	}

	// Filters.
	fb, err := sink.Query(ctx, obs.AuditFilter{Actor: "bob", Limit: 10})
	if err != nil || len(fb) != 2 {
		t.Fatalf("actor filter: want 2 got %d (err %v)", len(fb), err)
	}
	fa, err := sink.Query(ctx, obs.AuditFilter{PathLike: "/a/", Limit: 10})
	if err != nil || len(fa) != 5 {
		t.Fatalf("path filter: want 5 got %d (err %v)", len(fa), err)
	}
}
