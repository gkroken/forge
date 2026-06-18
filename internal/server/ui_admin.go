package server

import (
	"net/http"
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
	Title string
	Repos []repo.Repository
	Flash string
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

// adminTokensV2Page wraps adminTokensPage for the sidebar (Foundry) layout.
type adminTokensV2Page struct {
	adminTokensPage
	ActiveNav string
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
	render(w, tmplAdminRepos, "base.html", adminReposPage{
		Title: "Admin — Repositories",
		Repos: s.Repos.All(),
		Flash: r.URL.Query().Get("flash"),
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
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:       "Admin — Edit " + name,
		Repo:        rp,
		KindStr:     string(rp.Kind),
		IsEdit:      true,
		Formats:     allFormats,
		Kinds:       allKinds,
		PolicyNames: s.policyNames(),
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

	action := "Created"
	if isEdit {
		action = "Updated"
	}
	http.Redirect(w, r, "/ui/admin/?flash="+action+"+repository+"+name, http.StatusSeeOther) // #nosec G710 -- target is a hardcoded /ui/admin/ prefix
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

	title := "Admin — New repository"
	if isEdit {
		title = "Admin — Edit " + name
	}
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:       title,
		Repo:        rp,
		KindStr:     string(rp.Kind),
		IsEdit:      isEdit,
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

	if r.Method == http.MethodPost {
		s.processTokenFormV2(w, r)
		return
	}

	render(w, tmplAdminTokens, "admin_shell.html", s.buildTokensPageV2("", "", tokenForm{Repo: "*", Role: "read"}))
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
	for _, t := range tokens {
		page.Tokens = append(page.Tokens, tokenRow{
			ID:           t.ID,
			Description:  t.Description,
			GrantSummary: formatGrants(t.Grants),
			CreatedStr:   t.CreatedAt.UTC().Format("2006-01-02"),
			ExpiresStr:   formatExpiry(t.ExpiresAt),
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
	return adminTokensV2Page{adminTokensPage: base, ActiveNav: "tokens"}
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
