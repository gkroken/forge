package server

import (
	"fmt"
	"net/http"
	"strings"

	"forge/internal/cleanup"
)

// ── page types ────────────────────────────────────────────────────────────────

type cleanupPoliciesPage struct {
	Title       string
	ActiveNav   string
	PolicyCount int
	Policies    []cleanupPolicyRow
	SchedTasks  []schedTask
}

type cleanupPolicyRow struct {
	Name        string
	Description string
	Criteria    string
	Interval    string
}

type schedTask struct {
	Icon    string
	Name    string
	Cron    string
	Status  string
	Color   string
	LastRun string
}

// ── handler ──────────────────────────────────────────────────────────────────

func (s *Server) uiCleanupPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	var rows []cleanupPolicyRow
	if s.Cleanup != nil {
		policies, err := s.Cleanup.List()
		if err == nil {
			for _, p := range policies {
				rows = append(rows, cleanupPolicyRow{
					Name:        p.Name,
					Description: p.Description,
					Criteria:    summarizeNamedPolicy(p),
					Interval:    namedPolicyInterval(p),
				})
			}
		}
	}

	tasks := []schedTask{
		{Icon: "GC", Name: "Blob store GC", Cron: "0 2 * * *", Status: "Scheduled", Color: "var(--accent)", LastRun: "—"},
		{Icon: "IX", Name: "Rebuild search index", Cron: "0 */6 * * *", Status: "Scheduled", Color: "var(--accent)", LastRun: "—"},
		{Icon: "CL", Name: "Apply cleanup policies", Cron: "30 2 * * *", Status: "Scheduled", Color: "var(--accent)", LastRun: "—"},
	}

	render(w, tmplCleanupPolicies, "admin_shell.html", cleanupPoliciesPage{
		Title:       "Cleanup",
		ActiveNav:   "cleanup",
		PolicyCount: len(rows),
		Policies:    rows,
		SchedTasks:  tasks,
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func summarizeNamedPolicy(p cleanup.NamedPolicy) string {
	var parts []string
	if p.KeepVersions > 0 {
		parts = append(parts, fmt.Sprintf("Keep last %d versions", p.KeepVersions))
	}
	if p.DeleteOlderThanDays > 0 {
		parts = append(parts, fmt.Sprintf("Delete older than %d days", p.DeleteOlderThanDays))
	}
	if p.DeleteSnapshotsDays > 0 {
		parts = append(parts, fmt.Sprintf("Delete snapshots older than %d days", p.DeleteSnapshotsDays))
	}
	if p.KeepReleasesOnly {
		parts = append(parts, "Keep releases only")
	}
	if p.LastDownloadedDays > 0 {
		parts = append(parts, fmt.Sprintf("Delete not downloaded in %d days", p.LastDownloadedDays))
	}
	if len(parts) == 0 {
		return "No rules"
	}
	return strings.Join(parts, " · ")
}

func namedPolicyInterval(p cleanup.NamedPolicy) string {
	if p.Interval == 0 {
		return "Manual only"
	}
	return p.Interval.String()
}
