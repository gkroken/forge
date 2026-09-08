package cleanup

import (
	"errors"
	"fmt"
	"forge/internal/format"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Candidate is an artifact that would be deleted by a cleanup run.
type Candidate struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
	AgeDays   int    `json:"age_days"`
	Reason    string `json:"reason"`
}

// Unevaluable is a version that an age-based rule could not judge, because no
// publish time is known for it. Reporting these is the difference between a
// rule that is inert and a rule that is silently inert: without it, "no
// candidates" reads identically to "nothing is old enough yet".
type Unevaluable struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Rule      string `json:"rule"`
}

// DryRunResult lists the artifacts that would be removed without deleting them.
type DryRunResult struct {
	Candidates []Candidate `json:"candidates"`
	// Unevaluable is empty unless an age rule is configured and some version
	// has no known publish time.
	Unevaluable []Unevaluable `json:"unevaluable,omitempty"`
}

// DryRun applies p against repoName's stores and returns what would be deleted,
// without performing any deletions. Returns an empty result if p is nil.
func DryRun(r repo.Repository, res Resolver, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	if p == nil {
		return DryRunResult{Candidates: []Candidate{}}, nil
	}
	h, c, ok := resolve(r, res, b, m)
	if ok {
		if out, err := dryRunGeneric(h, c, p, b, m); !errors.Is(err, format.ErrNotSupported) {
			return out, err
		}
	}
	return DryRunResult{}, fmt.Errorf("cleanup: no retention for format %q", r.Format)
}

// tagged pairs a record with the rule that triggered its selection.
type tagged[T any] struct {
	rec    T
	reason string
	at     time.Time // resolved publish time; zero when unknown
}

// applyPoliciesTagged mirrors applyPolicies but labels each candidate with the
// rule that triggered deletion.
func applyPoliciesTagged[T any](
	p *repo.CleanupPolicy,
	recs []T,
	version func(T) string,
	uploadedAt func(T) time.Time,
	downloadedAt func(T) time.Time,
) ([]tagged[T], []T) {
	var toDelete []tagged[T]
	var unevaluable []T
	kept := make([]T, 0, len(recs))
	now := time.Now().UTC()
	// An age rule that cannot see a publish time skips the version rather than
	// assuming it is old. Track those so the caller can say so out loud.
	ageRule := p.DeleteOlderThanDays > 0 || p.DeleteSnapshotsDays > 0

	for _, r := range recs {
		ver := version(r)
		isSnap := isSnapshotVersion(ver)
		ua := uploadedAt(r)
		deleted := false
		if ageRule && ua.IsZero() {
			unevaluable = append(unevaluable, r)
		}

		if p.KeepReleasesOnly && isSnap {
			toDelete = append(toDelete, tagged[T]{r, "keep_releases_only", ua})
			deleted = true
		}
		if !deleted && p.DeleteSnapshotsDays > 0 && isSnap && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteSnapshotsDays)) {
				toDelete = append(toDelete, tagged[T]{r, "delete_snapshots_days", ua})
				deleted = true
			}
		}
		if !deleted && p.DeleteOlderThanDays > 0 && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteOlderThanDays)) {
				toDelete = append(toDelete, tagged[T]{r, "delete_older_than_days", ua})
				deleted = true
			}
		}
		if !deleted && p.LastDownloadedDays > 0 {
			if eff := effectiveDownloadTime(downloadedAt(r), ua); !eff.IsZero() &&
				eff.Before(now.AddDate(0, 0, -p.LastDownloadedDays)) {
				toDelete = append(toDelete, tagged[T]{r, "last_downloaded_days", ua})
				deleted = true
			}
		}
		if !deleted {
			kept = append(kept, r)
		}
	}

	if p.KeepVersions > 0 && len(kept) > p.KeepVersions {
		sorted := make([]T, len(kept))
		copy(sorted, kept)
		for i := 1; i < len(sorted); i++ {
			for j := i; j > 0 && compareVersions(version(sorted[j]), version(sorted[j-1])) < 0; j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		for _, r := range sorted[:len(sorted)-p.KeepVersions] {
			toDelete = append(toDelete, tagged[T]{r, "keep_versions", uploadedAt(r)})
		}
	}

	return toDelete, unevaluable
}

func blobAgeDays(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	return int(time.Since(t).Hours() / 24)
}

func statSize(b blob.Store, key string) int64 {
	info, exists, _ := b.Stat(key)
	if !exists {
		return 0
	}
	return info.Size
}

// ── CRAN ──────────────────────────────────────────────────────────────────────

// ── Helm ──────────────────────────────────────────────────────────────────────

// ── npm ───────────────────────────────────────────────────────────────────────

// ── Maven ─────────────────────────────────────────────────────────────────────
