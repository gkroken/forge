package nexus

import (
	"reflect"
	"testing"

	"forge/internal/auth"
	"forge/internal/repo"
	"forge/internal/selector"
)

func TestMapFormat(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"maven2", "maven", true},
		{"npm", "npm", true},
		{"helm", "helm", true},
		{"r", "cran", true},
		{"docker", "oci", true},
		{"Docker", "oci", true},
		{"pypi", "pypi", true},
		{"nuget", "", false},
		{"raw", "", false},
	}
	for _, c := range cases {
		got, ok := MapFormat(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("MapFormat(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMapRepo(t *testing.T) {
	existing := map[string]repo.Repository{
		"maven-releases": {Name: "maven-releases", Format: "maven", Kind: repo.Hosted},
		"clash":          {Name: "clash", Format: "npm", Kind: repo.Hosted},
	}

	t.Run("create hosted", func(t *testing.T) {
		p := MapRepo(Repository{Name: "libs", Format: "maven2", Type: "hosted"}, existing)
		if p.Action != "create" || p.TargetFormat != "maven" || p.TargetKind != repo.Hosted {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("exists is idempotent resume", func(t *testing.T) {
		p := MapRepo(Repository{Name: "maven-releases", Format: "maven2", Type: "hosted"}, existing)
		if p.Action != "exists" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("name conflict skips", func(t *testing.T) {
		p := MapRepo(Repository{Name: "clash", Format: "maven2", Type: "hosted"}, existing)
		if p.Action != "skip" || p.Reason == "" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("unsupported format skips", func(t *testing.T) {
		// nuget, not pypi: forge grew a PyPI format, and this case was the
		// only thing asserting the migration knew which formats exist.
		p := MapRepo(Repository{Name: "nu", Format: "nuget", Type: "hosted"}, existing)
		if p.Action != "skip" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("pypi maps like any other supported format", func(t *testing.T) {
		p := MapRepo(Repository{Name: "py", Format: "pypi", Type: "hosted"}, existing)
		if p.Action != "create" || p.TargetFormat != "pypi" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("proxy carries upstream", func(t *testing.T) {
		p := MapRepo(Repository{Name: "npm-proxy", Format: "npm", Type: "proxy", RemoteURL: "https://registry.npmjs.org"}, existing)
		if p.Action != "create" || p.Upstream != "https://registry.npmjs.org" {
			t.Fatalf("got %+v", p)
		}
		r := p.ToRepository()
		if r.Upstream != "https://registry.npmjs.org" || r.Kind != repo.Proxy || !r.Enabled {
			t.Fatalf("ToRepository: %+v", r)
		}
	})
	t.Run("proxy without remote URL skips", func(t *testing.T) {
		p := MapRepo(Repository{Name: "blind-proxy", Format: "npm", Type: "proxy"}, existing)
		if p.Action != "skip" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("group carries members", func(t *testing.T) {
		p := MapRepo(Repository{Name: "maven-all", Format: "maven2", Type: "group", Members: []string{"a", "b"}}, existing)
		if p.Action != "create" || len(p.Members) != 2 {
			t.Fatalf("got %+v", p)
		}
	})
}

func TestTranslateCSEL(t *testing.T) {
	cases := []struct {
		expr    string
		wantSel string
		wantFmt string
		ok      bool
	}{
		{`path =~ "^/org/acme/.*"`, "org/acme/**", "", true},
		{`path =~ "^/org/acme/"`, "org/acme/**", "", true},
		{`format == "maven2" and path =~ "^/com/example/.*"`, "com/example/**", "maven2", true},
		{`path == "/org/acme/thing.jar"`, "org/acme/thing.jar", "", true},
		// beyond the subset → refused, not silently approximated
		{`path =~ "^/(a|b)/.*"`, "", "", false},
		{`path =~ "^/org/.*/internal/.*"`, "", "", false},
		{`format == "maven2"`, "", "", false},                           // no path term
		{`path =~ "^/a/.*" or path =~ "^/b/.*"`, "", "", false},         // disjunction
		{`coordinate.groupId == "org.acme"`, "", "", false},             // coordinate terms
		{`path =~ "^/org/acme/.*" and path =~ "^/x/.*"`, "", "", false}, // two path terms
	}
	for _, c := range cases {
		sel, f, ok := TranslateCSEL(c.expr)
		if sel != c.wantSel || f != c.wantFmt || ok != c.ok {
			t.Errorf("TranslateCSEL(%q) = %q,%q,%v; want %q,%q,%v", c.expr, sel, f, ok, c.wantSel, c.wantFmt, c.ok)
		}
		if ok {
			if err := selector.Validate(sel); err != nil {
				t.Errorf("TranslateCSEL(%q) produced invalid forge selector %q: %v", c.expr, sel, err)
			}
		}
	}
}

func testPrivs() map[string]Privilege {
	return map[string]Privilege{
		"nx-all": {Name: "nx-all", Type: "wildcard"},
		"nx-repository-view-maven2-libs-read": {
			Name: "nx-repository-view-maven2-libs-read", Type: "repository-view",
			Format: "maven2", Repository: "libs", Actions: []string{"READ", "BROWSE"},
		},
		"nx-repository-view-maven2-libs-add": {
			Name: "nx-repository-view-maven2-libs-add", Type: "repository-view",
			Format: "maven2", Repository: "libs", Actions: []string{"ADD", "EDIT"},
		},
		"nx-repository-view-maven2-*-read": {
			Name: "nx-repository-view-maven2-*-read", Type: "repository-view",
			Format: "maven2", Repository: "*", Actions: []string{"READ"},
		},
		"nx-repository-view-*-*-*": {
			Name: "nx-repository-view-*-*-*", Type: "repository-view",
			Format: "*", Repository: "*", Actions: []string{"*"},
		},
		"nx-repository-admin-maven2-libs-*": {
			Name: "nx-repository-admin-maven2-libs-*", Type: "repository-admin",
			Format: "maven2", Repository: "libs", Actions: []string{"*"},
		},
		"acme-selector-priv": {
			Name: "acme-selector-priv", Type: "repository-content-selector",
			Repository: "libs", ContentSelector: "acme-only", Actions: []string{"READ", "ADD"},
		},
		"weird-selector-priv": {
			Name: "weird-selector-priv", Type: "repository-content-selector",
			Repository: "libs", ContentSelector: "complex", Actions: []string{"READ"},
		},
		"nx-settings-all": {Name: "nx-settings-all", Type: "application", Actions: []string{"*"}},
	}
}

func testSelectors() map[string]ContentSelector {
	return map[string]ContentSelector{
		"acme-only": {Name: "acme-only", Expression: `path =~ "^/com/acme/.*"`},
		"complex":   {Name: "complex", Expression: `coordinate.groupId == "com.acme"`},
	}
}

func reposOf(f string) []string {
	if f == "maven" {
		return []string{"libs", "maven-central"}
	}
	return nil
}

func TestBuildGrants_RepoScoped(t *testing.T) {
	grants, notes := BuildGrants(
		[]string{"nx-repository-view-maven2-libs-read", "nx-repository-view-maven2-libs-add"},
		testPrivs(), testSelectors(), reposOf)
	want := []auth.Grant{{Repo: "libs", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}}}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants = %+v; want %+v", grants, want)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %+v", notes)
	}
}

func TestBuildGrants_FormatStarExpands(t *testing.T) {
	grants, _ := BuildGrants([]string{"nx-repository-view-maven2-*-read"}, testPrivs(), testSelectors(), reposOf)
	want := []auth.Grant{
		{Repo: "libs", Actions: []auth.Action{auth.ActionRead}},
		{Repo: "maven-central", Actions: []auth.Action{auth.ActionRead}},
	}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants = %+v; want %+v", grants, want)
	}
}

func TestBuildGrants_GlobalStar(t *testing.T) {
	grants, _ := BuildGrants([]string{"nx-repository-view-*-*-*"}, testPrivs(), testSelectors(), reposOf)
	want := []auth.Grant{{Repo: "*", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete}}}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants = %+v; want %+v", grants, want)
	}
	if IsGlobalAdmin(grants) {
		t.Fatal("view-only star must not be global admin")
	}
}

func TestBuildGrants_Wildcard_IsGlobalAdmin(t *testing.T) {
	grants, _ := BuildGrants([]string{"nx-all"}, testPrivs(), testSelectors(), reposOf)
	if !IsGlobalAdmin(grants) {
		t.Fatalf("nx-all should map to global admin; got %+v", grants)
	}
	if BaseTierFor(grants) != "admin" {
		t.Fatalf("BaseTierFor = %q", BaseTierFor(grants))
	}
}

func TestBuildGrants_RepoAdmin(t *testing.T) {
	grants, _ := BuildGrants([]string{"nx-repository-admin-maven2-libs-*"}, testPrivs(), testSelectors(), reposOf)
	want := []auth.Grant{{Repo: "libs", Actions: []auth.Action{auth.ActionAdmin}}}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants = %+v; want %+v", grants, want)
	}
	// Repo-scoped admin must NOT summarise as the global-admin tier.
	if BaseTierFor(grants) == "admin" {
		t.Fatal("repo-scoped admin must not summarise as tier admin")
	}
}

func TestBuildGrants_ContentSelector(t *testing.T) {
	grants, notes := BuildGrants([]string{"acme-selector-priv"}, testPrivs(), testSelectors(), reposOf)
	want := []auth.Grant{{
		Repo: "libs", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite},
		Selectors: []string{"com/acme/**"},
	}}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants = %+v; want %+v", grants, want)
	}
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %+v", notes)
	}
	if err := auth.ValidateGrants(grants); err != nil {
		t.Fatalf("produced grants failed forge validation: %v", err)
	}
}

func TestBuildGrants_UntranslatableSelectorNoted(t *testing.T) {
	grants, notes := BuildGrants([]string{"weird-selector-priv"}, testPrivs(), testSelectors(), reposOf)
	if len(grants) != 0 {
		t.Fatalf("expected no grants, got %+v", grants)
	}
	if len(notes) != 1 || notes[0].Privilege != "weird-selector-priv" {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestBuildGrants_ApplicationPrivNoted(t *testing.T) {
	grants, notes := BuildGrants([]string{"nx-settings-all", "missing-priv"}, testPrivs(), testSelectors(), reposOf)
	if len(grants) != 0 {
		t.Fatalf("expected no grants, got %+v", grants)
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %+v", notes)
	}
}

func TestFlattenRole(t *testing.T) {
	roles := map[string]Role{
		"dev":    {ID: "dev", Privileges: []string{"p1"}, Roles: []string{"base"}},
		"base":   {ID: "base", Privileges: []string{"p2", "p1"}},
		"cycleA": {ID: "cycleA", Privileges: []string{"pa"}, Roles: []string{"cycleB"}},
		"cycleB": {ID: "cycleB", Privileges: []string{"pb"}, Roles: []string{"cycleA"}},
	}
	got := FlattenRole("dev", roles)
	if !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("FlattenRole(dev) = %v", got)
	}
	got = FlattenRole("cycleA", roles)
	if !reflect.DeepEqual(got, []string{"pa", "pb"}) {
		t.Fatalf("FlattenRole(cycleA) = %v (cycle not handled)", got)
	}
	if got := FlattenRole("ghost", roles); len(got) != 0 {
		t.Fatalf("FlattenRole(ghost) = %v", got)
	}
}
