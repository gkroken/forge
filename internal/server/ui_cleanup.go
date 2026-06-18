package server

import (
	"fmt"
	"net/http"
	"strings"

	"forge/internal/repo"
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
	RepoName string
	Format   string
	Kind     string
	Criteria string
	Interval string
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
	for _, rp := range s.Repos.All() {
		if rp.CleanupPolicy == nil {
			continue
		}
		rows = append(rows, cleanupPolicyRow{
			RepoName: rp.Name,
			Format:   rp.Format,
			Kind:     string(rp.Kind),
			Criteria: summarizeCleanupPolicy(rp),
			Interval: fmtInterval(rp),
		})
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

func summarizeCleanupPolicy(rp repo.Repository) string {
	p := rp.CleanupPolicy
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
	if len(parts) == 0 {
		return "No rules"
	}
	return strings.Join(parts, " · ")
}

func fmtInterval(rp repo.Repository) string {
	if rp.CleanupPolicy == nil || rp.CleanupPolicy.Interval == 0 {
		return ""
	}
	return rp.CleanupPolicy.Interval.String()
}
