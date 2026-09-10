package nexus

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"forge/internal/auth"
	"forge/internal/repo"
)

// formatMap translates a Nexus format name to a forge format name.
var formatMap = map[string]string{
	"maven2": "maven",
	"npm":    "npm",
	"helm":   "helm",
	"r":      "cran",
	"docker": "oci",
	"pypi":   "pypi",
}

// MapFormat translates a Nexus format to forge's, reporting whether forge
// supports it.
func MapFormat(nexusFormat string) (string, bool) {
	f, ok := formatMap[strings.ToLower(nexusFormat)]
	return f, ok
}

// RepoPlan is the migration decision for one source repository.
type RepoPlan struct {
	Source       string    `json:"source"`
	SourceFormat string    `json:"sourceFormat"`
	SourceType   string    `json:"sourceType"`
	Target       string    `json:"target,omitempty"`
	TargetFormat string    `json:"targetFormat,omitempty"`
	TargetKind   repo.Kind `json:"targetKind,omitempty"`
	Action       string    `json:"action"` // create | exists | skip
	Reason       string    `json:"reason,omitempty"`
	Upstream     string    `json:"upstream,omitempty"`
	Members      []string  `json:"members,omitempty"`

	// Source inventory, filled for hosted repos during planning.
	Components int `json:"components"`
	Assets     int `json:"assets"`
}

// Migratable reports whether this repo takes part in the apply phase.
func (p RepoPlan) Migratable() bool { return p.Action == "create" || p.Action == "exists" }

// MapRepo decides what happens to one source repository. existing is forge's
// current repo set (for idempotent re-runs and conflict detection).
func MapRepo(r Repository, existing map[string]repo.Repository) RepoPlan {
	p := RepoPlan{
		Source: r.Name, SourceFormat: r.Format, SourceType: strings.ToLower(r.Type),
	}
	f, ok := MapFormat(r.Format)
	if !ok {
		p.Action = "skip"
		p.Reason = fmt.Sprintf("format %q is not supported by forge", r.Format)
		return p
	}
	var kind repo.Kind
	switch p.SourceType {
	case "hosted":
		kind = repo.Hosted
	case "proxy":
		kind = repo.Proxy
	case "group":
		kind = repo.Group
	default:
		p.Action = "skip"
		p.Reason = fmt.Sprintf("unknown repository type %q", r.Type)
		return p
	}
	p.Target, p.TargetFormat, p.TargetKind = r.Name, f, kind
	p.Upstream = r.RemoteURL
	p.Members = r.Members

	if kind == repo.Proxy && r.RemoteURL == "" {
		p.Action = "skip"
		p.Reason = "proxy repository reports no remote URL (need admin credentials on the source to read repository settings)"
		return p
	}

	if ex, found := existing[r.Name]; found {
		if ex.Format == f && ex.Kind == kind {
			p.Action = "exists" // idempotent resume: reuse it
		} else {
			p.Action = "skip"
			p.Reason = fmt.Sprintf("forge already has a repository %q with format=%s kind=%s (source is %s/%s)",
				r.Name, ex.Format, ex.Kind, f, kind)
		}
		return p
	}
	p.Action = "create"
	return p
}

// ToRepository converts a create/exists plan row into a forge Repository.
func (p RepoPlan) ToRepository() repo.Repository {
	return repo.Repository{
		Name:     p.Target,
		Format:   p.TargetFormat,
		Kind:     p.TargetKind,
		Upstream: p.Upstream,
		Members:  p.Members,
		Enabled:  true,
	}
}

// --- privileges → grants -------------------------------------------------------

// mapActions translates Nexus privilege actions (BROWSE/READ/EDIT/ADD/DELETE/*)
// to forge content actions.
func mapActions(actions []string) []auth.Action {
	set := map[auth.Action]bool{}
	for _, a := range actions {
		switch strings.ToUpper(a) {
		case "BROWSE", "READ":
			set[auth.ActionRead] = true
		case "EDIT", "ADD":
			set[auth.ActionWrite] = true
		case "DELETE":
			set[auth.ActionDelete] = true
		case "*", "ALL":
			set[auth.ActionRead] = true
			set[auth.ActionWrite] = true
			set[auth.ActionDelete] = true
		}
	}
	var out []auth.Action
	for _, a := range auth.AllActions {
		if set[a] {
			out = append(out, a)
		}
	}
	return out
}

