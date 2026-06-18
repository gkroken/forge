package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// ── policy form ───────────────────────────────────────────────────────────────

type cleanupPolicyFormPage struct {
	Title       string
	ActiveNav   string
	Policy      cleanup.NamedPolicy
	IntervalStr string
	IsEdit      bool
	Error       string
}

func (s *Server) uiCleanupPolicyForm(w http.ResponseWriter, r *http.Request, name string, isEdit bool) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if s.Cleanup == nil {
		http.Error(w, "cleanup policies not configured", http.StatusServiceUnavailable)
		return
	}
	var policy cleanup.NamedPolicy
	if isEdit {
		np, ok, err := s.Cleanup.Get(name)
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		policy = np
	}
	if r.Method == http.MethodPost {
		s.processCleanupPolicyForm(w, r, name, isEdit)
		return
	}
	intervalStr := ""
	if policy.Interval > 0 {
		intervalStr = policy.Interval.String()
	}
	title := "Cleanup — New policy"
	if isEdit {
		title = "Cleanup — Edit " + name
	}
	render(w, tmplCleanupPolicyForm, "admin_shell.html", cleanupPolicyFormPage{
		Title:       title,
		ActiveNav:   "cleanup",
		Policy:      policy,
		IntervalStr: intervalStr,
		IsEdit:      isEdit,
	})
}

func (s *Server) processCleanupPolicyForm(w http.ResponseWriter, r *http.Request, existingName string, isEdit bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if isEdit {
		name = existingName
	}
	keepVersions, _ := strconv.Atoi(r.FormValue("keepVersions"))
	deleteOlderThanDays, _ := strconv.Atoi(r.FormValue("deleteOlderThanDays"))
	deleteSnapshotsDays, _ := strconv.Atoi(r.FormValue("deleteSnapshotsDays"))
	lastDownloadedDays, _ := strconv.Atoi(r.FormValue("lastDownloadedDays"))
	keepReleasesOnly := r.FormValue("keepReleasesOnly") == "on"

	var interval time.Duration
	if raw := strings.TrimSpace(r.FormValue("interval")); raw != "" {
		var err error
		interval, err = time.ParseDuration(raw)
		if err != nil {
			s.reRenderPolicyForm(w, r, existingName, isEdit, "Invalid interval: "+err.Error())
			return
		}
	}
	policy := cleanup.NamedPolicy{
		Name:                name,
		Description:         strings.TrimSpace(r.FormValue("description")),
		KeepVersions:        keepVersions,
		DeleteOlderThanDays: deleteOlderThanDays,
		DeleteSnapshotsDays: deleteSnapshotsDays,
		LastDownloadedDays:  lastDownloadedDays,
		KeepReleasesOnly:    keepReleasesOnly,
		Interval:            interval,
	}
	if err := s.Cleanup.Put(policy); err != nil {
		s.reRenderPolicyForm(w, r, existingName, isEdit, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/admin/cleanup-policies", http.StatusSeeOther) // #nosec G710
}

func (s *Server) reRenderPolicyForm(w http.ResponseWriter, r *http.Request, existingName string, isEdit bool, errMsg string) {
	var policy cleanup.NamedPolicy
	if isEdit {
		if np, ok, _ := s.Cleanup.Get(existingName); ok {
			policy = np
		}
	}
	policy.Name = strings.TrimSpace(r.FormValue("name"))
	if isEdit {
		policy.Name = existingName
	}
	policy.Description = r.FormValue("description")
	policy.KeepVersions, _ = strconv.Atoi(r.FormValue("keepVersions"))
	policy.DeleteOlderThanDays, _ = strconv.Atoi(r.FormValue("deleteOlderThanDays"))
	policy.DeleteSnapshotsDays, _ = strconv.Atoi(r.FormValue("deleteSnapshotsDays"))
	policy.LastDownloadedDays, _ = strconv.Atoi(r.FormValue("lastDownloadedDays"))
	policy.KeepReleasesOnly = r.FormValue("keepReleasesOnly") == "on"

	intervalStr := r.FormValue("interval")
	if iv, err := time.ParseDuration(intervalStr); err == nil && iv > 0 {
		policy.Interval = iv
	}
	title := "Cleanup — New policy"
	if isEdit {
		title = "Cleanup — Edit " + existingName
	}
	render(w, tmplCleanupPolicyForm, "admin_shell.html", cleanupPolicyFormPage{
		Title:       title,
		ActiveNav:   "cleanup",
		Policy:      policy,
		IntervalStr: intervalStr,
		IsEdit:      isEdit,
		Error:       errMsg,
	})
}

func (s *Server) uiDeleteCleanupPolicy(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if s.Cleanup == nil {
		http.Error(w, "cleanup policies not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.Cleanup.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	target := "/ui/admin/cleanup-policies"
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther) // #nosec G710
}

// ── per-repo cleanup panel ────────────────────────────────────────────────────

type cleanupRunRow struct {
	Timestamp  string
	PolicyName string
	DryRun     bool
	Deleted    int
	FreedMB    string
	DurationMs int64
}

type repoCleanupPage struct {
	Title      string
	ActiveNav  string
	RepoName   string
	Format     string
	PolicyName string
	History    []cleanupRunRow
}

func (s *Server) uiRepoCleanupPanel(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var history []cleanupRunRow
	runs, _ := cleanup.GetHistory(s.Meta, name)
	for _, run := range runs {
		history = append(history, cleanupRunRow{
			Timestamp:  run.Timestamp.UTC().Format("2006-01-02 15:04"),
			PolicyName: run.PolicyName,
			DryRun:     run.DryRun,
			Deleted:    run.Deleted,
			FreedMB:    fmt.Sprintf("%.2f", float64(run.FreedBytes)/1048576),
			DurationMs: run.DurationMs,
		})
	}
	render(w, tmplCleanupRun, "admin_shell.html", repoCleanupPage{
		Title:      "Cleanup — " + name,
		ActiveNav:  "cleanup",
		RepoName:   rp.Name,
		Format:     rp.Format,
		PolicyName: rp.CleanupPolicyName,
		History:    history,
	})
}

// policyNames returns a sorted list of named policy names for use in dropdowns.
// Returns nil when cleanup is not configured.
func (s *Server) policyNames() []string {
	if s.Cleanup == nil {
		return nil
	}
	policies, err := s.Cleanup.List()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(policies))
	for _, p := range policies {
		names = append(names, p.Name)
	}
	return names
}

