// Package cleanup implements artifact retention policies for hosted repositories.
//
// Named policies are managed via PolicyManager and stored in meta.Store.
// Call Run to apply a policy; call DryRun to preview what would be deleted.
// The four rule types are:
//
//   - KeepVersions       — retain only the N most recent versions per artifact
//   - KeepReleasesOnly   — delete all SNAPSHOT / pre-release versions
//   - DeleteSnapshotsDays — delete SNAPSHOT/pre-release versions older than N days
//   - DeleteOlderThanDays — delete any artifact older than N days
//
// Timestamp-based rules only apply to artifacts published after UploadedAt
// tracking was introduced; artifacts without a stored timestamp are skipped.
package cleanup

import (
	"errors"
	"fmt"
	"forge/internal/format"
	"strconv"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Result reports how many artifacts were removed and how many bytes were freed.
type Result struct {
	Deleted    int   `json:"deleted"`
	FreedBytes int64 `json:"freed_bytes"`
}

// Run applies p against repoName's blob and meta stores. Returns an empty
// result immediately if p is nil.
func Run(r repo.Repository, res Resolver, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	if p == nil {
		return Result{}, nil
	}
	h, c, ok := resolve(r, res, b, m)
	if ok {
		if out, err := runGeneric(h, c, p, b, m); !errors.Is(err, format.ErrNotSupported) {
			return out, err
		}
	}
	return Result{}, fmt.Errorf("cleanup: no retention for format %q", r.Format)
}

// resolve builds the handler + context retention needs, when one is available.
func resolve(r repo.Repository, res Resolver, b blob.Store, m meta.Store) (format.Handler, *format.Context, bool) {
	if res == nil {
		return nil, nil, false
	}
	h, ok := res.For(r.Format)
	if !ok {
		return nil, nil, false
	}
	return h, &format.Context{Repo: r, Blob: b, Meta: m}, true
}

// ── Maven ─────────────────────────────────────────────────────────────────────

// mavenSnapUploadTime returns the earliest UploadedAt timestamp found in snap
// meta records for the given SNAPSHOT version path prefix.

// ── CRAN ──────────────────────────────────────────────────────────────────────

type cranRecord struct {
	Package    string    `json:"package"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

// ── Helm ──────────────────────────────────────────────────────────────────────

type helmRecord struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Filename   string    `json:"filename"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

// ── npm ───────────────────────────────────────────────────────────────────────

type npmVersionRecord struct {
	Package    string    `json:"name"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

// ── shared policy helpers ─────────────────────────────────────────────────────

// applyPolicies returns the subset of records that should be deleted according
// to the policy. version() and uploadedAt() are accessors for the record type.
func applyPolicies[T any](
	p *repo.CleanupPolicy,
	recs []T,
	version func(T) string,
	uploadedAt func(T) time.Time,
	downloadedAt func(T) time.Time,
) []T {
	var toDelete []T
	kept := make([]T, 0, len(recs))

	now := time.Now().UTC()

	for _, r := range recs {
		ver := version(r)
		isSnap := isSnapshotVersion(ver)
		ua := uploadedAt(r)

		deleted := false

		if p.KeepReleasesOnly && isSnap {
			toDelete = append(toDelete, r)
			deleted = true
		}
		if !deleted && p.DeleteSnapshotsDays > 0 && isSnap && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteSnapshotsDays)) {
				toDelete = append(toDelete, r)
				deleted = true
			}
		}
		if !deleted && p.DeleteOlderThanDays > 0 && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteOlderThanDays)) {
				toDelete = append(toDelete, r)
				deleted = true
			}
		}
		if !deleted && p.LastDownloadedDays > 0 {
			if eff := effectiveDownloadTime(downloadedAt(r), ua); !eff.IsZero() &&
				eff.Before(now.AddDate(0, 0, -p.LastDownloadedDays)) {
				toDelete = append(toDelete, r)
				deleted = true
			}
		}
		if !deleted {
			kept = append(kept, r)
		}
	}

	// KeepVersions: sort remaining by version string and drop oldest.
	if p.KeepVersions > 0 && len(kept) > p.KeepVersions {
		sorted := make([]T, len(kept))
		copy(sorted, kept)
		// Semver-aware sort: 1.10.0 must outrank 1.9.0 so KeepVersions never
		// prunes a numerically-higher (e.g. just-published) version.
		for i := 1; i < len(sorted); i++ {
			for j := i; j > 0 && compareVersions(version(sorted[j]), version(sorted[j-1])) < 0; j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		// Drop everything before the last KeepVersions entries.
		toDelete = append(toDelete, sorted[:len(sorted)-p.KeepVersions]...)
	}

	return toDelete
}

// compareVersions orders version strings semver-aware: dotted numeric segments
// compare numerically (so 1.10.0 > 1.9.0), and a release outranks a pre-release
// of the same core version (1.0.0 > 1.0.0-rc1, 1.0 > 1.0-SNAPSHOT). Non-numeric
// segments fall back to lexicographic comparison. Returns -1, 0, or +1.
func compareVersions(a, b string) int {
	ac, ap := splitPreRelease(a)
	bc, bp := splitPreRelease(b)
	if c := compareDotted(ac, bc); c != 0 {
		return c
	}
	// Core equal: a release (no pre-release suffix) outranks a pre-release.
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return compareDotted(ap, bp)
}

// splitPreRelease separates the core version from a pre-release suffix at the
// first '-' (e.g. "1.0.0-rc1" → "1.0.0", "rc1"; "1.0-SNAPSHOT" → "1.0", "SNAPSHOT").
func splitPreRelease(v string) (core, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// compareDotted compares two dot-separated strings segment by segment. Segments
// that are both numeric compare numerically; otherwise lexicographically. A
// missing segment ranks lower (1.0 < 1.0.1). Returns -1, 0, or +1.
func compareDotted(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x == y {
			continue
		}
		if x == "" {
			return -1
		}
		if y == "" {
			return 1
		}
		if xn, xe := strconv.Atoi(x); xe == nil {
			if yn, ye := strconv.Atoi(y); ye == nil {
				if xn < yn {
					return -1
				}
				return 1
			}
		}
		if x < y {
			return -1
		}
		return 1
	}
	return 0
}

// isSnapshotVersion reports whether a version string represents a pre-release.
// Matches Maven SNAPSHOT convention and common npm pre-release patterns.
func isSnapshotVersion(version string) bool {
	upper := strings.ToUpper(version)
	return strings.Contains(upper, "SNAPSHOT") ||
		strings.Contains(version, "-alpha") ||
		strings.Contains(version, "-beta") ||
		strings.Contains(version, "-rc") ||
		strings.Contains(version, "-dev")
}