// cselPathRe matches the simple CSEL path conjuncts we can translate:
// path =~ "^/org/acme/.*" (and close variants). Anything else is unmappable.
var cselConjunctRe = regexp.MustCompile(`^\s*(format|path)\s*(==|=~)\s*"([^"]*)"\s*$`)

// TranslateCSEL converts a Nexus content-selector expression to a forge
// selector pattern (grammar in internal/selector). Only the honest subset is
// supported: conjunctions of an optional `format == "x"` filter and a single
// simple `path` term. Returns the selector, the format filter (may be empty),
// and ok=false when the expression is beyond the subset.
func TranslateCSEL(expr string) (sel, format string, ok bool) {
	if strings.ContainsAny(expr, "|") || strings.Contains(strings.ToLower(expr), " or ") {
		return "", "", false
	}
	var pathSel string
	for _, part := range strings.Split(expr, " and ") {
		m := cselConjunctRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return "", "", false
		}
		field, op, val := m[1], m[2], m[3]
		switch field {
		case "format":
			if op != "==" {
				return "", "", false
			}
			format = val
		case "path":
			if pathSel != "" {
				return "", "", false // two path terms: out of subset
			}
			s, pok := translatePathTerm(op, val)
			if !pok {
				return "", "", false
			}
			pathSel = s
		}
	}
	if pathSel == "" {
		return "", "", false
	}
	return pathSel, format, true
}

// translatePathTerm converts one CSEL path term to a forge selector.
//
//	path == "/org/acme/thing.jar"  → org/acme/thing.jar
//	path =~ "^/org/acme/.*"        → org/acme/**
//	path =~ "^/org/acme/"          → org/acme/**
func translatePathTerm(op, val string) (string, bool) {
	switch op {
	case "==":
		p := strings.TrimPrefix(val, "/")
		if p == "" || strings.ContainsAny(p, "*?[](){}^$+\\") {
			return "", false
		}
		return p, true
	case "=~":
		r := strings.TrimPrefix(val, "^")
		r = strings.TrimSuffix(r, "$")
		r = strings.TrimSuffix(r, ".*")
		r = strings.TrimPrefix(r, "/")
		r = strings.TrimSuffix(r, "/")
		if r == "" || strings.ContainsAny(r, "*?[](){}^$+\\|.") {
			// Any residual regex metacharacter puts the expression beyond the
			// subset we translate faithfully. (Dots are common in real regexes
			// as "any char"; translating them literally would silently widen
			// or narrow the selector, so we refuse.)
			return "", false
		}
		return r + "/**", true
	}
	return "", false
}

// GrantNote records something about a role's translation that the admin
// should read: a privilege that could not be mapped, and why.
type GrantNote struct {
	Privilege string `json:"privilege"`
	Reason    string `json:"reason"`
}

// grantKey groups grant fragments per (repo, selector) pair while translating.
type grantKey struct {
	repo string
	sel  string
}

