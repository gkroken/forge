package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// cleanupLeaderLockKey is the session advisory-lock key the scheduler contends
// for. Any fixed value works as long as it does not collide with another
// pg_advisory_lock user in the same database; forge owns its database, so this
// is just a stable arbitrary constant ("forge cleanup" mnemonic).
const cleanupLeaderLockKey int64 = 0x46_43_4C_4E // "FCLN"

// PGCoordinator is the multi-replica coordinator. It uses a Postgres session
// advisory lock to elect a single leader per tick and persists the shared
// lastRun state in the cleanup_schedule_state table (migration 004), so that
// across N replicas a due scheduled cleanup fires exactly once.
type PGCoordinator struct {
	db *sql.DB
}

var _ Coordinator = (*PGCoordinator)(nil)

// NewPGCoordinator returns a coordinator backed by db. The cleanup_schedule_state
// table must already exist (created by migrate.Up via meta.NewPG).
func NewPGCoordinator(db *sql.DB) *PGCoordinator { return &PGCoordinator{db: db} }

// RunExclusive acquires the leader lock without blocking. If another replica
// holds it, fn is not called and the tick is a no-op. Otherwise it loads the
// shared lastRun, runs fn, and persists every entry fn added or changed before
// releasing the lock.
//
// The lock is held on a single pinned connection for the whole tick. Cleanup
// runs are short and followers simply skip and retry next minute; a crash
// mid-run is self-healing because RunForRepo is idempotent.
func (c *PGCoordinator) RunExclusive(ctx context.Context, fn func(lastRun map[string]time.Time)) error {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("cleanup: acquire conn: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	var locked bool
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock($1)`, cleanupLeaderLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("cleanup: try advisory lock: %w", err)
	}
	if !locked {
		return nil // another replica is the leader this tick
	}
	defer conn.ExecContext(ctx, //nolint:errcheck
		`SELECT pg_advisory_unlock($1)`, cleanupLeaderLockKey)

	lastRun, err := loadScheduleState(ctx, conn)
	if err != nil {
		return fmt.Errorf("cleanup: load schedule state: %w", err)
	}

	before := make(map[string]time.Time, len(lastRun))
	for k, v := range lastRun {
		before[k] = v
	}

	fn(lastRun)

	for repo, t := range lastRun {
		if prev, ok := before[repo]; ok && prev.Equal(t) {
			continue // unchanged
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO cleanup_schedule_state (repo, last_run)
			VALUES ($1, $2)
			ON CONFLICT (repo) DO UPDATE SET last_run = EXCLUDED.last_run
		`, repo, t.UTC()); err != nil {
			return fmt.Errorf("cleanup: persist schedule state for %q: %w", repo, err)
		}
	}
	return nil
}

// Snapshot returns the shared last-run time per repo name.
func (c *PGCoordinator) Snapshot(ctx context.Context) (map[string]time.Time, error) {
	return loadScheduleState(ctx, c.db)
}

// rowQuerier is satisfied by both *sql.DB and *sql.Conn.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func loadScheduleState(ctx context.Context, q rowQuerier) (map[string]time.Time, error) {
	rows, err := q.QueryContext(ctx, `SELECT repo, last_run FROM cleanup_schedule_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[string]time.Time{}
	for rows.Next() {
		var repo string
		var t time.Time
		if err := rows.Scan(&repo, &t); err != nil {
			return nil, err
		}
		out[repo] = t
	}
	return out, rows.Err()
}
