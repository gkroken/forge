package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/config"
	"forge/internal/obs"
	"forge/internal/proxy"
	"forge/internal/repo"
)

// parseBoolField reads a hidden+checkbox pair where the checkbox has the same
// name but comes first; returns nil if the field is absent from the form.
// overlayDepGuardFields applies the dependency-confusion form fields. The
// kind-scoped sections are only hidden client-side, so hidden inputs still
// submit: claims apply to hosted repos only (cleared otherwise) and the guard
// toggle to group/proxy only. An enabled guard is stored as nil (the default)
// so only explicit opt-outs persist.
func overlayDepGuardFields(r *http.Request, rp *repo.Repository) {
	rp.Claims = nil
	if rp.Kind == repo.Hosted {
		for _, line := range strings.Split(r.FormValue("claims"), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				rp.Claims = append(rp.Claims, t)
			}
		}
	}
	rp.DepConfusionGuard = nil
	if rp.Kind == repo.Group || rp.Kind == repo.Proxy {
		if v := parseBoolField(r, "depConfusionGuard"); v != nil && !*v {
			rp.DepConfusionGuard = v
		}
	}
}

func parseBoolField(r *http.Request, name string) *bool {
	vals, ok := r.Form[name]
	if !ok || len(vals) == 0 {
		return nil
	}
	b := vals[0] == "true"
	return &b
}

var (
	allFormats = []string{"maven", "npm", "helm", "cran", "oci"}
	allKinds   = []string{"hosted", "proxy", "group"}
)

// ── page data types ───────────────────────────────────────────────────────────

type adminReposPage struct {
	Title     string
	ActiveNav string
	Rows      []adminRepoRow
	Flash     string
}

// adminRepoRow is a display-ready row for the canonical Repositories list:
// repo identity + storage usage (member-aggregated for groups) + upstream
// health, plus what the Browse/Configure/Delete actions need.
type adminRepoRow struct {
	Name     string
	Format   string
	Kind     string
	Upstream string
	Members  []string
	// ManagedByConfig marks a repo declared in the -config file. The UI shows a
	// badge and disables Configure/Delete; the API refuses those writes anyway.
	ManagedByConfig bool
	ArtifactCount   int
	SizeBytes       int64
	Health          string // "ok" | "down" | "" (proxy only)
	VulnTotal       int    // vulnerable components in this repo (0 = none / not scanned)
	VulnCritical    int    // of those, how many are critical
	VulnWorst       string // worst severity label for the row badge ("" = none)
}

type adminFormPage struct {
	Title       string
	ActiveNav   string
	Repo        repo.Repository
	KindStr     string // string(Repo.Kind) — avoids named-type comparison in templates
	IsEdit      bool
	Error       string
	Formats     []string
	Kinds       []string
	PolicyNames []string       // named cleanup policies available for selection
	Members     []memberOption // candidate repos for a group's member picker
}

// memberOption is one selectable repo in a group's member picker. The picker
// renders every eligible candidate (non-group, not self) tagged with its
// format+kind; repo_form.js shows only those matching the group's format.
type memberOption struct {
	Name    string
	Format  string
	Kind    string
	Checked bool
}

// memberOptions lists the repos a group can aggregate: every hosted/proxy repo
// except the group itself. Already-selected members come first, in their saved
// priority order, so editing a group preserves member ordering; the rest follow
// hosted-first then by name. Filtering to the group's own format happens client
// side (the format can change on the new-repo form).
func (s *Server) memberOptions(current repo.Repository) []memberOption {
	selected := map[string]bool{}
	for _, m := range current.Members {
		selected[m] = true
	}
	byName := map[string]repo.Repository{}
	var rest []repo.Repository
	for _, rp := range s.Repos.All() {
		if rp.Name == current.Name || rp.Kind == repo.Group {
			continue
		}
		byName[rp.Name] = rp
		if !selected[rp.Name] {
			rest = append(rest, rp)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].Kind != rest[j].Kind {
			return rest[i].Kind == repo.Hosted // hosted before proxy
		}
		return rest[i].Name < rest[j].Name
	})

	var out []memberOption
	for _, m := range current.Members { // saved order first
		if rp, ok := byName[m]; ok {
			out = append(out, memberOption{Name: rp.Name, Format: rp.Format, Kind: string(rp.Kind), Checked: true})
		}
	}
	for _, rp := range rest {
		out = append(out, memberOption{Name: rp.Name, Format: rp.Format, Kind: string(rp.Kind)})
	}
	return out
}

type repoConfigPage struct {
	Title     string
	ActiveNav string
	Repo      repo.Repository
	KindStr   string
	Error     string
	Flash     string
	// ManagedByConfig makes the Settings tab read-only. A form that looks
	// editable but refuses to save is the most complained-about part of Grafana
	// provisioning (grafana/grafana#37679) — disable at the door instead.
	ManagedByConfig bool
	ConfigSource    string
	Formats         []string
	Kinds           []string
	PolicyNames     []string
	Members         []memberOption // candidate repos for a group's member picker
	ActiveTab       string         // "settings" | "content" | "access" | "security" | "integrity" | "activity"
	ArtifactCount   int
	SizeBytes       int64
	StoragePct      int
	QuotaBytes      int64 // 0 = unlimited
	QuotaPct        int   // used/quota, 0..100+ (clamped for the bar width in the template)
	QuotaNear       bool  // >=80% used — warn styling
	QuotaOver       bool  // >=100% used — over-quota styling
	RecentActivity  []auditRow
}

