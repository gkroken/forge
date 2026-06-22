package server

import (
	"net/http"
	"net/url"
	"sort"

	"forge/internal/format"
	"forge/internal/vuln"
)

// securityPageSize is the number of finding rows per Security page.
const securityPageSize = 50

// cursorSep separates the (repo, component, version) parts of the keyset cursor.
// 0x1f (unit separator) can't appear in a repo/component/version, so the cursor
// round-trips unambiguously.
const cursorSep = "\x1f"

type securityPage struct {
	Title      string
	ActiveNav  string
	Replica    string
	Rows       []securityRow
	Repo       string   // echoed repo filter
	Severity   string   // echoed min-severity filter
	AllRepos   []string // scannable repos, for the filter dropdown
	Severities []string // selectable min-severity values
	Enabled    bool     // false → scanning not configured (empty-state message)
	HasMore    bool
	OlderURL   string // keyset link to the next page, preserving filters
	ResetURL   string // back to the first page with current filters
}

// securityRow is one vulnerable component@version in the findings list.
type securityRow struct {
	Repo      string
	Component string
	Version   string
	Severity  string // worst severity label
	Count     int    // number of advisories
	ScannedAt string
}

// uiSecurity renders GET /ui/admin/security — every vulnerable finding across
// all repos, filterable by repo and minimum severity, keyset-paginated by
// (repo, component, version). It pages in memory over the per-repo sorted
// findings; a dedicated keyset query (the audit_log precedent) is the scale
// path if the findings set ever outgrows that.
func (s *Server) uiSecurity(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	repoFilter := r.URL.Query().Get("repo")
	sevFilter := r.URL.Query().Get("severity")
	minSev := vuln.ParseSeverity(sevFilter) // SeverityUnknown when blank → no floor

	page := securityPage{
		Title:      "Security",
		ActiveNav:  "security",
		Replica:    replicaID(),
		Repo:       repoFilter,
		Severity:   sevFilter,
		Severities: []string{"low", "moderate", "high", "critical"},
		Enabled:    s.Vuln != nil,
		ResetURL:   securityURL(repoFilter, sevFilter, ""),
	}

	if s.Vuln == nil {
		render(w, tmplSecurity, "admin_shell.html", page)
		return
	}

	// Gather vulnerable findings across the selected repo(s). Each repo's List is
	// already sorted by (component, version); we merge and re-sort by
	// (repo, component, version) for a stable global keyset order.
	type row struct {
		repo string
		f    vuln.Finding
	}
	var all []row
	for _, rp := range s.Repos.All() {
		h, ok := s.Handlers.For(rp.Format)
		if !ok {
			continue
		}
		if _, scannable := h.(format.VulnCoordinates); !scannable {
			continue // only list repos that can produce findings
		}
		page.AllRepos = append(page.AllRepos, rp.Name)
		if repoFilter != "" && rp.Name != repoFilter {
			continue
		}
		findings, err := s.Vuln.List(rp.Name)
		if err != nil {
			continue
		}
		for _, f := range findings {
			if len(f.Advisories) == 0 {
				continue // clean: not a finding to list
			}
			if sevFilter != "" && f.Worst() < minSev {
				continue
			}
			all = append(all, row{rp.Name, f})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].repo != all[j].repo {
			return all[i].repo < all[j].repo
		}
		if all[i].f.Component != all[j].f.Component {
			return all[i].f.Component < all[j].f.Component
		}
		return all[i].f.Version < all[j].f.Version
	})

	// Apply the keyset cursor: start just after the (repo, component, version)
	// encoded in ?after=.
	start := 0
	if after := r.URL.Query().Get("after"); after != "" {
		for i, rw := range all {
			if rowCursor(rw.repo, rw.f.Component, rw.f.Version) > after {
				start = i
				break
			}
			start = i + 1
		}
	}

	window := all[start:]
	page.HasMore = len(window) > securityPageSize
	if page.HasMore {
		window = window[:securityPageSize]
	}
	for _, rw := range window {
		scanned := ""
		if !rw.f.ScannedAt.IsZero() {
			scanned = rw.f.ScannedAt.UTC().Format("2006-01-02 15:04")
		}
		page.Rows = append(page.Rows, securityRow{
			Repo:      rw.repo,
			Component: rw.f.Component,
			Version:   rw.f.Version,
			Severity:  rw.f.Worst().String(),
			Count:     len(rw.f.Advisories),
			ScannedAt: scanned,
		})
	}
	if page.HasMore && len(window) > 0 {
		last := window[len(window)-1]
		page.OlderURL = securityURL(repoFilter, sevFilter,
			rowCursor(last.repo, last.f.Component, last.f.Version))
	}

	render(w, tmplSecurity, "admin_shell.html", page)
}

func rowCursor(repo, component, version string) string {
	return repo + cursorSep + component + cursorSep + version
}

// securityURL builds a /ui/admin/security link preserving filters and, when set,
// the keyset cursor.
func securityURL(repoFilter, sevFilter, after string) string {
	v := url.Values{}
	if repoFilter != "" {
		v.Set("repo", repoFilter)
	}
	if sevFilter != "" {
		v.Set("severity", sevFilter)
	}
	if after != "" {
		v.Set("after", after)
	}
	if enc := v.Encode(); enc != "" {
		return "/ui/admin/security?" + enc
	}
	return "/ui/admin/security"
}
