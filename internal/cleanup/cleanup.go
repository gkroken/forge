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
func Run(repoName, format string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	if p == nil {
		return Result{}, nil
	}
	switch format {
	case "maven":
		return runMaven(repoName, p, b, m)
	case "cran":
		return runCRAN(repoName, p, b, m)
	case "helm":
		return runHelm(repoName, p, b, m)
	case "npm":
		return runNPM(repoName, p, b, m)
	}
	return Result{}, nil
}

// ── Maven ─────────────────────────────────────────────────────────────────────

func runMaven(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	keys, err := b.List(repoName + "/")
	if err != nil {
		return Result{}, err
	}

	// Group blob keys by {groupId}/{artifactId}: the path up to (not including)
	// the version directory (second-to-last path component).
	type artifact struct {
		version string
		keys    []string // all blob keys in this version directory
	}
	byGA := map[string][]artifact{} // ga → []artifact
	gaVer := map[string]map[string]*artifact{}

	prefix := repoName + "/"
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		parts := strings.Split(rel, "/")
		if len(parts) < 3 {
			continue // need at least groupId/artifactId/version
		}
		version := parts[len(parts)-2]
		ga := strings.Join(parts[:len(parts)-2], "/")

		if gaVer[ga] == nil {
			gaVer[ga] = map[string]*artifact{}
		}
		if gaVer[ga][version] == nil {
			a := &artifact{version: version}
			gaVer[ga][version] = a
			byGA[ga] = append(byGA[ga], *a)
		}
		gaVer[ga][version].keys = append(gaVer[ga][version].keys, k)
	}

	// Rebuild byGA from gaVer (pointer indirection fix).
	byGA = map[string][]artifact{}
	for ga, vers := range gaVer {
		for ver, a := range vers {
			_ = ver
			byGA[ga] = append(byGA[ga], *a)
		}
	}

	var res Result
	snapNS := repoName + ":maven:snap:v"

	for ga, arts := range byGA {
		_ = ga
		// Apply KeepReleasesOnly: collect SNAPSHOT versions to delete.
		var toDelete []artifact
		var kept []artifact
		for _, a := range arts {
			if p.KeepReleasesOnly && isSnapshotVersion(a.version) {
				toDelete = append(toDelete, a)
			} else {
				kept = append(kept, a)
			}
		}
		arts = kept

		// Apply DeleteSnapshotsDays: delete old SNAPSHOT versions.
		if p.DeleteSnapshotsDays > 0 {
			cutoff := time.Now().UTC().AddDate(0, 0, -p.DeleteSnapshotsDays)
			var remaining []artifact
			for _, a := range arts {
				if !isSnapshotVersion(a.version) {
					remaining = append(remaining, a)
					continue
				}
				// Look up upload time from any snap record for this version.
				snapTime := mavenSnapUploadTime(snapNS, a.version, a.keys, m)
				if !snapTime.IsZero() && snapTime.Before(cutoff) {
					toDelete = append(toDelete, a)
				} else {
					remaining = append(remaining, a)
				}
			}
			arts = remaining
		}

		// Apply DeleteOlderThanDays: same logic but for all artifact types.
		if p.DeleteOlderThanDays > 0 {
			cutoff := time.Now().UTC().AddDate(0, 0, -p.DeleteOlderThanDays)
			var remaining []artifact
			for _, a := range arts {
				snapTime := mavenSnapUploadTime(snapNS, a.version, a.keys, m)
				if !snapTime.IsZero() && snapTime.Before(cutoff) {
					toDelete = append(toDelete, a)
				} else {
					remaining = append(remaining, a)
				}
			}
			arts = remaining
		}

		// Apply LastDownloadedDays: delete versions whose last download (or
		// upload time when never downloaded) is older than the cutoff.
		if p.LastDownloadedDays > 0 {
			cutoff := time.Now().UTC().AddDate(0, 0, -p.LastDownloadedDays)
			var remaining []artifact
			for _, a := range arts {
				eff := effectiveDownloadTime(
					lastDownloadTime(m, a.keys...),
					mavenSnapUploadTime(snapNS, a.version, a.keys, m),
				)
				if !eff.IsZero() && eff.Before(cutoff) {
					toDelete = append(toDelete, a)
				} else {
					remaining = append(remaining, a)
				}
			}
			arts = remaining
		}

		// Apply KeepVersions: sort remaining versions and drop oldest.
		if p.KeepVersions > 0 && len(arts) > p.KeepVersions {
			sorted := make([]artifact, len(arts))
			copy(sorted, arts)
			for i := 1; i < len(sorted); i++ {
				for j := i; j > 0 && compareVersions(sorted[j].version, sorted[j-1].version) < 0; j-- {
					sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
				}
			}
			toDelete = append(toDelete, sorted[:len(sorted)-p.KeepVersions]...)
		}

		// Execute deletions.
		for _, a := range toDelete {
			for _, k := range a.keys {
				info, exists, _ := b.Stat(k)
				if exists {
					res.FreedBytes += info.Size
					b.Delete(k) //nolint:errcheck
					res.Deleted++
				}
			}
		}
	}
	return res, nil
}

