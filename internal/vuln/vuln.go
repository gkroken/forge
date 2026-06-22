// Package vuln is the source-agnostic vulnerability-findings spine: a model and
// a store that know nothing about *how* a finding was produced. Producers (the
// OSV client for npm/Maven, a future Trivy sidecar for OCI, …) write Findings
// into the store; surfaces (browse badge, Security page, policy gate) read them.
// This mirrors forge's "shared spine, formats are plugins" invariant — the spine
// is durable, scanners are pluggable.
package vuln

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"forge/internal/meta"
)

// Source identifiers for the producer that wrote a Finding. A Finding carries
// its Source so multiple producers coexist per component without collision.
const (
	SourceOSV   = "osv"
	SourceTrivy = "trivy"
)

// Severity is an ordered severity bucket. Higher is worse, so findings compare
// with plain >. SeverityUnknown sorts lowest (unscored / unmapped advisories).
type Severity int

const (
	SeverityUnknown Severity = iota
	SeverityLow
	SeverityModerate
	SeverityHigh
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityLow:
		return "low"
	case SeverityModerate:
		return "moderate"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ParseSeverity maps a label (e.g. OSV database_specific.severity, "MODERATE")
// to a bucket. "medium" is accepted as an alias for moderate. Unrecognised →
// SeverityUnknown.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return SeverityLow
	case "moderate", "medium":
		return SeverityModerate
	case "high":
		return SeverityHigh
	case "critical":
		return SeverityCritical
	default:
		return SeverityUnknown
	}
}

// SeverityFromCVSS buckets a CVSS base score (0–10) per the standard qualitative
// ratings (CVSS v3.x). A score of 0 is treated as unscored.
func SeverityFromCVSS(score float64) Severity {
	switch {
	case score >= 9.0:
		return SeverityCritical
	case score >= 7.0:
		return SeverityHigh
	case score >= 4.0:
		return SeverityModerate
	case score > 0:
		return SeverityLow
	default:
		return SeverityUnknown
	}
}

// Severity marshals as its label string: readable on disk in FS eval mode and
// stable across reorderings of the underlying iota.
func (s Severity) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s *Severity) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	*s = ParseSeverity(str)
	return nil
}

// Advisory is one known vulnerability affecting a component version. The CVSS
// vector is kept raw alongside the derived Severity bucket so the UI and policy
// engine can compare without re-parsing.
type Advisory struct {
	ID       string   `json:"id"`                // primary identifier (OSV/CVE/GHSA)
	Aliases  []string `json:"aliases,omitempty"` // cross-references (CVE/GHSA)
	Summary  string   `json:"summary,omitempty"`
	Severity Severity `json:"severity"`
	CVSS     string   `json:"cvss,omitempty"`    // raw CVSS vector string, may be empty
	FixedIn  []string `json:"fixedIn,omitempty"` // versions that fix this advisory
	URL      string   `json:"url,omitempty"`
}

// Finding is the set of advisories known for one component@version from one
// Source, plus when it was scanned. Re-scans overwrite the prior Finding
// (advisories are published retroactively, so freshness matters — ScannedAt
// records it).
type Finding struct {
	Component  string     `json:"component"`
	Version    string     `json:"version"`
	Source     string     `json:"source"`
	Advisories []Advisory `json:"advisories,omitempty"`
	ScannedAt  time.Time  `json:"scannedAt"`
}

// Worst returns the highest severity among the finding's advisories
// (SeverityUnknown if there are none).
func (f Finding) Worst() Severity {
	worst := SeverityUnknown
	for _, a := range f.Advisories {
		if a.Severity > worst {
			worst = a.Severity
		}
	}
	return worst
}

// Store persists Findings under the meta namespace "{repo}:vuln", one document
// per component@version. It mirrors webhook.Store / cleanup.PolicyManager: a thin
// typed layer over meta.Store, backend-agnostic (FS in eval, Postgres in prod).
type Store struct{ meta meta.Store }

// NewStore returns a findings store backed by m.
func NewStore(m meta.Store) *Store { return &Store{meta: m} }

func nsFor(repo string) string { return repo + ":vuln" }

// keyFor builds the per-finding document key. The component and version are also
// stored as Finding fields, so callers reconstruct from the document — never by
// parsing this key (component names may themselves contain "@", e.g. scoped npm).
func keyFor(component, version string) string { return component + "@" + version }

// Put writes (or overwrites) the finding for f.Component@f.Version in repo. It is
// idempotent and re-scannable; a zero ScannedAt is stamped with the current time.
func (s *Store) Put(repo string, f Finding) error {
	if f.ScannedAt.IsZero() {
		f.ScannedAt = time.Now().UTC()
	}
	return s.meta.PutJSON(nsFor(repo), keyFor(f.Component, f.Version), f)
}

// Get returns the finding for component@version in repo, or ok=false if none.
func (s *Store) Get(repo, component, version string) (Finding, bool, error) {
	var f Finding
	ok, err := s.meta.GetJSON(nsFor(repo), keyFor(component, version), &f)
	return f, ok, err
}

// List returns all findings in repo, sorted by component then version.
func (s *Store) List(repo string) ([]Finding, error) {
	ns := nsFor(repo)
	keys, err := s.meta.List(ns)
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(keys))
	for _, k := range keys {
		var f Finding
		if ok, _ := s.meta.GetJSON(ns, k, &f); ok {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// Delete removes the finding for component@version in repo (no error if absent).
func (s *Store) Delete(repo, component, version string) error {
	return s.meta.Delete(nsFor(repo), keyFor(component, version))
}
