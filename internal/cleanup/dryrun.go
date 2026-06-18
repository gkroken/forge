package cleanup

import (
	"strings"
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

// DryRunResult lists the artifacts that would be removed without deleting them.
type DryRunResult struct {
	Candidates []Candidate `json:"candidates"`
}

// DryRun applies p against repoName's stores and returns what would be deleted,
// without performing any deletions. Returns an empty result if p is nil.
func DryRun(repoName, format string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	if p == nil {
		return DryRunResult{Candidates: []Candidate{}}, nil
	}
	switch format {
	case "cran":
		return dryRunCRAN(repoName, p, b, m)
	case "helm":
		return dryRunHelm(repoName, p, b, m)
	case "npm":
		return dryRunNPM(repoName, p, b, m)
	case "maven":
		return dryRunMaven(repoName, p, b)
	}
	return DryRunResult{Candidates: []Candidate{}}, nil
}

// tagged pairs a record with the rule that triggered its selection.
type tagged[T any] struct {
	rec    T
	reason string
}

// applyPoliciesTagged mirrors applyPolicies but labels each candidate with the
// rule that triggered deletion.
func applyPoliciesTagged[T any](
	p *repo.CleanupPolicy,
	recs []T,
	version func(T) string,
	uploadedAt func(T) time.Time,
) []tagged[T] {
	var toDelete []tagged[T]
	kept := make([]T, 0, len(recs))
	now := time.Now().UTC()

	for _, r := range recs {
		ver := version(r)
		isSnap := isSnapshotVersion(ver)
		ua := uploadedAt(r)
		deleted := false

		if p.KeepReleasesOnly && isSnap {
			toDelete = append(toDelete, tagged[T]{r, "keep_releases_only"})
			deleted = true
		}
		if !deleted && p.DeleteSnapshotsDays > 0 && isSnap && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteSnapshotsDays)) {
				toDelete = append(toDelete, tagged[T]{r, "delete_snapshots_days"})
				deleted = true
			}
		}
		if !deleted && p.DeleteOlderThanDays > 0 && !ua.IsZero() {
			if ua.Before(now.AddDate(0, 0, -p.DeleteOlderThanDays)) {
				toDelete = append(toDelete, tagged[T]{r, "delete_older_than_days"})
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
			for j := i; j > 0 && version(sorted[j]) < version(sorted[j-1]); j-- {
				sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			}
		}
		for _, r := range sorted[:len(sorted)-p.KeepVersions] {
			toDelete = append(toDelete, tagged[T]{r, "keep_versions"})
		}
	}

	return toDelete
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

func dryRunCRAN(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	ns := repoName + ":cran"
	keys, err := m.List(ns)
	if err != nil {
		return DryRunResult{}, err
	}

	byPkg := map[string][]cranRecord{}
	for _, k := range keys {
		var rec cranRecord
		if ok, _ := m.GetJSON(ns, k, &rec); !ok {
			continue
		}
		byPkg[rec.Package] = append(byPkg[rec.Package], rec)
	}

	var result DryRunResult
	for _, recs := range byPkg {
		for _, t := range applyPoliciesTagged(p, recs,
			func(r cranRecord) string    { return r.Version },
			func(r cranRecord) time.Time { return r.UploadedAt },
		) {
			blobKey := repoName + "/src/contrib/" + t.rec.Package + "_" + t.rec.Version + ".tar.gz"
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.Package,
				Version:   t.rec.Version,
				SizeBytes: statSize(b, blobKey),
				AgeDays:   blobAgeDays(t.rec.UploadedAt),
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}

// ── Helm ──────────────────────────────────────────────────────────────────────

func dryRunHelm(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	ns := repoName + ":helm"
	keys, err := m.List(ns)
	if err != nil {
		return DryRunResult{}, err
	}

	byChart := map[string][]helmRecord{}
	for _, k := range keys {
		var rec helmRecord
		if ok, _ := m.GetJSON(ns, k, &rec); !ok {
			continue
		}
		byChart[rec.Name] = append(byChart[rec.Name], rec)
	}

	var result DryRunResult
	for _, recs := range byChart {
		for _, t := range applyPoliciesTagged(p, recs,
			func(r helmRecord) string    { return r.Version },
			func(r helmRecord) time.Time { return r.UploadedAt },
		) {
			blobKey := repoName + "/" + t.rec.Filename
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.Name,
				Version:   t.rec.Version,
				SizeBytes: statSize(b, blobKey),
				AgeDays:   blobAgeDays(t.rec.UploadedAt),
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}

// ── npm ───────────────────────────────────────────────────────────────────────

func dryRunNPM(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	versNS := repoName + ":npm:v"
	keys, err := m.List(versNS)
	if err != nil {
		return DryRunResult{}, err
	}

	byPkg := map[string][]npmVersionRecord{}
	for _, k := range keys {
		pkg, ver, ok := strings.Cut(k, ":")
		if !ok {
			continue
		}
		byPkg[pkg] = append(byPkg[pkg], npmVersionRecord{Package: pkg, Version: ver})
	}

	var result DryRunResult
	for _, recs := range byPkg {
		for _, t := range applyPoliciesTagged(p, recs,
			func(r npmVersionRecord) string    { return r.Version },
			func(r npmVersionRecord) time.Time { return r.UploadedAt },
		) {
			blobKey := repoName + "/" + t.rec.Package + "/-/" + t.rec.Package + "-" + t.rec.Version + ".tgz"
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.Package,
				Version:   t.rec.Version,
				SizeBytes: statSize(b, blobKey),
				AgeDays:   blobAgeDays(t.rec.UploadedAt),
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}

// ── Maven ─────────────────────────────────────────────────────────────────────

func dryRunMaven(repoName string, p *repo.CleanupPolicy, b blob.Store) (DryRunResult, error) {
	keys, err := b.List(repoName + "/")
	if err != nil {
		return DryRunResult{}, err
	}

	type mavenArtifact struct {
		ga      string
		version string
		blobKeys []string
	}

	gaVer := map[string]map[string]*mavenArtifact{}
	prefix := repoName + "/"
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		parts := strings.Split(rel, "/")
		if len(parts) < 3 {
			continue
		}
		version := parts[len(parts)-2]
		ga := strings.Join(parts[:len(parts)-2], "/")
		if gaVer[ga] == nil {
			gaVer[ga] = map[string]*mavenArtifact{}
		}
		if gaVer[ga][version] == nil {
			gaVer[ga][version] = &mavenArtifact{ga: ga, version: version}
		}
		gaVer[ga][version].blobKeys = append(gaVer[ga][version].blobKeys, k)
	}

	// Flatten per-GA slices for applyPoliciesTagged.
	byGA := map[string][]mavenArtifact{}
	for ga, vers := range gaVer {
		for _, a := range vers {
			byGA[ga] = append(byGA[ga], *a)
		}
	}

	zero := time.Time{}
	var result DryRunResult
	for _, arts := range byGA {
		for _, t := range applyPoliciesTagged(p, arts,
			func(a mavenArtifact) string    { return a.version },
			func(a mavenArtifact) time.Time { return zero },
		) {
			var size int64
			for _, k := range t.rec.blobKeys {
				size += statSize(b, k)
			}
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.ga,
				Version:   t.rec.version,
				SizeBytes: size,
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}
