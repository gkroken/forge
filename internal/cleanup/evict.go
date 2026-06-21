package cleanup

import (
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// RunForRepo applies p to r using the strategy appropriate to its kind:
// format-aware version retention for hosted repos, blob-TTL cache eviction for
// proxy repos. Group repos own no storage and are a no-op.
func RunForRepo(r repo.Repository, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	switch r.Kind {
	case repo.Proxy:
		return EvictProxyCache(r.Name, p, b, m)
	case repo.Hosted:
		return Run(r.Name, r.Format, p, b, m)
	}
	return Result{}, nil
}

// DryRunForRepo previews RunForRepo without deleting anything.
func DryRunForRepo(r repo.Repository, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	switch r.Kind {
	case repo.Proxy:
		return EvictProxyCacheDryRun(r.Name, p, b, m)
	case repo.Hosted:
		return DryRun(r.Name, r.Format, p, b, m)
	}
	return DryRunResult{Candidates: []Candidate{}}, nil
}

// EvictProxyCache deletes cached blobs whose most recent download is older than
// p.LastDownloadedDays. A proxy cache has no version semantics, so count-based
// and snapshot rules do not apply — only LastDownloadedDays is honored (it is
// the one rule a cache fill records, since every proxy GET stamps a download
// time). Eviction is blob-level: the format's cached metadata (packuments,
// indexes) is left in place and simply re-fetched from upstream on demand.
func EvictProxyCache(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	if p == nil || p.LastDownloadedDays <= 0 {
		return Result{}, nil
	}
	keys, err := b.List(repoName + "/")
	if err != nil {
		return Result{}, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -p.LastDownloadedDays)
	var res Result
	for _, k := range keys {
		dl := lastDownloadTime(m, k)
		if dl.IsZero() || !dl.Before(cutoff) {
			continue // never downloaded (can't age) or still warm
		}
		info, exists, _ := b.Stat(k)
		if !exists {
			continue
		}
		if err := b.Delete(k); err != nil {
			continue
		}
		res.FreedBytes += info.Size
		res.Deleted++
		_ = m.Delete(downloadNS, k) // drop the now-stale stamp
	}
	return res, nil
}

// EvictProxyCacheDryRun previews EvictProxyCache, listing the blobs that would
// be evicted without deleting anything.
func EvictProxyCacheDryRun(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	result := DryRunResult{Candidates: []Candidate{}}
	if p == nil || p.LastDownloadedDays <= 0 {
		return result, nil
	}
	keys, err := b.List(repoName + "/")
	if err != nil {
		return result, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -p.LastDownloadedDays)
	prefix := repoName + "/"
	for _, k := range keys {
		dl := lastDownloadTime(m, k)
		if dl.IsZero() || !dl.Before(cutoff) {
			continue
		}
		result.Candidates = append(result.Candidates, Candidate{
			Component: strings.TrimPrefix(k, prefix),
			SizeBytes: statSize(b, k),
			AgeDays:   blobAgeDays(dl),
			Reason:    "last_downloaded_days",
		})
	}
	return result, nil
}
