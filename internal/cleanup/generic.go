package cleanup

import (
	"sort"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/ledger"
	"forge/internal/meta"
	"forge/internal/repo"
)

// The generic retention pass.
//
// Every format used to carry its own copy of "enumerate versions, apply the
// rules, delete what the rules picked" — five near-identical functions per
// operation, against a policy engine of about sixty lines. Only the enumerating
// and the deleting are actually format-specific, and those now live behind
// format.Handler, so the rules run once here for every format including ones
// that do not exist yet.

// Resolver hands retention the handler for a repository's format. *format.Registry
// satisfies it; a test can pass a stub. It exists so internal/cleanup can dispatch
// through the interface without depending on how handlers are registered.
type Resolver interface {
	For(format string) (format.Handler, bool)
}

// versionInfo is a format.Version enriched with what retention derives
// generically: size, download time, and a publish time resolved through the
// ledger when the format keeps none of its own.
type versionInfo struct {
	v          format.Version
	sizeBytes  int64
	published  time.Time
	downloaded time.Time
}

// collect enumerates a repository's versions and fills in the derived fields.
func collect(h format.Handler, c *format.Context, b blob.Store, m meta.Store) (map[string][]versionInfo, error) {
	versions, err := h.ListVersions(c)
	if err != nil {
		return nil, err
	}
	pub := ledger.Load(m, c.Repo.Name)
	byComponent := map[string][]versionInfo{}
	for _, v := range versions {
		vi := versionInfo{
			v:          v,
			published:  ledger.Resolve(v.PublishedAt, pub, v.Component, v.Version),
			downloaded: lastDownloadTime(m, v.BlobKeys...),
		}
		if v.SizeBytes > 0 {
			vi.sizeBytes = v.SizeBytes
		} else {
			for _, k := range v.BlobKeys {
				vi.sizeBytes += statSize(b, k)
			}
		}
		byComponent[v.Component] = append(byComponent[v.Component], vi)
	}
	return byComponent, nil
}

func viVersion(vi versionInfo) string       { return vi.v.Version }
func viPublished(vi versionInfo) time.Time  { return vi.published }
func viDownloaded(vi versionInfo) time.Time { return vi.downloaded }

// runGeneric applies p to every component in the repository.
func runGeneric(h format.Handler, c *format.Context, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	byComponent, err := collect(h, c, b, m)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, component := range sortedKeys(byComponent) {
		for _, vi := range applyPolicies(p, byComponent[component], viVersion, viPublished, viDownloaded) {
			freed, err := h.DeleteVersion(c, vi.v.Component, vi.v.Version)
			if err != nil {
				return res, err
			}
			ledger.Forget(m, c.Repo.Name, vi.v.Component, vi.v.Version)
			res.FreedBytes += freed
			res.Deleted++
		}
	}
	return res, nil
}

// dryRunGeneric reports what runGeneric would delete, and what it could not
// judge for lack of a publish time.
func dryRunGeneric(h format.Handler, c *format.Context, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	byComponent, err := collect(h, c, b, m)
	if err != nil {
		return DryRunResult{}, err
	}
	// Components in a stable order: a preview that reshuffles between identical
	// runs is hard to diff and hard to trust.
	result := DryRunResult{Candidates: []Candidate{}}
	for _, component := range sortedKeys(byComponent) {
		cands, skipped := applyPoliciesTagged(p, byComponent[component], viVersion, viPublished, viDownloaded)
		for _, vi := range skipped {
			result.Unevaluable = append(result.Unevaluable, Unevaluable{
				Component: vi.v.Component, Version: vi.v.Version, Rule: "delete_older_than_days",
			})
		}
		for _, t := range cands {
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.v.Component,
				Version:   t.rec.v.Version,
				SizeBytes: t.rec.sizeBytes,
				AgeDays:   blobAgeDays(t.at),
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}

// sortedKeys gives map iteration a defined order, so runs and previews are
// reproducible.
func sortedKeys(m map[string][]versionInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
