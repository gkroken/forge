package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/webhook"
)

const bytesPerGB = 1 << 30

// quotaBlocks enforces a hosted repository's storage quota on a write. It returns
// true only when the write was refused — a 507 Insufficient Storage has been
// written and the caller must stop serving the request.
//
// Only HOSTED repositories are gated. Proxy cache-fills are deliberately never
// blocked: refusing to cache an upstream dependency would break a build over a
// transitive artifact the caller never chose to store, and proxy growth is
// managed by cache eviction instead (see cleanup.EvictProxyCache). Group repos
// own no storage of their own.
//
// Accounting is a SOFT limit. Usage is the last periodic blob walk
// (GetBlobSizes) plus an in-process count of bytes written since that walk
// (quotaDelta). The walk is re-triggered after every write, so the base is fresh
// within one walk cycle; the delta bounds how far a burst of concurrent uploads
// can overrun the quota before the next walk reconciles it. The limit is
// therefore approximate at the margin and exact within one walk cycle — the
// right trade for a store this size (no write-path ledger, no double-stat per
// upload). A single write that finds usage under the quota is always allowed to
// complete, even if it crosses the line; the next write is refused.
func (s *Server) quotaBlocks(w http.ResponseWriter, r *http.Request, rp repo.Repository) bool {
	if rp.Kind != repo.Hosted || rp.QuotaGB == nil || *rp.QuotaGB <= 0 {
		return false
	}
	quotaBytes := int64(*rp.QuotaGB * bytesPerGB)
	used := s.usedBytes(rp.Name)
	// Fold in the declared body size when the client sends one, so a single large
	// upload can't silently blow far past the quota in one shot. Unknown length
	// (-1) or chunked bodies fall back to the pure soft check.
	incoming := r.ContentLength
	if incoming < 0 {
		incoming = 0
	}
	if used+incoming <= quotaBytes {
		return false
	}
	s.recordQuotaBlock(r, rp, used, quotaBytes)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInsufficientStorage) // 507
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      "storage quota exceeded",
		"repo":       rp.Name,
		"usedBytes":  used,
		"quotaBytes": quotaBytes,
		"detail":     "repository is at or over its configured storage quota; delete artifacts (they go to trash and free quota immediately) or raise the quota",
	})
	return true
}

// usedBytes reports a repository's current storage usage: the last blob-walk
// snapshot plus the in-flight delta of bytes written since that walk.
func (s *Server) usedBytes(name string) int64 {
	return s.GetBlobSizes().ByRepo[name] + s.quotaDeltaValue(name)
}

// quotaDeltaValue reads the in-flight byte delta for a repo (0 if none tracked).
func (s *Server) quotaDeltaValue(name string) int64 {
	if v, ok := s.quotaDelta.Load(name); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// addQuotaDelta records n bytes written to a repo since the last blob walk. The
// middleware calls this after a successful hosted write; walkBlobSizes reconciles
// it back to zero once the bytes are reflected on disk.
func (s *Server) addQuotaDelta(name string, n int64) {
	v, _ := s.quotaDelta.LoadOrStore(name, new(atomic.Int64))
	v.(*atomic.Int64).Add(n)
}

// recordQuotaBlock records a refused write across the governance surfaces: the
// durable audit log, the policy.violation webhook (kind=quota), and the
// forge_quota_blocked_total metric. All side effects are best-effort and off the
// critical path of refusing the write.
func (s *Server) recordQuotaBlock(r *http.Request, rp repo.Repository, used, quota int64) {
	actor := actorLabel(r, s.Auth)
	if s.AuditLog != nil {
		s.AuditLog.Append(obs.AuditEntry{
			Timestamp: time.Now().UTC(),
			Actor:     actor,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    http.StatusInsufficientStorage,
			Detail:    "quota: blocked write to " + rp.Name + " (" + humanBytes(used) + " / " + humanBytes(quota) + ")",
		})
	}
	if s.Metrics != nil && s.Metrics.QuotaBlocked != nil {
		s.Metrics.QuotaBlocked.WithLabelValues(rp.Name).Inc()
	}
	if s.Webhooks != nil {
		ev := webhook.Event{
			Type:      webhook.EventPolicyViolation,
			Repo:      rp.Name,
			Format:    rp.Format,
			Path:      rp.Name,
			Actor:     actor,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"kind":       "quota",
				"usedBytes":  used,
				"quotaBytes": quota,
			},
		}
		go s.Webhooks.Dispatch(context.Background(), ev)
	}
}