// ── access view types ─────────────────────────────────────────────────────────

type repoGrant struct {
	Role        string
	Description string
}

type accessRow struct {
	RepoName      string
	Format        string
	Kind          string
	AnonymousRead bool
	Grants        []repoGrant
}

type ssoMapping struct {
	Group string
	Role  string
}

type ssoInfo struct {
	Enabled     bool
	Issuer      string
	ClientID    string
	RedirectURL string
	GroupsClaim string
	Mappings    []ssoMapping // empty = all SSO logins get the default grant
}

type ldapInfo struct {
	Enabled    bool
	URLs       []string
	BindDN     string
	UserBaseDN string
	UserFilter string
	GroupMode  string
	Mappings   []ssoMapping // empty = all LDAP logins get the default grant
}

type adminAccessPage struct {
	Title       string
	ActiveNav   string
	AuthEnabled bool
	Rows        []accessRow
	SSO         ssoInfo
	LDAP        ldapInfo
}

// ── token types ───────────────────────────────────────────────────────────────

// tokenRow is a display-ready snapshot of one token for the template.
type tokenRow struct {
	ID          string
	Description string
	Grants      []tokenGrantView
	CreatedStr  string
	ExpiresStr  string
	Owner       string // from auth.Token.Owner (empty if not set)
	LastUsedStr string // formatted auth.Token.LastUsed; "never" if nil
	StatusClass string // CSS dot class: dot-ok / dot-err / dot-neutral
	StatusLabel string // "Active" | "Expired" | "Never used"
}

// tokenGrantView is one grant rendered in the token table: repo, action
// badges, and the joined selector list (empty = whole repo).
type tokenGrantView struct {
	Repo      string
	Actions   []string
	Selectors string
}

// grantRow is one row of the grant builder, round-tripped on validation
// errors. Actions is keyed by verb so the template can restore checkboxes.
type grantRow struct {
	Repo      string
	Actions   map[string]bool
	Selectors string
}

// tokenForm holds the last-submitted (or default) create-token form values
// so the template can round-trip them on validation errors.
type tokenForm struct {
	Description string
	Expires     string
	Rows        []grantRow
}

// defaultTokenForm returns the empty grant builder: one read-only row on all
// repositories.
func defaultTokenForm() tokenForm {
	return tokenForm{Rows: []grantRow{{Repo: "*", Actions: map[string]bool{"read": true}}}}
}

type adminTokensPage struct {
	Title       string
	AuthEnabled bool
	Tokens      []tokenRow
	AllRepos    []string
	Form        tokenForm
	NewSecret   string
	Error       string
	Flash       string
}

// userRow is a display-ready snapshot of one user for the template.
type userRow struct {
	Username     string
	DisplayName  string
	Role         string
	RoleClass    string // CSS badge class
	CreatedStr   string
	LastLoginStr string
	StatusClass  string
	StatusLabel  string
	Disabled     bool
}

// roleCard is a display-ready snapshot of one role for the template.
type roleCard struct {
	Name         string
	Description  string
	BaseRole     string
	RoleClass    string // CSS badge class
	MemberCount  int
	IsPredefined bool
	// ManagedByConfig marks a custom role declared in the -config file.
	ManagedByConfig bool
}

// adminTokensV2Page wraps adminTokensPage for the sidebar (Foundry) layout.
type adminTokensV2Page struct {
	adminTokensPage
	ActiveNav string
	ActiveTab string // "tokens" | "users" | "roles"
	Users     []userRow
	Roles     []roleCard
}

// ── dispatcher ────────────────────────────────────────────────────────────────

