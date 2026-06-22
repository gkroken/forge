//go:build integration

package queue_test

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/testutil"
)

// TestPG_EnqueueAfter_DelaysVisibility verifies migration 005 + the dequeue
// visibility filter: a delayed job is not dequeued before its visible_after,
// while an immediate job enqueued alongside it is processed right away.
func TestPG_EnqueueAfter_DelaysVisibility(t *testing.T) {
	dsn := testutil.StartPostgres(t)

	// meta.NewPG runs migrate.Up, creating jobs + the visible_after column.
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

	q := queue.NewPG(db)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var immediate, delayed atomic.Int32
	go q.Work(ctx, func(_ context.Context, j queue.Job) error { //nolint:errcheck
		switch j.Type {
		case "immediate":
			immediate.Add(1)
		case "delayed":
			delayed.Add(1)
		}
		return nil
	})

	if err := q.EnqueueAfter(ctx, "delayed", map[string]int{"n": 1}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, "immediate", map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}

	// The immediate job lands quickly; the delayed one must still be invisible.
	waitFor(t, func() bool { return immediate.Load() == 1 }, "immediate job")
	if got := delayed.Load(); got != 0 {
		t.Fatalf("delayed job dequeued before its delay: %d", got)
	}

	// After the delay it becomes visible and is processed.
	waitFor(t, func() bool { return delayed.Load() == 1 }, "delayed job after delay")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