// mavenSnapUploadTime returns the earliest UploadedAt timestamp found in snap
// meta records for the given SNAPSHOT version path prefix.
func mavenSnapUploadTime(snapNS, version string, blobKeys []string, m meta.Store) time.Time {
	_ = version
	allKeys, _ := m.List(snapNS)
	for _, k := range blobKeys {
		prefix := strings.TrimPrefix(k, "/")
		// key format: "{snapshotPath}:{ext}:"
		for _, mk := range allKeys {
			if strings.HasPrefix(mk, prefix+":") {
				var rec struct {
					UploadedAt time.Time `json:"uploadedAt"`
				}
				if ok, _ := m.GetJSON(snapNS, mk, &rec); ok && !rec.UploadedAt.IsZero() {
					return rec.UploadedAt
				}
			}
		}
	}
	return time.Time{}
}

// ── CRAN ──────────────────────────────────────────────────────────────────────

type cranRecord struct {
	Package    string    `json:"package"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func runCRAN(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	ns := repoName + ":cran"
	keys, err := m.List(ns)
	if err != nil {
		return Result{}, err
	}

	byPkg := map[string][]cranRecord{}
	for _, k := range keys {
		var rec cranRecord
		if ok, _ := m.GetJSON(ns, k, &rec); !ok {
			continue
		}
		byPkg[rec.Package] = append(byPkg[rec.Package], rec)
	}

	var res Result
	for _, recs := range byPkg {
		toDelete := applyPolicies(p, recs,
			func(r cranRecord) string    { return r.Version },
			func(r cranRecord) time.Time { return r.UploadedAt },
			func(r cranRecord) time.Time {
				return lastDownloadTime(m, repoName+"/src/contrib/"+r.Package+"_"+r.Version+".tar.gz")
			},
		)
		for _, rec := range toDelete {
			blobKey := repoName + "/src/contrib/" + rec.Package + "_" + rec.Version + ".tar.gz"
			info, exists, _ := b.Stat(blobKey)
			if exists {
				res.FreedBytes += info.Size
				b.Delete(blobKey) //nolint:errcheck
			}
			m.Delete(ns, rec.Package+"_"+rec.Version) //nolint:errcheck
			res.Deleted++
		}
	}
	return res, nil
}

// ── Helm ──────────────────────────────────────────────────────────────────────

type helmRecord struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Filename   string    `json:"filename"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func runHelm(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	ns := repoName + ":helm"
	keys, err := m.List(ns)
	if err != nil {
		return Result{}, err
	}

	byChart := map[string][]helmRecord{}
	for _, k := range keys {
		var rec helmRecord
		if ok, _ := m.GetJSON(ns, k, &rec); !ok {
			continue
		}
		byChart[rec.Name] = append(byChart[rec.Name], rec)
	}

	var res Result
	for _, recs := range byChart {
		toDelete := applyPolicies(p, recs,
			func(r helmRecord) string    { return r.Version },
			func(r helmRecord) time.Time { return r.UploadedAt },
			func(r helmRecord) time.Time { return lastDownloadTime(m, repoName+"/"+r.Filename) },
		)
		for _, rec := range toDelete {
			blobKey := repoName + "/" + rec.Filename
			info, exists, _ := b.Stat(blobKey)
			if exists {
				res.FreedBytes += info.Size
				b.Delete(blobKey) //nolint:errcheck
			}
			m.Delete(ns, rec.Name+"-"+rec.Version) //nolint:errcheck
			res.Deleted++
		}
	}
	return res, nil
}

// ── npm ───────────────────────────────────────────────────────────────────────

type npmVersionRecord struct {
	Package    string    `json:"name"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func runNPM(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	versNS := repoName + ":npm:v"
	pkgNS := repoName + ":npm"

	keys, err := m.List(versNS)
	if err != nil {
		return Result{}, err
	}

	// Keys are "{pkg}:{version}".
	byPkg := map[string][]npmVersionRecord{}
	for _, k := range keys {
		pkg, ver, ok := strings.Cut(k, ":")
		if !ok {
			continue
		}
		byPkg[pkg] = append(byPkg[pkg], npmVersionRecord{Package: pkg, Version: ver})
	}

	var res Result
	for _, recs := range byPkg {
		toDelete := applyPolicies(p, recs,
			func(r npmVersionRecord) string    { return r.Version },
			func(r npmVersionRecord) time.Time { return r.UploadedAt },
			func(r npmVersionRecord) time.Time {
				return lastDownloadTime(m, repoName+"/"+r.Package+"/-/"+r.Package+"-"+r.Version+".tgz")
			},
		)
		for _, rec := range toDelete {
			blobKey := repoName + "/" + rec.Package + "/-/" + rec.Package + "-" + rec.Version + ".tgz"
			info, exists, _ := b.Stat(blobKey)
			if exists {
				res.FreedBytes += info.Size
				b.Delete(blobKey) //nolint:errcheck
			}
			m.Delete(versNS, rec.Package+":"+rec.Version) //nolint:errcheck

			// Remove the version from the packument.
			var packument map[string]any
			if ok, _ := m.GetJSON(pkgNS, rec.Package, &packument); ok {
				if vers, ok := packument["versions"].(map[string]any); ok {
					delete(vers, rec.Version)
					packument["versions"] = vers
				}
				m.PutJSON(pkgNS, rec.Package, packument) //nolint:errcheck
			}
			res.Deleted++
		}
	}
	return res, nil
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