// handleUIAdmin dispatches all /ui/admin/* routes.
func (s *Server) handleUIAdmin(w http.ResponseWriter, r *http.Request, sub string) {
	sub = strings.TrimRight(sub, "/")
	if sub == "" {
		sub = "/"
	}
	switch {
	case sub == "/" || sub == "":
		s.uiAdminHome(w, r)
	case sub == "/repos/new":
		s.uiAdminNewRepo(w, r)
	case strings.HasPrefix(sub, "/repos/") && strings.HasSuffix(sub, "/edit"):
		name := strings.TrimSuffix(strings.TrimPrefix(sub, "/repos/"), "/edit")
		s.uiAdminEditRepo(w, r, name)
	case strings.HasPrefix(sub, "/repos/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(sub, "/repos/")
		s.uiAdminDeleteRepo(w, r, name)
	case sub == "/tokens":
		s.uiAdminTokens(w, r)
	case strings.HasPrefix(sub, "/tokens/users/") && r.Method == http.MethodDelete:
		username := strings.TrimPrefix(sub, "/tokens/users/")
		s.uiAdminDeleteUser(w, r, username)
	case strings.HasPrefix(sub, "/tokens/users/") && strings.HasSuffix(sub, "/disable"):
		username := strings.TrimSuffix(strings.TrimPrefix(sub, "/tokens/users/"), "/disable")
		s.uiAdminToggleUser(w, r, username)
	case strings.HasPrefix(sub, "/tokens/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(sub, "/tokens/")
		s.uiAdminRevokeToken(w, r, id)
	case sub == "/access":
		s.uiAdminAccess(w, r)
	case sub == "/cleanup-policies":
		s.uiCleanupPolicies(w, r)
	case sub == "/cleanup-policies/new":
		s.uiCleanupPolicyForm(w, r, "", false)
	case strings.HasPrefix(sub, "/cleanup-policies/") && strings.HasSuffix(sub, "/edit"):
		name := strings.TrimSuffix(strings.TrimPrefix(sub, "/cleanup-policies/"), "/edit")
		s.uiCleanupPolicyForm(w, r, name, true)
	case strings.HasPrefix(sub, "/cleanup-policies/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(sub, "/cleanup-policies/")
		s.uiDeleteCleanupPolicy(w, r, name)
	case strings.HasPrefix(sub, "/repos/") && strings.HasSuffix(sub, "/cleanup"):
		name := strings.TrimSuffix(strings.TrimPrefix(sub, "/repos/"), "/cleanup")
		s.uiRepoCleanupPanel(w, r, name)
	case sub == "/webhooks":
		s.uiWebhooks(w, r)
	case sub == "/observability":
		s.uiObservability(w, r)
	case sub == "/audit":
		s.uiAuditHistory(w, r)
	case sub == "/security":
		s.uiSecurity(w, r)
	case sub == "/security-policies":
		s.uiSecurityPolicies(w, r)
	case sub == "/integrity":
		s.uiIntegrity(w, r)
	case sub == "/migration":
		s.uiMigration(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (s *Server) uiAdminHome(w http.ResponseWriter, r *http.Request) {
	bsizes := s.GetBlobSizes()
	var rows []adminRepoRow
	for _, rp := range s.Repos.All() {
		row := adminRepoRow{
			Name:          rp.Name,
			Format:        rp.Format,
			Kind:          string(rp.Kind),
			Upstream:      rp.Upstream,
			Members:       rp.Members,
			ArtifactCount: bsizes.CountByRepo[rp.Name],
			SizeBytes:     bsizes.ByRepo[rp.Name],
		}
		row.ManagedByConfig = s.configOwns(config.KindRepository, rp.Name)
		if rp.Kind == repo.Proxy && rp.Upstream != "" {
			row.Health = proxy.HealthOf(rp.Upstream)
		}
		if s.Vuln != nil {
			vr := s.vulnRollupFor(rp.Name)
			row.VulnTotal = vr.VulnerableCount
			row.VulnCritical = vr.BySeverity["critical"]
			row.VulnWorst = vr.WorstSeverity()
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	render(w, tmplAdminRepos, "admin_shell.html", adminReposPage{
		Title:     "Repositories",
		ActiveNav: "repos",
		Rows:      rows,
		Flash:     r.URL.Query().Get("flash"),
	})
}

func (s *Server) uiAdminNewRepo(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		s.processRepoForm(w, r, "", false)
		return
	}
	newRepo := repo.Repository{Kind: repo.Hosted}
	render(w, tmplAdminForm, "admin_shell.html", adminFormPage{
		Title:       "Admin — New repository",
		ActiveNav:   "repos",
		Repo:        newRepo,
		KindStr:     "hosted",
		Formats:     allFormats,
		Kinds:       allKinds,
		PolicyNames: s.policyNames(),
		Members:     s.memberOptions(newRepo),
	})
}

func (s *Server) uiAdminEditRepo(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.processRepoForm(w, r, name, true)
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "settings"
	}
	s.renderRepoConfig(w, rp, tab, "", r.URL.Query().Get("flash"))
}

// buildRepoActivity assembles the last few audit events touching a repository
// for the Settings rail card: a semantic verb, the artifact/target it acted on,
// and the status. It reads the same audit ring the Activity tab paginates.
func buildRepoActivity(log obs.AuditSink, repoName string) []auditRow {
	if log == nil {
		return nil
	}
	needle := "/" + repoName
	methodVerbs := map[string]string{
		"POST": "Published", "PUT": "Uploaded",
		"DELETE": "Deleted", "PATCH": "Updated", "GET": "Downloaded",
	}
	var activity []auditRow
	for _, e := range log.Recent(100) {
		if !strings.Contains(e.Path, needle) {
			continue
		}
		action := methodVerbs[e.Method]
		if action == "" {
			action = e.Method
		}
		// Admin-lifecycle rows (delete/restore/purge/promote) carry a Detail that
		// leads with the real verb; use it so a POST restore doesn't read
		// "Published". Downloads/uploads keep their method-derived verb.
		if v := leadingVerb(e.Detail); v != "" {
			action = v
		}
		if e.Status >= 400 {
			action = "Denied"
		}
		activity = append(activity, auditRow{
			Time:   e.Timestamp.UTC().Format("15:04:05"),
			Actor:  e.Actor,
			Action: action,
			Target: activityTarget(e, repoName),
			Status: strconv.Itoa(e.Status),
			OK:     e.Status < 400,
		})
		if len(activity) == 5 {
			break
		}
	}
	return activity
}

// leadingVerb maps the first word of a curated audit Detail to a display verb
// for the activity card, so a structured lifecycle event reads with the action
// it actually performed rather than its HTTP method. Returns "" for details
// that don't start with a known verb (their method-derived verb is kept).
func leadingVerb(detail string) string {
	first, _, _ := strings.Cut(detail, " ")
	switch strings.TrimSuffix(first, ":") {
	case "deleted":
		return "Deleted"
	case "restored":
		return "Restored"
	case "purged":
		return "Purged"
	case "promote":
		return "Promoted"
	}
	return ""
}

// activityTarget is the human "what" for an audit row: the curated Detail note
// when the event carries one (quota block, vuln-policy decision, promotion),
// otherwise the request path with the repository's own routing prefix stripped
// so only the artifact coordinates remain (e.g. "com/acme/app/1.0/app-1.0.jar",
// "component?name=…&version=…"). The full string is kept; the template
// ellipsizes and shows it in full on hover.
func activityTarget(e obs.AuditEntry, repoName string) string {
	if e.Detail != "" {
		return e.Detail
	}
	for _, pre := range []string{
		"/repository/" + repoName + "/",
		"/v2/" + repoName + "/",
		"/api/v1/repos/" + repoName + "/",
	} {
		if strings.HasPrefix(e.Path, pre) {
			return strings.TrimPrefix(e.Path, pre)
		}
	}
	// Fall back to the path minus a leading routing prefix (group members etc.).
	p := strings.TrimPrefix(e.Path, "/repository/")
	return strings.TrimPrefix(p, "/v2/")
}

func (s *Server) renderRepoConfig(w http.ResponseWriter, rp repo.Repository, tab, errMsg, flash string) {
	bsizes := s.GetBlobSizes()
	sizeBytes := bsizes.ByRepo[rp.Name]
	storagePct := 0
	if bsizes.TotalBytes > 0 {
		storagePct = int(float64(sizeBytes) / float64(bsizes.TotalBytes) * 100)
	}
	var quotaBytes int64
	quotaPct := 0
	if rp.QuotaGB != nil && *rp.QuotaGB > 0 {
		quotaBytes = int64(*rp.QuotaGB * float64(bytesPerGB))
		if quotaBytes > 0 {
			quotaPct = int(float64(sizeBytes) / float64(quotaBytes) * 100)
		}
	}

	activity := buildRepoActivity(s.AuditLog, rp.Name)

	render(w, tmplRepoConfig, "admin_shell.html", repoConfigPage{
		Title:     rp.Name + " — Settings",
		ActiveNav: "repos",
		Repo:      rp,
		KindStr:   string(rp.Kind),
		Error:     errMsg,

		ManagedByConfig: s.configOwns(config.KindRepository, rp.Name) && !s.configOverride,
		ConfigSource:    s.configSource,
		Flash:           flash,
		Formats:         allFormats,
		Kinds:           allKinds,
		PolicyNames:     s.policyNames(),
		Members:         s.memberOptions(rp),
		ActiveTab:       tab,
		ArtifactCount:   bsizes.CountByRepo[rp.Name],
		SizeBytes:       sizeBytes,
		StoragePct:      storagePct,
		QuotaBytes:      quotaBytes,
		QuotaPct:        quotaPct,
		QuotaNear:       quotaBytes > 0 && quotaPct >= 80,
		QuotaOver:       quotaBytes > 0 && quotaPct >= 100,
		RecentActivity:  activity,
	})
}

func (s *Server) uiAdminDeleteRepo(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	// The UI mutates repos through its own handlers, so the admin-API gate does
	// not cover this path — enforce here too.
	if s.refuseConfigOwned(w, r, config.KindRepository, name) {
		return
	}
	if err := s.Repos.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// HX-Redirect triggers a full-page navigation in htmx; plain redirect for non-htmx.
	target := "/ui/admin/?flash=Deleted+repository+" + name
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther) // #nosec G710 -- target is a hardcoded /ui/admin/ prefix
}

// processRepoForm handles the POST for both create and edit.
func (s *Server) processRepoForm(w http.ResponseWriter, r *http.Request, existingName string, isEdit bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if isEdit {
		name = existingName // URL name takes precedence
	}

	var ttl time.Duration
	if raw := strings.TrimSpace(r.FormValue("proxyTTL")); raw != "" {
		var err error
		ttl, err = time.ParseDuration(raw)
		if err != nil {
			s.reRenderForm(w, r, name, isEdit, "Invalid proxy TTL: "+err.Error())
			return
		}
	}

	// Members arrive as repeated checkbox values from the picker; still split on
	// commas so a legacy single comma-separated value keeps working. Dedupe while
	// preserving the submitted (priority) order.
	var members []string
	seen := map[string]bool{}
	for _, raw := range r.Form["members"] {
		for _, m := range strings.Split(raw, ",") {
			t := strings.TrimSpace(m)
			if t != "" && !seen[t] {
				seen[t] = true
				members = append(members, t)
			}
		}
	}

	// For edits, start from the existing repo so that BE-D fields not yet in
	// the form (Enabled, ContentMaxAge, QuotaGB, etc.) are preserved rather
	// than reset to their zero values.
	var rp repo.Repository
	if isEdit {
		rp, _ = s.Repos.Get(name)
	} else {
		rp.Enabled = true // new repos default to online
	}
	// Overlay the form-controlled fields.
	rp.Name = name
	rp.Format = r.FormValue("format")
	rp.Kind = repo.Kind(r.FormValue("kind"))
	rp.Upstream = strings.TrimSpace(r.FormValue("upstream"))
	rp.Members = members
	rp.AnonymousRead = r.FormValue("anonymousRead") == "on"
	rp.ProxyAuth = strings.TrimSpace(r.FormValue("proxyAuth"))
	rp.ProxyTTL = ttl
	if ttl > 0 {
		rp.ContentMaxAge = &ttl
	}
	rp.CleanupPolicyName = strings.TrimSpace(r.FormValue("cleanupPolicyName"))
	rp.Immutable = r.FormValue("immutable") == "on" // hosted write-once (IsImmutable gates on kind)
	overlayDepGuardFields(r, &rp)

	// BE-D fields — only overlay when the form field was actually submitted.
	if v := r.FormValue("enabled"); v != "" {
		rp.Enabled = v == "true"
	}
	if v := strings.TrimSpace(r.FormValue("blobStore")); v != "" && v != "default" {
		rp.BlobStore = v
	} else if v == "default" {
		rp.BlobStore = ""
	}

	if rp.Kind == repo.Proxy {
		if raw := strings.TrimSpace(r.FormValue("contentMaxAge")); raw != "" {
			if mins, err := strconv.Atoi(raw); err == nil {
				d := time.Duration(mins) * time.Minute
				rp.ContentMaxAge = &d
			}
		}
		if raw := strings.TrimSpace(r.FormValue("metadataMaxAge")); raw != "" {
			if mins, err := strconv.Atoi(raw); err == nil {
				d := time.Duration(mins) * time.Minute
				rp.MetadataMaxAge = &d
			}
		}
		rp.NegativeCache = parseBoolField(r, "negativeCache")
		rp.AutoBlock = parseBoolField(r, "autoBlock")
		if raw := strings.TrimSpace(r.FormValue("timeoutSecs")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				rp.TimeoutSecs = &n
			}
		}
		if raw := strings.TrimSpace(r.FormValue("retries")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
				rp.Retries = &n
			}
		}
	}
	if raw := strings.TrimSpace(r.FormValue("quotaGB")); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
			rp.QuotaGB = &f
		}
	}

	if msg := validateRepo(rp); msg != "" {
		s.reRenderForm(w, r, existingName, isEdit, msg)
		return
	}

	if isEdit && s.configOwns(config.KindRepository, existingName) && !s.configOverride {
		s.reRenderForm(w, r, existingName, isEdit,
			"This repository is managed by "+s.configSource+". Edit it there and redeploy.")
		return
	}
	var opErr error
	if isEdit {
		opErr = s.Repos.Update(rp)
	} else {
		opErr = s.Repos.Add(rp)
	}
	if opErr != nil {
		s.reRenderForm(w, r, existingName, isEdit, opErr.Error())
		return
	}

	if isEdit {
		http.Redirect(w, r, "/ui/admin/repos/"+name+"/edit?tab=settings&flash=Saved", http.StatusSeeOther) // #nosec G710
		return
	}
	http.Redirect(w, r, "/ui/admin/?flash=Created+repository+"+name, http.StatusSeeOther) // #nosec G710
}

func (s *Server) reRenderForm(w http.ResponseWriter, r *http.Request, name string, isEdit bool, errMsg string) {
	var rp repo.Repository
	if isEdit {
		rp, _ = s.Repos.Get(name)
	}
	// Overlay form values so the user doesn't lose their input.
	if v := r.FormValue("format"); v != "" {
		rp.Format = v
	}
	if v := r.FormValue("kind"); v != "" {
		rp.Kind = repo.Kind(v)
	}
	rp.Name = r.FormValue("name")
	if isEdit {
		rp.Name = name
	}
	rp.Upstream = r.FormValue("upstream")
	rp.ProxyAuth = r.FormValue("proxyAuth")
	rp.AnonymousRead = r.FormValue("anonymousRead") == "on"
	rp.CleanupPolicyName = strings.TrimSpace(r.FormValue("cleanupPolicyName"))
	rp.Immutable = r.FormValue("immutable") == "on" // hosted write-once (IsImmutable gates on kind)
	overlayDepGuardFields(r, &rp)
	if v := r.FormValue("enabled"); v != "" {
		rp.Enabled = v == "true"
	}
	if repo.Kind(r.FormValue("kind")) == repo.Proxy {
		if raw := strings.TrimSpace(r.FormValue("contentMaxAge")); raw != "" {
			if mins, err := strconv.Atoi(raw); err == nil {
				d := time.Duration(mins) * time.Minute
				rp.ContentMaxAge = &d
			}
		}
		if raw := strings.TrimSpace(r.FormValue("metadataMaxAge")); raw != "" {
			if mins, err := strconv.Atoi(raw); err == nil {
				d := time.Duration(mins) * time.Minute
				rp.MetadataMaxAge = &d
			}
		}
		rp.NegativeCache = parseBoolField(r, "negativeCache")
		rp.AutoBlock = parseBoolField(r, "autoBlock")
	}

	if isEdit {
		s.renderRepoConfig(w, rp, "settings", errMsg, "")
		return
	}
	render(w, tmplAdminForm, "admin_shell.html", adminFormPage{
		Title:       "Admin — New repository",
		ActiveNav:   "repos",
		Repo:        rp,
		KindStr:     string(rp.Kind),
		Error:       errMsg,
		Formats:     allFormats,
		Kinds:       allKinds,
		PolicyNames: s.policyNames(),
		Members:     s.memberOptions(rp),
	})
}

// ── token management ──────────────────────────────────────────────────────────

func (s *Server) uiAdminTokens(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "tokens"
	}

	if r.Method == http.MethodPost {
		switch tab {
		case "users":
			s.processUserInviteForm(w, r)
		case "roles":
			s.processRoleCreateForm(w, r)
		default:
			s.processTokenFormV2(w, r)
		}
		return
	}

	page := s.buildTokensPageV2("", "", defaultTokenForm())
	page.ActiveTab = tab
	if tab == "users" || tab == "roles" {
		page.Users = s.buildUsersTabData()
		page.Roles = s.buildRolesTabData()
	}
	render(w, tmplAdminTokens, "admin_shell.html", page)
}

