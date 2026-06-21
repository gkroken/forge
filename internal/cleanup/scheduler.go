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

	mu      sync.RWMutex
	lastRun map[string]time.Time // last scheduled run time per repo name

	pubMu       sync.Mutex
	lastPublish map[string]time.Time // last on-publish run time per repo name
	now         func() time.Time     // injectable clock for tests
}

func NewScheduler(repos *repo.Manager, policies *PolicyManager, b blob.Store, m meta.Store) *Scheduler {
	return &Scheduler{
		repos: repos, policies: policies, blob: b, meta: m,
		lastRun:     map[string]time.Time{},
		lastPublish: map[string]time.Time{},
		now:         time.Now,
	}
}

// Start runs the scheduler in a background goroutine. It checks every minute
// whether any repo's cleanup interval has elapsed and fires Run if so.
func (s *Scheduler) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Scheduler) loop(ctx context.Context) {
	local := map[string]time.Time{}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.RunDue(now, local)
			s.mu.Lock()
			for k, v := range local {
				s.lastRun[k] = v
			}
			s.mu.Unlock()
		}
	}
}

// LastRuns returns a snapshot of the most recent scheduled run time per repo name.
func (s *Scheduler) LastRuns() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string]time.Time, len(s.lastRun))
	for k, v := range s.lastRun {
		cp[k] = v
	}
	return cp
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
	result, err := Run(r.Name, r.Format, np.ToCleanupPolicy(), s.blob, s.meta)
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
	}
}
