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
	sink := obs.NewPGAuditSink(ctx, db)

	now := time.Now().Truncate(time.Millisecond)
	const total = 3
	for i := 0; i < total; i++ {
		sink.Append(obs.AuditEntry{
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Actor:     "alice",
			Method:    "PUT",
			Path:      fmt.Sprintf("/r/%d", i),
			Status:    201,
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
	if got[0].Actor != "alice" || got[0].Method != "PUT" || got[0].Status != 201 {
		t.Errorf("field round-trip mismatch: %+v", got[0])
	}

	// LIMIT is honoured.
	if one := sink.Recent(1); len(one) != 1 {
		t.Errorf("expected LIMIT 1 to return 1 entry, got %d", len(one))
	}
}