func (s *Server) processUserInviteForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.Users == nil {
		s.renderTokensTab(w, "users", "User management not enabled.", "")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	if role == "" {
		role = "Reader"
	}
	if username == "" || password == "" {
		s.renderTokensTab(w, "users", "Username and password are required.", "")
		return
	}
	if _, err := s.Users.Create(username, password, role); err != nil {
		s.renderTokensTab(w, "users", err.Error(), "")
		return
	}
	s.renderTokensTab(w, "users", "", "User "+username+" created.")
}

func (s *Server) processRoleCreateForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.Roles == nil {
		s.renderTokensTab(w, "roles", "Role management not enabled.", "")
		return
	}
	role := auth.CustomRole{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		BaseRole:    r.FormValue("baseRole"),
	}
	if role.Name == "" {
		s.renderTokensTab(w, "roles", "Role name is required.", "")
		return
	}
	if err := s.Roles.Create(role); err != nil {
		s.renderTokensTab(w, "roles", err.Error(), "")
		return
	}
	s.renderTokensTab(w, "roles", "", "Role "+role.Name+" created.")
}

func (s *Server) renderTokensTab(w http.ResponseWriter, tab, errMsg, flash string) {
	page := s.buildTokensPageV2("", "", defaultTokenForm())
	page.ActiveTab = tab
	page.Error = errMsg
	page.Flash = flash
	page.Users = s.buildUsersTabData()
	page.Roles = s.buildRolesTabData()
	render(w, tmplAdminTokens, "admin_shell.html", page)
}

