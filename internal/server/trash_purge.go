package server

import (
	"log/slog"
	"time"

	"forge/internal/cleanup"
)

// trashPurgeInterval is how often the retention sweep runs (once a day, like the
// vuln re-scan). The sweep itself hard-deletes tombstones older than
// s.TrashRetention.
const trashPurgeInterval = 24 * time.Hour

// trashPurgeKey namespaces the sweep's last-run timestamp in the scheduler's
// shared lastRun map, separate from the per-repo cleanup/vuln keys.
const trashPurgeKey = "__trash-purge__"

// TrashPurgeTick is a scheduler tick hook (registered via WithTickHook, chained
// with VulnRescanTick). It runs inside the cleanup leader lock with the shared,
// persisted lastRun map — so it fires exactly once across replicas and remembers
// the last sweep across leadership changes. It hard-deletes soft-deleted
// artifacts whose age exceeds s.TrashRetention.
func (s *Server) TrashPurgeTick(now time.Time, lastRun map[string]time.Time) {
	if s.TrashRetention <= 0 {
		return // retention disabled: trash is kept until manually purged
	}
	if now.Sub(lastRun[trashPurgeKey]) < trashPurgeInterval {
		return
	}
	purged, freed, err := cleanup.PurgeExpired(s.Meta, s.Blob, s.TrashRetention)
	if err != nil {
		slog.Warn("trash: retention purge failed", "err", err)
		return
	}
	lastRun[trashPurgeKey] = now
	if purged > 0 {
		slog.Info("trash: retention purge", "purged", purged, "freedBytes", freed)
		s.triggerWalk()
	}
}
