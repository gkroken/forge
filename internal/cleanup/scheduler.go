package cleanup

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// publishCooldown is the minimum gap between two on-publish runs for the same
// repo. A single version push is many writes (.jar/.pom/.sha1…); this coalesces
// them into one cleanup run instead of one per file.
const publishCooldown = 30 * time.Second

// Scheduler runs cleanup policies on a per-repo cadence. Start it once at
// startup; it stops when ctx is cancelled (e.g. on SIGTERM). It also handles
// on-publish runs via Notify, debounced per repo by publishCooldown.
type Scheduler struct {
	repos    *repo.Manager
	policies *PolicyManager
	blob     blob.Store
	meta     meta.Store

	// coord gates scheduled runs and owns the shared lastRun state so a due job
	// fires exactly once across N replicas. Defaults to a single-node
	// localCoordinator; main.go swaps in a PGCoordinator when POSTGRES_DSN is set.
	coord Coordinator

	pubMu       sync.Mutex
	lastPublish map[string]time.Time // last on-publish run time per repo name
	now         func() time.Time     // injectable clock for tests

	// runHook, if set, is called after an automated run that deleted something.
	// Plain-typed so the cleanup package stays decoupled from webhooks; main.go
	// wires it to emit a cleanup.completed event.
	runHook func(RunEvent)
}

// RunEvent describes a completed automated cleanup run that removed artifacts.
type RunEvent struct {
	Repo       string
	Policy     string
	Deleted    int
	FreedBytes int64
	Trigger    string // "scheduled" | "on-publish"
}

func NewScheduler(repos *repo.Manager, policies *PolicyManager, b blob.Store, m meta.Store) *Scheduler {
	return &Scheduler{
		repos: repos, policies: policies, blob: b, meta: m,
		coord:       newLocalCoordinator(),
		lastPublish: map[string]time.Time{},
		now:         time.Now,
	}
}

// WithCoordinator replaces the default single-node coordinator. Call it with a
// PGCoordinator in multi-replica (Postgres) mode so scheduled cleanup is
// leader-gated and shares lastRun across pods. Returns s for chaining.
func (s *Scheduler) WithCoordinator(c Coordinator) *Scheduler {
	s.coord = c
	return s
}

// WithRunHook registers a callback invoked after an automated run (scheduled or
// on-publish) that deleted at least one artifact. Returns s for chaining.
func (s *Scheduler) WithRunHook(fn func(RunEvent)) *Scheduler {
	s.runHook = fn
	return s
}

// Start runs the scheduler in a background goroutine. It checks every minute
// whether any repo's cleanup interval has elapsed and fires Run if so.
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.Tick(ctx, now)
		}
	}
}

// Tick performs one scheduled-cleanup cycle: under the coordinator's leader
// lock, fire Run for every repo whose interval has elapsed, then persist the
// updated lastRun. If another replica holds the lock this tick is a no-op.
// Exported so tests can drive a tick without the minute ticker.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	if err := s.coord.RunExclusive(ctx, func(lastRun map[string]time.Time) {
		s.RunDue(now, lastRun)
	}); err != nil {
		slog.Warn("cleanup: scheduler tick failed", "err", err)
	}
}

// LastRuns returns a snapshot of the most recent scheduled run time per repo name.
func (s *Scheduler) LastRuns() map[string]time.Time {
	runs, err := s.coord.Snapshot(context.Background())
	if err != nil {
		slog.Warn("cleanup: load last runs failed", "err", err)
		return map[string]time.Time{}
	}
	return runs
}

// RunDue checks every repo and fires Run for those whose interval has elapsed.
// lastRun is updated in-place. Exported for testing.
func (s *Scheduler) RunDue(now time.Time, lastRun map[string]time.Time) {
	for _, r := range s.repos.All() {
		// Hosted repos get version retention; proxy repos get cache eviction.
		// Group repos own no storage.
		if r.CleanupPolicyName == "" || r.Kind == repo.Group {
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
		s.runOne(r, np, now, "scheduled")
	}
}

// Notify requests an on-publish cleanup run for repoName. It is safe to call
// from a request handler: it returns immediately and never runs cleanup inline.
// A run is scheduled only if the repo is hosted, has a policy with RunOnPublish
// set, and the per-repo cooldown has elapsed — coalescing a multi-file version
// push into a single run. The Run itself executes in a background goroutine.
// Returns true when a run was scheduled.
func (s *Scheduler) Notify(repoName string) bool {
	r, ok := s.repos.Get(repoName)
	if !ok || r.Kind != repo.Hosted || r.CleanupPolicyName == "" {
		return false
	}
	np, ok, err := s.policies.Get(r.CleanupPolicyName)
	if err != nil || !ok || !np.RunOnPublish {
		return false
	}
	now := s.now()
	s.pubMu.Lock()
	if last, seen := s.lastPublish[repoName]; seen && now.Sub(last) < publishCooldown {
		s.pubMu.Unlock()
		return false
	}
	s.lastPublish[repoName] = now
	s.pubMu.Unlock()

	go s.runOne(r, np, now, "on-publish")
	return true
}

// runOne applies np to repo r, records the run in history, and logs the outcome.
// trigger labels the log line ("scheduled" / "on-publish").
func (s *Scheduler) runOne(r repo.Repository, np NamedPolicy, ts time.Time, trigger string) {
	start := time.Now()
	result, err := RunForRepo(r, np.ToCleanupPolicy(), s.blob, s.meta)
	if err != nil {
		slog.Error("cleanup: "+trigger+" run failed", "repo", r.Name, "err", err)
		return
	}
	_ = RecordRun(s.meta, r.Name, CleanupRun{
		Timestamp:  ts,
		PolicyName: r.CleanupPolicyName,
		Deleted:    result.Deleted,
		FreedBytes: result.FreedBytes,
		DurationMs: time.Since(start).Milliseconds(),
	})
	if result.Deleted > 0 {
		slog.Info("cleanup: "+trigger+" run complete",
			"repo", r.Name,
			"deleted", result.Deleted,
			"freed_bytes", result.FreedBytes,
		)
		if s.runHook != nil {
			s.runHook(RunEvent{
				Repo: r.Name, Policy: r.CleanupPolicyName,
				Deleted: result.Deleted, FreedBytes: result.FreedBytes, Trigger: trigger,
			})
		}
	}
}