func (s *Server) uiAdminRevokeToken(w http.ResponseWriter, r *http.Request, id string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if s.Auth == nil {
		http.Error(w, "auth not enabled", http.StatusNotImplemented)
		return
	}
	s.Auth.Revoke(id) //nolint:errcheck
	// htmx swaps out the row; return empty 200 (the row disappears).
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uiAdminDeleteUser(w http.ResponseWriter, r *http.Request, username string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if s.Users == nil {
		http.Error(w, "user management not enabled", http.StatusNotImplemented)
		return
	}
	s.Users.Delete(username) //nolint:errcheck
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uiAdminToggleUser(w http.ResponseWriter, r *http.Request, username string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	if s.Users == nil {
		http.Error(w, "user management not enabled", http.StatusNotImplemented)
		return
	}
	u, ok, _ := s.Users.Get(username)
	if !ok {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	s.Users.SetDisabled(username, !u.Disabled) //nolint:errcheck
	// Redirect back to the users tab.
	http.Redirect(w, r, "/ui/admin/tokens?tab=users", http.StatusSeeOther) // #nosec G710
}

// parseGrantRows reads the grant-builder fields g{N}_repo / g{N}_actions /
// g{N}_selectors. Row indices may be sparse (the builder does not re-index
// when a middle row is removed), so it scans the form keys.
func parseGrantRows(r *http.Request) []grantRow {
	var idxs []int
	for key := range r.Form {
		if !strings.HasPrefix(key, "g") || !strings.HasSuffix(key, "_repo") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(key, "g"), "_repo")); err == nil {
			idxs = append(idxs, n)
		}
	}
	sort.Ints(idxs)
	rows := make([]grantRow, 0, len(idxs))
	for _, i := range idxs {
		prefix := "g" + strconv.Itoa(i) + "_"
		row := grantRow{
			Repo:      r.FormValue(prefix + "repo"),
			Actions:   map[string]bool{},
			Selectors: strings.TrimSpace(r.FormValue(prefix + "selectors")),
		}
		for _, a := range r.Form[prefix+"actions"] {
			row.Actions[a] = true
		}
		rows = append(rows, row)
	}
	return rows
}

// toGrants converts builder rows into auth grants. Selector lists are
// comma- or whitespace-separated.
func toGrants(rows []grantRow) []auth.Grant {
	grants := make([]auth.Grant, 0, len(rows))
	for _, row := range rows {
		g := auth.Grant{Repo: row.Repo}
		for _, a := range auth.AllActions { // stable verb order
			if row.Actions[string(a)] {
				g.Actions = append(g.Actions, a)
			}
		}
		for _, sel := range strings.FieldsFunc(row.Selectors, func(c rune) bool {
			return c == ',' || c == ' ' || c == '\t' || c == '\n'
		}) {
			g.Selectors = append(g.Selectors, sel)
		}
		grants = append(grants, g)
	}
	return grants
}

// buildTokensPage assembles the adminTokensPage data, loading the live token
// list and repo names each time so the table is always current.
func (s *Server) buildTokensPage(errMsg, newSecret string, form tokenForm) adminTokensPage {
	page := adminTokensPage{
		Title:       "Admin — API Tokens",
		AuthEnabled: s.Auth != nil,
		Form:        form,
		NewSecret:   newSecret,
		Error:       errMsg,
	}
	if s.Auth == nil {
		return page
	}

	tokens, _ := s.Auth.List()
	now := time.Now()
	for _, t := range tokens {
		lastUsed := "never"
		if t.LastUsed != nil {
			lastUsed = t.LastUsed.UTC().Format("2006-01-02 15:04")
		}
		statusClass, statusLabel := "dot-ok", "Active"
		if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
			statusClass, statusLabel = "dot-err", "Expired"
		} else if t.LastUsed == nil {
			statusClass, statusLabel = "dot-neutral", "Never used"
		}
		page.Tokens = append(page.Tokens, tokenRow{
			ID:          t.ID,
			Description: t.Description,
			Grants:      grantViews(t.Grants),
			CreatedStr:  t.CreatedAt.UTC().Format("2006-01-02"),
			ExpiresStr:  formatExpiry(t.ExpiresAt),
			Owner:       t.Owner,
			LastUsedStr: lastUsed,
			StatusClass: statusClass,
			StatusLabel: statusLabel,
		})
	}

	for _, rp := range s.Repos.All() {
		page.AllRepos = append(page.AllRepos, rp.Name)
	}
	return page
}

