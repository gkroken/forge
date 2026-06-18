package cleanup

import (
	"context"
	"log/slog"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Scheduler runs cleanup policies on a per-repo cadence. Start it once at
// startup; it stops when ctx is cancelled (e.g. on SIGTERM).
type Scheduler struct {
	repos    *repo.Manager
	policies *PolicyManager
	blob     blob.Store
	meta     meta.Store
}

func NewScheduler(repos *repo.Manager, policies *PolicyManager, b blob.Store, m meta.Store) *Scheduler {
	return &Scheduler{repos: repos, policies: policies, blob: b, meta: m}
}

// Start runs the scheduler in a background goroutine. It checks every minute
// whether any repo's cleanup interval has elapsed and fires Run if so.
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	lastRun := map[string]time.Time{}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.RunDue(now, lastRun)
		}
	}
}

// RunDue checks every repo and fires Run for those whose interval has elapsed.
// lastRun is updated in-place. Exported for testing.
func (s *Scheduler) RunDue(now time.Time, lastRun map[string]time.Time) {
	for _, r := range s.repos.All() {
		if r.CleanupPolicyName == "" || r.Kind != repo.Hosted {
			continue
		}
		np, ok, err := s.policies.Get(r.CleanupPolicyName)
		if err != nil || !ok || np.Interval == 0 {
			continue
		}
		if now.Sub(lastRun[r.Name]) < np.Interval {
			continue
		}
		lastRun[r.Name] = now
		result, err := Run(r.Name, r.Format, np.ToCleanupPolicy(), s.blob, s.meta)
		if err != nil {
			slog.Error("cleanup: scheduled run failed", "repo", r.Name, "err", err)
			continue
		}
		if result.Deleted > 0 {
			slog.Info("cleanup: scheduled run complete",
				"repo", r.Name,
				"deleted", result.Deleted,
				"freed_bytes", result.FreedBytes,
			)
		}
	}
}
