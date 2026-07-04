package server

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"forge/internal/integrity"
)

// integrityChip is one kind→count pill in the rollup's findings column.
type integrityChip struct {
	Kind  string
	Count int
	Sev   string // reuses the severity token classes: sev-{Sev}
}

// integKindSev maps a finding kind to the severity token that colors its chip:
// missing and mismatch mean data loss/corruption, orphan is hygiene, drift is
// regenerable.
var integKindSev = map[string]string{
	integrity.KindMissing:  "critical",
	integrity.KindMismatch: "high",
	integrity.KindOrphan:   "moderate",
	integrity.KindDrift:    "low",
}

// integrityRow backs one repository line on the rollup page.
type integrityRow struct {
	Repo, Format, Kind string
	Status             string // never | queued | running | clean | findings | failed
	StatusLabel        string
	Chips              []integrityChip
	Mode               string
	VerifiedAgo        string // "—" when never verified
	BlobsChecked       int
	BytesRead          string
	Note               string
}

// integrityPage backs GET /ui/admin/integrity.
type integrityPage struct {
	Title     string
	ActiveNav string
	Rows      []integrityRow
	CanVerify bool // false when no async worker is wired (verify buttons hidden)
}

// buildIntegrityChips converts a report's per-kind counts into ordered chips.
func buildIntegrityChips(counts map[string]int) []integrityChip {
	order := []string{integrity.KindMissing, integrity.KindMismatch, integrity.KindOrphan, integrity.KindDrift}
	var chips []integrityChip
	for _, k := range order {
		if counts[k] > 0 {
			chips = append(chips, integrityChip{Kind: k, Count: counts[k], Sev: integKindSev[k]})
		}
	}
	return chips
}

// humanAgo renders a compact "how stale is this" suffix for a past time.
func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// uiIntegrity renders GET /ui/admin/integrity — the storage-consistency
// rollup: every repository with its latest verify report.
func (s *Server) uiIntegrity(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	store := integrity.NewStore(s.Meta)
	var rows []integrityRow
	for _, rp := range s.Repos.All() {
		row := integrityRow{
			Repo: rp.Name, Format: rp.Format, Kind: string(rp.Kind),
			Status: "never", StatusLabel: "never verified", VerifiedAgo: "—",
		}
		rep, ok, err := store.Get(rp.Name)
		if err == nil && ok {
			row.Mode = string(rep.Mode)
			switch rep.Status {
			case integrity.StatusQueued:
				row.Status, row.StatusLabel = "queued", "queued"
			case integrity.StatusRunning:
				row.Status, row.StatusLabel = "running", "running"
			case integrity.StatusFailed:
				row.Status, row.StatusLabel = "failed", "verify failed"
				row.Note = rep.Error
			case integrity.StatusComplete:
				row.VerifiedAgo = humanAgo(rep.FinishedAt)
				row.BlobsChecked = rep.BlobsChecked
				row.BytesRead = humanBytes(rep.BytesRead)
				if rep.TotalFindings == 0 {
					row.Status, row.StatusLabel = "clean", "intact"
				} else {
					row.Status = "findings"
					row.StatusLabel = fmt.Sprintf("%d findings", rep.TotalFindings)
					if rep.TotalFindings == 1 {
						row.StatusLabel = "1 finding"
					}
					row.Chips = buildIntegrityChips(rep.Counts)
				}
				row.Note = rep.Note
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Repo < rows[j].Repo })
	render(w, tmplIntegrity, "admin_shell.html", integrityPage{
		Title:     "Integrity",
		ActiveNav: "integrity",
		Rows:      rows,
		CanVerify: s.Queue != nil,
	})
}
