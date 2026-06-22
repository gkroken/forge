package cleanup

import (
	"context"
	"sync"
	"time"
)

// Coordinator decides whether this replica may run scheduled cleanup on a given
// tick and owns the shared last-run state. It exists to make scheduled cleanup
// fire exactly once across N replicas.
//
// Two implementations, gated on POSTGRES_DSN exactly like queue.NewPG/NewMem:
//   - localCoordinator: single-node (eval/FS mode). Always the leader; lastRun is
//     an in-memory map — today's behavior, unchanged.
//   - PGCoordinator: multi-replica (Postgres mode). A pg_try_advisory_lock makes
//     exactly one replica the leader per tick; lastRun is persisted in Postgres so
//     leadership can move between replicas without re-firing a due job.
type Coordinator interface {
	// RunExclusive invokes fn while holding the cluster cleanup-leader lock,
	// passing it the shared lastRun map; any entries fn adds or updates are
	// persisted before the lock is released. If another replica currently holds
	// the lock, fn is not called and this tick is a no-op. fn must not retain the
	// map after it returns.
	RunExclusive(ctx context.Context, fn func(lastRun map[string]time.Time)) error

	// Snapshot returns a copy of the shared last-run time per repo name, for the
	// scheduled-tasks UI.
	Snapshot(ctx context.Context) (map[string]time.Time, error)
}

// localCoordinator is the single-node coordinator: it is always the leader and
// keeps lastRun in memory. This reproduces the scheduler's original behavior for
// eval/FS mode, where there is exactly one process.
type localCoordinator struct {
	mu      sync.RWMutex
	lastRun map[string]time.Time
}

func newLocalCoordinator() *localCoordinator {
	return &localCoordinator{lastRun: map[string]time.Time{}}
}

func (c *localCoordinator) RunExclusive(_ context.Context, fn func(lastRun map[string]time.Time)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.lastRun)
	return nil
}

func (c *localCoordinator) Snapshot(_ context.Context) (map[string]time.Time, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := make(map[string]time.Time, len(c.lastRun))
	for k, v := range c.lastRun {
		cp[k] = v
	}
	return cp, nil
}