// buildTokensPageV2 wraps buildTokensPage for the sidebar layout.
func (s *Server) buildTokensPageV2(errMsg, newSecret string, form tokenForm) adminTokensV2Page {
	base := s.buildTokensPage(errMsg, newSecret, form)
	base.Title = "Tokens & Access"
	return adminTokensV2Page{adminTokensPage: base, ActiveNav: "tokens", ActiveTab: "tokens"}
}

// buildUsersPage populates the Users tab.
func (s *Server) buildUsersTabData() []userRow {
	if s.Users == nil {
		return nil
	}
	users, _ := s.Users.List()
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		lastLogin := "never"
		if u.LastLogin != nil {
			lastLogin = u.LastLogin.UTC().Format("2006-01-02 15:04")
		}
		statusClass, statusLabel := "dot-ok", "Active"
		if u.Disabled {
			statusClass, statusLabel = "dot-err", "Disabled"
		}
		rows = append(rows, userRow{
			Username:     u.Username,
			DisplayName:  u.DisplayName,
			Role:         u.Role,
			RoleClass:    roleClass(u.Role),
			CreatedStr:   u.CreatedAt.UTC().Format("2006-01-02"),
			LastLoginStr: lastLogin,
			StatusClass:  statusClass,
			StatusLabel:  statusLabel,
			Disabled:     u.Disabled,
		})
	}
	return rows
}