// BuildGrants translates a set of Nexus privilege names into forge grants.
//
//	privByName — all privilege definitions on the source
//	selByName  — all content selectors on the source
//	reposOf    — target repo names per forge format, for expanding
//	             format-scoped wildcards (e.g. all-maven2-repos privileges);
//	             the caller passes only repos that are part of the migration.
func BuildGrants(privNames []string, privByName map[string]Privilege, selByName map[string]ContentSelector, reposOf func(forgeFormat string) []string) ([]auth.Grant, []GrantNote) {
	acc := map[grantKey]map[auth.Action]bool{}
	var notes []GrantNote

	add := func(repoName, sel string, actions []auth.Action) {
		if len(actions) == 0 {
			return
		}
		k := grantKey{repoName, sel}
		if acc[k] == nil {
			acc[k] = map[auth.Action]bool{}
		}
		for _, a := range actions {
			acc[k][a] = true
		}
	}

	// targets resolves a privilege's repository field to forge repo names.
	targets := func(p Privilege) []string {
		if p.Repository != "" && p.Repository != "*" {
			return []string{p.Repository}
		}
		// "*" scoped to a format means every repo of that format; with no
		// format it means every repository.
		if p.Format != "" && p.Format != "*" {
			f, ok := MapFormat(p.Format)
			if !ok {
				return nil
			}
			return reposOf(f)
		}
		return []string{"*"}
	}

	for _, name := range privNames {
		p, found := privByName[name]
		if !found {
			notes = append(notes, GrantNote{name, "privilege not found on source"})
			continue
		}
		switch p.Type {
		case "wildcard":
			// nx-all: everything, everywhere.
			add("*", "", []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete, auth.ActionAdmin})

		case "repository-view":
			acts := mapActions(p.Actions)
			reposHit := targets(p)
			if len(reposHit) == 0 {
				notes = append(notes, GrantNote{name, fmt.Sprintf("no migrated repositories match format %q", p.Format)})
				continue
			}
			for _, r := range reposHit {
				add(r, "", acts)
			}

		case "repository-admin":
			reposHit := targets(p)
			if len(reposHit) == 0 {
				notes = append(notes, GrantNote{name, fmt.Sprintf("no migrated repositories match format %q", p.Format)})
				continue
			}
			for _, r := range reposHit {
				add(r, "", []auth.Action{auth.ActionAdmin})
			}

		case "repository-content-selector":
			cs, csFound := selByName[p.ContentSelector]
			if !csFound {
				notes = append(notes, GrantNote{name, fmt.Sprintf("content selector %q not found on source", p.ContentSelector)})
				continue
			}
			sel, _, ok := TranslateCSEL(cs.Expression)
			if !ok {
				notes = append(notes, GrantNote{name, fmt.Sprintf("content selector %q uses an expression beyond the translatable subset: %s", cs.Name, cs.Expression)})
				continue
			}
			acts := mapActions(p.Actions)
			reposHit := targets(p)
			if len(reposHit) == 0 {
				notes = append(notes, GrantNote{name, fmt.Sprintf("no migrated repositories match format %q", p.Format)})
				continue
			}
			for _, r := range reposHit {
				add(r, sel, acts)
			}

		case "application", "script":
			notes = append(notes, GrantNote{name, "application/script privileges have no forge equivalent"})

		default:
			notes = append(notes, GrantNote{name, fmt.Sprintf("privilege type %q has no forge equivalent", p.Type)})
		}
	}

	// Collapse the accumulator into a deterministic grant list.
	keys := make([]grantKey, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].sel < keys[j].sel
	})
	var grants []auth.Grant
	for _, k := range keys {
		var actions []auth.Action
		for _, a := range auth.AllActions {
			if acc[k][a] {
				actions = append(actions, a)
			}
		}
		g := auth.Grant{Repo: k.repo, Actions: actions}
		if k.sel != "" {
			g.Selectors = []string{k.sel}
		}
		grants = append(grants, g)
	}
	return grants, notes
}

// FlattenRole resolves a role's transitive privilege set, following nested
// roles cycle-safely.
func FlattenRole(id string, byID map[string]Role) []string {
	seen := map[string]bool{}
	privs := map[string]bool{}
	var walk func(string)
	walk = func(rid string) {
		if seen[rid] {
			return
		}
		seen[rid] = true
		r, ok := byID[rid]
		if !ok {
			return
		}
		for _, p := range r.Privileges {
			privs[p] = true
		}
		for _, nested := range r.Roles {
			walk(nested)
		}
	}
	walk(id)
	out := make([]string, 0, len(privs))
	for p := range privs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// IsGlobalAdmin reports whether a grant list contains the wildcard-admin grant.
func IsGlobalAdmin(grants []auth.Grant) bool {
	for _, g := range grants {
		if g.Repo == "*" {
			for _, a := range g.Actions {
				if a == auth.ActionAdmin {
					return true
				}
			}
		}
	}
	return false
}

// BaseTierFor summarises a grant list as a forge base role name for the
// CustomRole.BaseRole fallback. Deliberately conservative: "admin" only for
// global admin (a repo-scoped admin grant must not fall back to global admin
// if a legacy code path consults the tier instead of the grants).
func BaseTierFor(grants []auth.Grant) string {
	if IsGlobalAdmin(grants) {
		return "admin"
	}
	tier := "read"
	for _, g := range grants {
		for _, a := range g.Actions {
			if a == auth.ActionWrite || a == auth.ActionDelete {
				tier = "write"
			}
		}
	}
	return tier
}
