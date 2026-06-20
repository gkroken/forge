package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/repo"
)

var (
	allFormats = []string{"maven", "npm", "helm", "cran", "oci"}
	allKinds   = []string{"hosted", "proxy", "group"}
)

// ── page data types ───────────────────────────────────────────────────────────

type adminReposPage struct {
	Title     string
	ActiveNav string
	Repos     []repo.Repository
	Flash     string
}

type adminFormPage struct {
	Title       string
	Repo        repo.Repository
	KindStr     string // string(Repo.Kind) — avoids named-type comparison in templates
	IsEdit      bool
	Error       string
	Formats     []string
	Kinds       []string
	PolicyNames []string // named cleanup policies available for selection
}

type repoConfigPage struct {
	Title          string
	ActiveNav      string
	Repo           repo.Repository
	KindStr        string
	Error          string
	Flash          string
	Formats        []string
	Kinds          []string
	PolicyNames    []string
	ActiveTab      string // "settings" | "content" | "access" | "activity"
	ArtifactCount  int
	SizeBytes      int64
	StoragePct     int
	RecentActivity []auditRow
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

type adminAccessPage struct {
	Title       string
	AuthEnabled bool
	Rows        []accessRow
}

// ── token types ───────────────────────────────────────────────────────────────

// tokenRow is a display-ready snapshot of one token for the template.
type tokenRow struct {
	ID           string
	Description  string
	GrantSummary string
	CreatedStr   string
	ExpiresStr   string
	Owner        string // from auth.Token.Owner (empty if not set)
	LastUsedStr  string // formatted auth.Token.LastUsed; "never" if nil
	StatusClass  string // CSS dot class: dot-ok / dot-err / dot-neutral
	StatusLabel  string // "Active" | "Expired" | "Never used"
}

// tokenForm holds the last-submitted (or default) create-token form values
// so the template can round-trip them on validation errors.
type tokenForm struct {
	Description string
	Repo        string
	Role        string
	Expires     string
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
}

// adminTokensV2Page wraps adminTokensPage for the sidebar (Foundry) layout.
type adminTokensV2Page struct {
	adminTokensPage
	ActiveNav string
	ActiveTab string     // "tokens" | "users" | "roles"
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
	case sub == "/observability":
		s.uiObservability(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (s *Server) uiAdminHome(w http.ResponseWriter, r *http.Request) {
	render(w, tmplAdminRepos, "admin_shell.html", adminReposPage{
		Title:     "Repositories",
		ActiveNav: "repos",
		Repos:     s.Repos.All(),
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
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:       "Admin — New repository",
		Repo:        repo.Repository{Kind: repo.Hosted},
		KindStr:     "hosted",
		Formats:     allFormats,
		Kinds:       allKinds,
		PolicyNames: s.policyNames(),
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

func (s *Server) renderRepoConfig(w http.ResponseWriter, rp repo.Repository, tab, errMsg, flash string) {
	bsizes := s.GetBlobSizes()
	sizeBytes := bsizes.ByRepo[rp.Name]
	storagePct := 0
	if bsizes.TotalBytes > 0 {
		storagePct = int(float64(sizeBytes) / float64(bsizes.TotalBytes) * 100)
	}

	var activity []auditRow
	if s.AuditLog != nil {
		needle := "/" + rp.Name
		methodVerbs := map[string]string{
			"POST": "Published", "PUT": "Uploaded",
			"DELETE": "Deleted", "PATCH": "Updated", "GET": "Downloaded",
		}
		for _, e := range s.AuditLog.Recent(100) {
			if !strings.Contains(e.Path, needle) {
				continue
			}
			action := methodVerbs[e.Method]
			if action == "" {
				action = e.Method
			}
			if e.Status >= 400 {
				action = "Denied"
			}
			activity = append(activity, auditRow{
				Time:   e.Timestamp.UTC().Format("15:04:05"),
				Actor:  e.Actor,
				Action: action,
				Status: strconv.Itoa(e.Status),
				OK:     e.Status < 400,
			})
			if len(activity) == 5 {
				break
			}
		}
	}

	render(w, tmplRepoConfig, "admin_shell.html", repoConfigPage{
		Title:          rp.Name + " — Settings",
		ActiveNav:      "repos",
		Repo:           rp,
		KindStr:        string(rp.Kind),
		Error:          errMsg,
		Flash:          flash,
		Formats:        allFormats,
		Kinds:          allKinds,
		PolicyNames:    s.policyNames(),
		ActiveTab:      tab,
		ArtifactCount:  bsizes.CountByRepo[rp.Name],
		SizeBytes:      sizeBytes,
		StoragePct:     storagePct,
		RecentActivity: activity,
	})
}

func (s *Server) uiAdminDeleteRepo(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
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

	var members []string
	if raw := strings.TrimSpace(r.FormValue("members")); raw != "" {
		for _, m := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(m); t != "" {
				members = append(members, t)
			}
		}
	}

	rp := repo.Repository{
		Name:          name,
		Format:        r.FormValue("format"),
		Kind:          repo.Kind(r.FormValue("kind")),
		Upstream:      strings.TrimSpace(r.FormValue("upstream")),
		Members:       members,
		AnonymousRead: r.FormValue("anonymousRead") == "on",
		ProxyAuth:     strings.TrimSpace(r.FormValue("proxyAuth")),
		ProxyTTL:          ttl,
		CleanupPolicyName: strings.TrimSpace(r.FormValue("cleanupPolicyName")),
	}

	if msg := validateRepo(rp); msg != "" {
		s.reRenderForm(w, r, existingName, isEdit, msg)
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

	if isEdit {
		s.renderRepoConfig(w, rp, "settings", errMsg, "")
		return
	}
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:       "Admin — New repository",
		Repo:        rp,
		KindStr:     string(rp.Kind),
		Error:       errMsg,
		Formats:     allFormats,
		Kinds:       allKinds,
		PolicyNames: s.policyNames(),
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

	page := s.buildTokensPageV2("", "", tokenForm{Repo: "*", Role: "read"})
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
	page := s.buildTokensPageV2("", "", tokenForm{Repo: "*", Role: "read"})
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

func (s *Server) processTokenForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	form := tokenForm{
		Description: strings.TrimSpace(r.FormValue("description")),
		Repo:        r.FormValue("repo"),
		Role:        r.FormValue("role"),
		Expires:     r.FormValue("expires"),
	}

	if form.Description == "" {
		render(w, tmplTokens, "base.html", s.buildTokensPage("description is required", "", form))
		return
	}
	if s.Auth == nil {
		render(w, tmplTokens, "base.html", s.buildTokensPage("auth not enabled", "", form))
		return
	}

	role, err := auth.ParseRole(form.Role)
	if err != nil {
		render(w, tmplTokens, "base.html", s.buildTokensPage("invalid role: "+form.Role, "", form))
		return
	}

	repoName := form.Repo
	if repoName == "" {
		repoName = "*"
	}

	var expiresAt *time.Time
	if form.Expires != "" {
		t, err := time.ParseInLocation("2006-01-02", form.Expires, time.UTC)
		if err != nil {
			render(w, tmplTokens, "base.html", s.buildTokensPage("invalid expiry date (use YYYY-MM-DD)", "", form))
			return
		}
		// Expire at end of the chosen day.
		t = t.Add(24*time.Hour - time.Second)
		expiresAt = &t
	}

	_, secret, err := s.Auth.Create(form.Description, []auth.Grant{{Repo: repoName, Role: role}}, expiresAt)
	if err != nil {
		render(w, tmplTokens, "base.html", s.buildTokensPage("failed to create token: "+err.Error(), "", form))
		return
	}

	// Re-render with the secret displayed once and form reset to defaults.
	render(w, tmplTokens, "base.html", s.buildTokensPage("", secret, tokenForm{Repo: "*", Role: "read"}))
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
			ID:           t.ID,
			Description:  t.Description,
			GrantSummary: formatGrants(t.Grants),
			CreatedStr:   t.CreatedAt.UTC().Format("2006-01-02"),
			ExpiresStr:   formatExpiry(t.ExpiresAt),
			Owner:        t.Owner,
			LastUsedStr:  lastUsed,
			StatusClass:  statusClass,
			StatusLabel:  statusLabel,
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

// processTokenFormV2 handles POST for the sidebar tokens page.
func (s *Server) processTokenFormV2(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	form := tokenForm{
		Description: strings.TrimSpace(r.FormValue("description")),
		Repo:        r.FormValue("repo"),
		Role:        r.FormValue("role"),
		Expires:     r.FormValue("expires"),
	}

	if form.Description == "" {
		render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("description is required", "", form))
		return
	}
	if s.Auth == nil {
		render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("auth not enabled", "", form))
		return
	}

	role, err := auth.ParseRole(form.Role)
	if err != nil {
		render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("invalid role: "+form.Role, "", form))
		return
	}

	repoName := form.Repo
	if repoName == "" {
		repoName = "*"
	}

	var expiresAt *time.Time
	if form.Expires != "" {
		t, err := time.ParseInLocation("2006-01-02", form.Expires, time.UTC)
		if err != nil {
			render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("invalid expiry date (use YYYY-MM-DD)", "", form))
			return
		}
		t = t.Add(24*time.Hour - time.Second)
		expiresAt = &t
	}

	_, secret, err := s.Auth.Create(form.Description, []auth.Grant{{Repo: repoName, Role: role}}, expiresAt)
	if err != nil {
		render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("failed to create token: "+err.Error(), "", form))
		return
	}

	render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("", secret, tokenForm{Repo: "*", Role: "read"}))
}

func formatGrants(grants []auth.Grant) string {
	parts := make([]string, 0, len(grants))
	for _, g := range grants {
		parts = append(parts, g.Role.String()+" on "+g.Repo)
	}
	return strings.Join(parts, ", ")
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
		AuthEnabled: s.Auth != nil,
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
							Role:        g.Role.String(),
							Description: tok.Description,
						})
						break
					}
				}
			}
			page.Rows = append(page.Rows, row)
		}
	}

	render(w, tmplAccess, "base.html", page)
}
