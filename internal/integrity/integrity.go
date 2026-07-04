// Package integrity defines the read-only store-verification model: finding
// kinds, per-run reports, and their persistence.
//
// The verify job proves a repository's storage is internally consistent —
// every metadata record is backed by real bytes, every blob is owned by a
// record, and every stored checksum still matches what is on disk. It is the
// tool that proves a migrated store intact. It REPORTS ONLY; it never
// repairs, deletes, or rewrites anything.
//
// Format-specific knowledge (what counts as an orphan for npm vs maven) lives
// in each format plugin behind the optional format.IntegrityChecker seam.
// This package holds only the shared vocabulary, the report store, and
// helpers usable by any plugin. It must not import format (format imports it).
package integrity

import (
	"sort"
	"time"

	"forge/internal/meta"
)

// Finding kinds. The vocabulary is deliberately small and format-neutral.
const (
	// KindMissing — a metadata record (or manifest/index reference) points at
	// a blob that does not exist. Content a client could ask for is gone.
	KindMissing = "missing"
	// KindOrphan — an object with no owner on the other side: a blob no
	// metadata record claims, or a metadata record with no backing content.
	KindOrphan = "orphan"
	// KindMismatch — a stored integrity expectation (checksum sidecar, dist
	// shasum, chart digest, OCI digest key) does not match the bytes on disk.
	KindMismatch = "mismatch"
	// KindDrift — a materialized index disagrees with its source records
	// (npm packument vs per-version records). Regenerable; reindex repairs it.
	KindDrift = "drift"
)

// Mode selects how deep a verify run goes.
type Mode string

const (
	// ModeQuick checks structure only: cross-references between blob and meta
	// stores, digest-addressed keys, manifest references. No artifact bytes
	// are hashed (small metadata documents may still be read).
	ModeQuick Mode = "quick"
	// ModeFull additionally re-reads every blob that carries a stored
	// integrity expectation and re-computes its checksums. IO-heavy but
	// linear; this is the mode that proves a migrated store intact.
	ModeFull Mode = "full"
)

// ParseMode maps a user-supplied string to a Mode, defaulting to full.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "", string(ModeFull):
		return ModeFull, true
	case string(ModeQuick):
		return ModeQuick, true
	}
	return "", false
}

// Finding is one detected inconsistency.
type Finding struct {
	Kind      string `json:"kind"`                // missing | orphan | mismatch | drift
	Object    string `json:"object"`              // blob key, or "ns · key" for a meta record
	Component string `json:"component,omitempty"` // component name when derivable
	Version   string `json:"version,omitempty"`
	Detail    string `json:"detail"` // human explanation: what was expected vs found
}

// Result accumulates what one checker pass observed.
type Result struct {
	Findings     []Finding
	BlobsChecked int   // blob keys examined
	MetaChecked  int   // metadata records examined
	BytesRead    int64 // artifact bytes streamed through hashing
}

// Add appends a finding.
func (r *Result) Add(kind, object, component, version, detail string) {
	r.Findings = append(r.Findings, Finding{
		Kind: kind, Object: object, Component: component, Version: version, Detail: detail,
	})
}

// Merge folds another result into r.
func (r *Result) Merge(o Result) {
	r.Findings = append(r.Findings, o.Findings...)
	r.BlobsChecked += o.BlobsChecked
	r.MetaChecked += o.MetaChecked
	r.BytesRead += o.BytesRead
}

// Report lifecycle states.
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusComplete = "complete"
	StatusFailed   = "failed"
)

// MaxStoredFindings caps the finding list persisted per report so one
// pathological store cannot balloon the record. Counts stay exact.
const MaxStoredFindings = 500

// Report is the persisted outcome of one verify run. Re-runs replace it —
// the report answers "is this store intact now"; history lives in the audit
// log.
type Report struct {
	Repo       string    `json:"repo"`
	Status     string    `json:"status"` // queued | running | complete | failed
	Mode       Mode      `json:"mode"`
	QueuedAt   time.Time `json:"queuedAt,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	DurationMs int64     `json:"durationMs,omitempty"`

	BlobsChecked int   `json:"blobsChecked"`
	MetaChecked  int   `json:"metaChecked"`
	BytesRead    int64 `json:"bytesRead"`

	Findings      []Finding      `json:"findings"`
	TotalFindings int            `json:"totalFindings"`
	Counts        map[string]int `json:"counts"` // findings per kind
	Truncated     bool           `json:"truncated,omitempty"`

	Note  string `json:"note,omitempty"`  // caveats, e.g. proxy self-healing
	Error string `json:"error,omitempty"` // set when Status == failed
}

// Clean reports whether the run completed and found nothing.
func (rep Report) Clean() bool {
	return rep.Status == StatusComplete && rep.TotalFindings == 0
}

// BuildReport assembles a complete Report from a checker Result. Findings are
// sorted (kind, then object) for stable rendering and capped at
// MaxStoredFindings.
func BuildReport(repoName string, mode Mode, queued, started time.Time, res Result, note string) Report {
	sort.SliceStable(res.Findings, func(i, j int) bool {
		if res.Findings[i].Kind != res.Findings[j].Kind {
			return res.Findings[i].Kind < res.Findings[j].Kind
		}
		return res.Findings[i].Object < res.Findings[j].Object
	})
	counts := map[string]int{}
	for _, f := range res.Findings {
		counts[f.Kind]++
	}
	total := len(res.Findings)
	truncated := false
	if total > MaxStoredFindings {
		res.Findings = res.Findings[:MaxStoredFindings]
		truncated = true
	}
	if res.Findings == nil {
		res.Findings = []Finding{}
	}
	now := time.Now().UTC()
	return Report{
		Repo: repoName, Status: StatusComplete, Mode: mode,
		QueuedAt: queued, StartedAt: started, FinishedAt: now,
		DurationMs:   now.Sub(started).Milliseconds(),
		BlobsChecked: res.BlobsChecked, MetaChecked: res.MetaChecked, BytesRead: res.BytesRead,
		Findings: res.Findings, TotalFindings: total, Counts: counts,
		Truncated: truncated, Note: note,
	}
}

// storeNS is the meta namespace holding one Report per repo (key = repo name).
const storeNS = "admin:integrity"

// Store persists the latest Report per repository.
type Store struct{ m meta.Store }

func NewStore(m meta.Store) *Store { return &Store{m: m} }

// Put replaces the stored report for rep.Repo.
func (s *Store) Put(rep Report) error { return s.m.PutJSON(storeNS, rep.Repo, rep) }

// Get returns the stored report for a repo, if any.
func (s *Store) Get(repoName string) (Report, bool, error) {
	var rep Report
	ok, err := s.m.GetJSON(storeNS, repoName, &rep)
	return rep, ok, err
}

// Delete removes a repo's report (used when the repo itself is deleted).
func (s *Store) Delete(repoName string) error { return s.m.Delete(storeNS, repoName) }
