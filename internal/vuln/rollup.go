package vuln

import "time"

// Rollup is a per-repo summary of findings, persisted at scan time so list
// views (Browse, search, admin table, dashboard, gauge) read it in O(1) via
// GetRollup instead of re-scanning every finding on each render. It is
// recomputed authoritatively whenever a repo is scanned; an artifact deletion
// between scans may leave it briefly stale (a vulnerable badge lingers until the
// next scan) — an accepted, documented eventual-consistency trade.
//
// Only *vulnerable* components/versions (≥1 advisory) appear in the maps. Clean
// (scanned, no advisories) and unscanned components are simply absent, so a
// surface badges exactly the keys present and shows nothing otherwise.
type Rollup struct {
	Repo string `json:"repo"`
	// VulnerableCount is the number of distinct components with ≥1 advisory.
	VulnerableCount int `json:"vulnerableCount"`
	// BySeverity is the histogram of worst-severity across vulnerable components
	// (each vulnerable component counts once, in its worst bucket). Keyed by the
	// severity label; sums to VulnerableCount.
	BySeverity map[string]int `json:"bySeverity,omitempty"`
	// WorstByComponent maps a component to its worst severity across all versions.
	WorstByComponent map[string]Severity `json:"worstByComponent,omitempty"`
	// WorstByVersion maps component → version → that version's worst severity.
	// Nested (rather than a "component@version" join) to stay collision-free for
	// names/versions that themselves contain "@" (e.g. scoped npm).
	WorstByVersion map[string]map[string]Severity `json:"worstByVersion,omitempty"`
	ComputedAt     time.Time                      `json:"computedAt"`
}

// BuildRollup computes a Rollup for repo from findings. Pure (no I/O) so callers
// that already hold the findings — e.g. a scan that just wrote them — avoid a
// re-read; ComputeAndPutRollup is the List-then-build convenience for callers
// that don't.
func BuildRollup(repo string, findings []Finding) Rollup {
	r := Rollup{Repo: repo, ComputedAt: time.Now().UTC()}
	for _, f := range findings {
		if len(f.Advisories) == 0 {
			continue // clean: scanned, no advisories
		}
		w := f.Worst() // may be SeverityUnknown (vulnerable but unscored)

		if r.WorstByVersion == nil {
			r.WorstByVersion = map[string]map[string]Severity{}
		}
		vers := r.WorstByVersion[f.Component]
		if vers == nil {
			vers = map[string]Severity{}
			r.WorstByVersion[f.Component] = vers
		}
		if cur, ok := vers[f.Version]; !ok || w > cur {
			vers[f.Version] = w
		}

		if r.WorstByComponent == nil {
			r.WorstByComponent = map[string]Severity{}
		}
		// Set on first sight (presence is what marks "vulnerable"), then keep the
		// max — so a component with only unscored advisories is still recorded.
		if cur, ok := r.WorstByComponent[f.Component]; !ok || w > cur {
			r.WorstByComponent[f.Component] = w
		}
	}

	for _, w := range r.WorstByComponent {
		if r.BySeverity == nil {
			r.BySeverity = map[string]int{}
		}
		r.BySeverity[w.String()]++
		r.VulnerableCount++
	}
	return r
}

// PutRollup persists r as repo's rollup. Stored in a separate meta namespace
// from findings ("{repo}:vuln-rollup") so List (which scans "{repo}:vuln")
// never mistakes the rollup document for a Finding.
func (s *Store) PutRollup(repo string, r Rollup) error {
	if r.ComputedAt.IsZero() {
		r.ComputedAt = time.Now().UTC()
	}
	return s.meta.PutJSON(rollupNS(repo), rollupKey, r)
}

// GetRollup returns repo's persisted rollup, or ok=false if none has been
// computed yet (repo never scanned).
func (s *Store) GetRollup(repo string) (Rollup, bool, error) {
	var r Rollup
	ok, err := s.meta.GetJSON(rollupNS(repo), rollupKey, &r)
	return r, ok, err
}

// ComputeAndPutRollup rebuilds repo's rollup from all stored findings and
// persists it. Used by the startup sweep and the post-deletion recompute, where
// the caller doesn't already hold the findings in memory.
func (s *Store) ComputeAndPutRollup(repo string) (Rollup, error) {
	findings, err := s.List(repo)
	if err != nil {
		return Rollup{}, err
	}
	r := BuildRollup(repo, findings)
	if err := s.PutRollup(repo, r); err != nil {
		return Rollup{}, err
	}
	return r, nil
}

// ComponentSeverity returns the worst-severity label for a component, or "" if
// the component has no findings (not vulnerable → surfaces render no badge). A
// present-but-unscored component returns "unknown" (a grey badge), distinct
// from absent — that distinction is why the maps omit clean components rather
// than relying on the zero value.
func (r Rollup) ComponentSeverity(component string) string {
	if s, ok := r.WorstByComponent[component]; ok {
		return s.String()
	}
	return ""
}

// VersionSeverity returns the worst-severity label for one component@version, or
// "" if that version has no findings.
func (r Rollup) VersionSeverity(component, version string) string {
	if vs, ok := r.WorstByVersion[component]; ok {
		if s, ok := vs[version]; ok {
			return s.String()
		}
	}
	return ""
}

// WorstSeverity returns the highest severity label across all vulnerable
// components in the repo, or "" if none are vulnerable. Used for the single
// repo-level badge in the admin table / dashboard.
func (r Rollup) WorstSeverity() string {
	worst := SeverityUnknown
	seen := false
	for _, s := range r.WorstByComponent {
		if !seen || s > worst {
			worst, seen = s, true
		}
	}
	if !seen {
		return ""
	}
	return worst.String()
}

func rollupNS(repo string) string { return repo + ":vuln-rollup" }

const rollupKey = "rollup"