// buildRolesTabData populates the Roles tab with predefined + custom roles,
// computing member counts from the user list.
func (s *Server) buildRolesTabData() []roleCard {
	// Count users per role name.
	memberCount := map[string]int{}
	if s.Users != nil {
		if users, err := s.Users.List(); err == nil {
			for _, u := range users {
				memberCount[u.Role]++
			}
		}
	}

	var cards []roleCard
	for _, p := range auth.PredefinedRoles {
		cards = append(cards, roleCard{
			Name:         p.Name,
			Description:  p.Description,
			BaseRole:     p.BaseRole,
			RoleClass:    roleClass(p.Name),
			MemberCount:  memberCount[p.Name],
			IsPredefined: true,
		})
	}
	if s.Roles != nil {
		if custom, err := s.Roles.List(); err == nil {
			for _, r := range custom {
				cards = append(cards, roleCard{
					Name:         r.Name,
					Description:  r.Description,
					BaseRole:     r.BaseRole,
					RoleClass:    roleClass(r.Name),
					MemberCount:  memberCount[r.Name],
					IsPredefined: false,

					ManagedByConfig: s.configOwns(config.KindRole, r.Name),
				})
			}
		}
	}
	return cards
}

func roleClass(name string) string {
	switch auth.BaseRoleFor(name) {
	case auth.RoleRead:
		return "scope-read"
	case auth.RoleWrite:
		return "scope-write"
	case auth.RoleAdmin:
		return "scope-admin"
	}
	return "scope-read"
}

