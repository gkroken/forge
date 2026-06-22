//go:build integration

package cleanup_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"forge/internal/cleanup"
	"forge/internal/meta"
	"forge/internal/repo"
	"forge/internal/testutil"
)

// TestPGCoordinator_FiresExactlyOnce verifies the core HA property: two
// schedulers (simulating two replicas) sharing one Postgres database fire a due
// scheduled cleanup exactly once, no matter how many ticks they race at the same
// instant. This exercises both the advisory lock (mutual exclusion within a
// tick) and the persisted lastRun (no re-fire once a follower later acquires the
// lock).
func TestPGCoordinator_FiresExactlyOnce(t *testing.T) {
	dsn := testutil.StartPostgres(t)

	// meta.NewPG runs migrate.Up, which creates cleanup_schedule_state (004).
	m, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	// Shared repo + policy state lives in the same PG meta store both replicas read.
	pm := cleanup.NewPolicyManager(m)
	if err := pm.Put(cleanup.NamedPolicy{Name: "keep-1", KeepVersions: 1, Interval: time.Hour}); err != nil {
		t.Fatalf("put policy: %v", err)
	}
	mgr := repo.NewManager()
	if err := mgr.WithStore(m); err != nil {
		t.Fatalf("repo store: %v", err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "r", Format: "helm", Kind: repo.Hosted, CleanupPolicyName: "keep-1",
	}); err != nil {
		t.Fatalf("add repo: %v", err)
	}

	// Shared blob store (stands in for S3, which both replicas would share).
	bstore, _ := stores(t)

	// Two independent DB handles + coordinators = two replicas contending for the
	// same advisory lock and reading/writing the same cleanup_schedule_state rows.
	newReplica := func() *cleanup.Scheduler {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return cleanup.NewScheduler(mgr, pm, bstore, m).
			WithCoordinator(cleanup.NewPGCoordinator(db))
	}
	a, b := newReplica(), newReplica()

	// Single due timestamp: lastRun starts empty, so the job is overdue.
	now := time.Now()
	ctx := context.Background()

	// Race both replicas at the same instant (tests the lock), then tick each
	// again sequentially (tests persisted lastRun blocks a re-fire).
	var wg sync.WaitGroup
	for _, s := range []*cleanup.Scheduler{a, b} {
		wg.Add(1)
		go func(s *cleanup.Scheduler) { defer wg.Done(); s.Tick(ctx, now) }(s)
	}
	wg.Wait()
	a.Tick(ctx, now)
	b.Tick(ctx, now)

	// A scheduled fire records exactly one CleanupRun in shared history.
	hist, err := cleanup.GetHistory(m, "r")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("want exactly 1 scheduled run across both replicas, got %d", len(hist))
	}

	// And the shared lastRun is visible to both replicas (read via Snapshot).
	if got, ok := a.LastRuns()["r"]; !ok || got.IsZero() {
		t.Fatalf("lastRun for r not persisted/visible: %v", a.LastRuns())
	}
}
