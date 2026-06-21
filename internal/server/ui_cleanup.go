package server

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

// ── page types ────────────────────────────────────────────────────────────────

type cleanupPoliciesPage struct {
	Title          string
	ActiveNav      string
	PolicyCount    int
	ReclaimableGB  string // formatted from cleanup.Reclaimable; "—" when unknown
	FreedLast30dGB string // formatted from cleanup.FreedLast30d
	NextRun        string // next scheduled cleanup; "—" when none scheduled
	Policies       []cleanupPolicyRow
	SchedTasks     []schedTask
}

type cleanupPolicyRow struct {
	Name        string
	Description string
	Criteria    string
	Interval    string
	AppliedTo   string // comma-separated repo names using this policy
	HasRepos    bool   // true when at least one repo uses this policy
	Status      string // "Active" | "Manual"
	StatusClass string // CSS class for the status pill
}

type schedTask struct {
	Icon    string // Material Symbols icon name
	Name    string
	Cron    string
	Status  string
	Color   template.CSS // trusted CSS color token (e.g. var(--accent))
	LastRun string
}

// ── handler ──────────────────────────────────────────────────────────────────

func (s *Server) uiCleanupPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	// Build a map of policy name → repo names that use it.
	policyRepos := make(map[string][]string)
	for _, rp := range s.Repos.All() {
		if rp.CleanupPolicyName != "" {
			policyRepos[rp.CleanupPolicyName] = append(policyRepos[rp.CleanupPolicyName], rp.Name)
		}
	}

	var rows []cleanupPolicyRow
	if s.Cleanup != nil {
		policies, err := s.Cleanup.List()
		if err == nil {
			for _, p := range policies {
				status, cls := "Manual", "pill-muted"
				if p.Interval > 0 {
					status, cls = "Active", "pill-ok"
				}
				applied := strings.Join(policyRepos[p.Name], ", ")
				if applied == "" {
					applied = "—"
				}
				rows = append(rows, cleanupPolicyRow{
					Name:        p.Name,
					Description: p.Description,
					Criteria:    summarizeNamedPolicy(p),
					Interval:    namedPolicyInterval(p),
					AppliedTo:   applied,
					HasRepos:    len(policyRepos[p.Name]) > 0,
					Status:      status,
					StatusClass: cls,
				})
			}
		}
	}

	// KPI: reclaimable and freed in last 30 days.
	reclaimableGB := "—"
	freedLast30dGB := "—"
	if s.Cleanup != nil {
		if rb := cleanup.Reclaimable(s.Cleanup, s.Repos, s.Blob, s.Meta); rb >= 0 {
			reclaimableGB = humanBytes(rb)
		}
		if fb := cleanup.FreedLast30d(s.Meta, s.Repos); fb >= 0 {
			freedLast30dGB = humanBytes(fb)
		}
	}

	// Determine which hosted repos have a scheduled (interval > 0) policy, and
	// look up each policy's interval — used for the scheduler status + next-run.
	intervalByPolicy := map[string]time.Duration{}
	if s.Cleanup != nil {
		if policies, err := s.Cleanup.List(); err == nil {
			for _, p := range policies {
				intervalByPolicy[p.Name] = p.Interval
			}
		}
	}
	type schedRepo struct {
		name     string
		interval time.Duration
	}
	var scheduled []schedRepo
	for _, rp := range s.Repos.All() {
		if rp.Kind != repo.Hosted || rp.CleanupPolicyName == "" {
			continue
		}
		if iv := intervalByPolicy[rp.CleanupPolicyName]; iv > 0 {
			scheduled = append(scheduled, schedRepo{name: rp.Name, interval: iv})
		}
	}

	// Scheduled tasks — only "Apply cleanup policies" is a real background job
	// (the Scheduler); report its actual status, cadence, and last run rather
	// than inventing cron strings for jobs that don't exist.
	lastCleanupRun := "—"
	var latestRun time.Time
	runs := map[string]time.Time{}
	if s.Scheduler != nil {
		runs = s.Scheduler.LastRuns()
		for _, t := range runs {
			if t.After(latestRun) {
				latestRun = t
			}
		}
		if !latestRun.IsZero() {
			lastCleanupRun = latestRun.UTC().Format("2006-01-02 15:04")
		}
	}

	// Next run = earliest (lastRun + interval) across scheduled repos. A repo the
	// scheduler hasn't run yet is due on the next tick.
	nextRun := "—"
	schedStatus, cadence := "Idle", "No scheduled policies"
	schedColor := template.CSS("var(--text-light)")
	if s.Scheduler != nil && len(scheduled) > 0 {
		schedStatus, schedColor = "Active", template.CSS("var(--accent)")
		cadence = fmt.Sprintf("%d repo(s) · runs on each policy interval", len(scheduled))
		now := time.Now()
		var earliest time.Time
		for _, sr := range scheduled {
			next := now // never-run → due now
			if last, ok := runs[sr.name]; ok {
				next = last.Add(sr.interval)
			}
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
		if !earliest.After(now) {
			nextRun = "Due now"
		} else {
			nextRun = earliest.UTC().Format("2006-01-02 15:04")
		}
	}

	tasks := []schedTask{
		{Icon: "auto_delete", Name: "Apply cleanup policies", Cron: cadence, Status: schedStatus, Color: schedColor, LastRun: lastCleanupRun},
	}

	render(w, tmplCleanupPolicies, "admin_shell.html", cleanupPoliciesPage{
		Title:          "Cleanup",
		ActiveNav:      "cleanup",
		PolicyCount:    len(rows),
		ReclaimableGB:  reclaimableGB,
		FreedLast30dGB: freedLast30dGB,
		NextRun:        nextRun,
		Policies:       rows,
		SchedTasks:     tasks,
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
	Repos       []policyRepoOption // hosted repos this policy can be applied to
}

// policyRepoOption is one selectable hosted repository on the policy form.
type policyRepoOption struct {
	Name        string
	Format      string
	Checked     bool   // currently assigned to THIS policy
	OtherPolicy string // assigned to a different policy (surfaced, not blocked)
}

// policyRepoOptions lists hosted repos and marks which are assigned to
// policyName (Checked) or to some other policy (OtherPolicy).
func (s *Server) policyRepoOptions(policyName string) []policyRepoOption {
	var opts []policyRepoOption
	for _, rp := range s.Repos.All() {
		if rp.Kind != repo.Hosted {
			continue
		}
		opt := policyRepoOption{Name: rp.Name, Format: rp.Format}
		if policyName != "" && rp.CleanupPolicyName == policyName {
			opt.Checked = true
		} else if rp.CleanupPolicyName != "" {
			opt.OtherPolicy = rp.CleanupPolicyName
		}
		opts = append(opts, opt)
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
	return opts
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
		Repos:       s.policyRepoOptions(name),
	})
}

// applyPolicyToRepos syncs hosted repo→policy assignments from the form's
// "applyRepos" checkboxes: checked repos get policyName (overwriting a prior
// policy), and repos that previously had policyName but are now unchecked are
// cleared. Repos assigned to a different policy are left alone unless checked.
func (s *Server) applyPolicyToRepos(r *http.Request, policyName string) {
	selected := map[string]bool{}
	for _, n := range r.Form["applyRepos"] {
		selected[n] = true
	}
	for _, rp := range s.Repos.All() {
		if rp.Kind != repo.Hosted {
			continue
		}
		switch {
		case selected[rp.Name] && rp.CleanupPolicyName != policyName:
			rp.CleanupPolicyName = policyName
			_ = s.Repos.Update(rp)
		case !selected[rp.Name] && rp.CleanupPolicyName == policyName:
			rp.CleanupPolicyName = ""
			_ = s.Repos.Update(rp)
		}
	}
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
	s.applyPolicyToRepos(r, policy.Name)
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
	// Preserve the user's checkbox selections across the error re-render.
	selected := map[string]bool{}
	for _, n := range r.Form["applyRepos"] {
		selected[n] = true
	}
	opts := s.policyRepoOptions(policy.Name)
	for i := range opts {
		opts[i].Checked = selected[opts[i].Name]
	}
	render(w, tmplCleanupPolicyForm, "admin_shell.html", cleanupPolicyFormPage{
		Title:       title,
		ActiveNav:   "cleanup",
		Policy:      policy,
		IntervalStr: intervalStr,
		IsEdit:      isEdit,
		Error:       errMsg,
		Repos:       opts,
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