// processTokenFormV2 handles POST for the sidebar tokens page: the grant
// builder submits one g{N}_repo/actions/selectors group per grant row.
func (s *Server) processTokenFormV2(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	form := tokenForm{
		Description: strings.TrimSpace(r.FormValue("description")),
		Expires:     r.FormValue("expires"),
		Rows:        parseGrantRows(r),
	}
	if len(form.Rows) == 0 {
		form.Rows = defaultTokenForm().Rows
	}

	fail := func(msg string) {
		render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2(msg, "", form))
	}

	if form.Description == "" {
		fail("description is required")
		return
	}
	if s.Auth == nil {
		fail("auth not enabled")
		return
	}

	grants := toGrants(form.Rows)
	if err := auth.ValidateGrants(grants); err != nil {
		fail(err.Error())
		return
	}

	var expiresAt *time.Time
	if form.Expires != "" {
		t, err := time.ParseInLocation("2006-01-02", form.Expires, time.UTC)
		if err != nil {
			fail("invalid expiry date (use YYYY-MM-DD)")
			return
		}
		// Expire at end of the chosen day.
		t = t.Add(24*time.Hour - time.Second)
		expiresAt = &t
	}

	_, secret, err := s.Auth.Create(form.Description, grants, expiresAt)
	if err != nil {
		fail("failed to create token: " + err.Error())
		return
	}

	// Re-render with the secret displayed once and the form reset.
	render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("", secret, defaultTokenForm()))
}

// grantViews converts grants into their table representation.
func grantViews(grants []auth.Grant) []tokenGrantView {
	views := make([]tokenGrantView, 0, len(grants))
	for _, g := range grants {
		v := tokenGrantView{Repo: g.Repo, Selectors: strings.Join(g.Selectors, ", ")}
		for _, a := range g.Actions {
			v.Actions = append(v.Actions, string(a))
		}
		views = append(views, v)
	}
	return views
}

func formatActions(actions []auth.Action) string {
	parts := make([]string, 0, len(actions))
	for _, a := range actions {
		parts = append(parts, string(a))
	}
	return strings.Join(parts, ",")
}

func formatExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02")
}

// ── access view ───────────────────────────────────────────────────────────────

func (s *Server) uiAdminAccess(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	page := adminAccessPage{
		Title:       "Admin — Access",
		ActiveNav:   "tokens",
		AuthEnabled: s.Auth != nil,
	}

	if s.OIDC != nil {
		page.SSO = ssoInfo{
			Enabled:     true,
			Issuer:      s.OIDC.Issuer(),
			ClientID:    s.OIDC.ClientID(),
			RedirectURL: s.OIDC.RedirectURL(),
			GroupsClaim: s.OIDC.GroupsClaim(),
		}
		for _, rule := range s.GroupMapper.Rules() {
			page.SSO.Mappings = append(page.SSO.Mappings, ssoMapping{
				Group: rule.Group,
				Role:  rule.Role.String(),
			})
		}
	}

	if s.LDAP != nil {
		page.LDAP = ldapInfo{
			Enabled:    true,
			URLs:       s.LDAP.URLs(),
			BindDN:     s.LDAP.BindDN(),
			UserBaseDN: s.LDAP.UserBaseDN(),
			UserFilter: s.LDAP.UserFilter(),
			GroupMode:  s.LDAP.GroupMode(),
		}
		for _, rule := range s.ldapMapper.Rules() {
			page.LDAP.Mappings = append(page.LDAP.Mappings, ssoMapping{
				Group: rule.Group,
				Role:  rule.Role.String(),
			})
		}
	}

	if s.Auth != nil {
		tokens, _ := s.Auth.List()
		for _, rp := range s.Repos.All() {
			row := accessRow{
				RepoName:      rp.Name,
				Format:        rp.Format,
				Kind:          string(rp.Kind),
				AnonymousRead: rp.AnonymousRead,
			}
			for _, tok := range tokens {
				for _, g := range tok.Grants {
					if g.Repo == rp.Name || g.Repo == "*" {
						row.Grants = append(row.Grants, repoGrant{
							Role:        formatActions(g.Actions),
							Description: tok.Description,
						})
					}
				}
			}
			page.Rows = append(page.Rows, row)
		}
	}

	render(w, tmplAccess, "admin_shell.html", page)
}
