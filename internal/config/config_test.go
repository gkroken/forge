package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"forge/internal/auth"
	"forge/internal/cleanup"
	"forge/internal/config"
	"forge/internal/ldap"
	"forge/internal/meta"
	"forge/internal/repo"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

func newAppliers(t *testing.T) config.Appliers {
	t.Helper()
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoMgr := repo.NewManager()
	if err := repoMgr.WithStore(m); err != nil {
		t.Fatal(err)
	}
	return config.Appliers{
		Repos:    repoMgr,
		Cleanup:  cleanup.NewPolicyManager(m),
		Vuln:     vuln.NewPolicyManager(m),
		Roles:    auth.NewRoleStore(m),
		Webhooks: webhook.NewStore(m),
		Meta:     m,
	}
}

func TestApply_LDAPSection(t *testing.T) {
	a := newAppliers(t)

	// Valid ldap block → applied cleanly, LDAPConfigured reported.
	valid := config.File{LDAP: &ldap.Config{
		URLs:       []string{"ldaps://dc1:636"},
		UserBaseDN: "ou=people,dc=example,dc=com",
	}}
	res, err := config.Apply(valid, a)
	if err != nil {
		t.Fatalf("valid ldap block rejected: %v", err)
	}
	if !res.LDAPConfigured {
		t.Error("expected LDAPConfigured=true")
	}

	// Invalid ldap block (search mode without group base DN) → validation error.
	invalid := config.File{LDAP: &ldap.Config{
		URLs:       []string{"ldaps://dc1:636"},
		UserBaseDN: "ou=people,dc=example,dc=com",
		GroupMode:  "search",
	}}
	if _, err := config.Apply(invalid, a); err == nil {
		t.Fatal("expected invalid ldap block to be rejected")
	}

	// No ldap block → LDAPConfigured=false.
	res, err = config.Apply(config.File{}, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.LDAPConfigured {
		t.Error("expected LDAPConfigured=false when no ldap block")
	}
}

func TestLoad_EnvExpand(t *testing.T) {
	t.Setenv("TEST_UPSTREAM", "https://example.com")
	f := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(f, []byte(`{
		"repositories": [{"name":"r","format":"npm","kind":"hosted","enabled":true}],
		"webhooks": [{"name":"h","url":"${TEST_UPSTREAM}/hook","enabled":true}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repositories) != 1 {
		t.Fatalf("want 1 repo, got %d", len(got.Repositories))
	}
	if got.Webhooks[0].URL != "https://example.com/hook" {
		t.Errorf("env expand: got URL %q", got.Webhooks[0].URL)
	}
}

func TestLoad_UndefinedEnvVar(t *testing.T) {
	os.Unsetenv("FORGE_TEST_UNDEFINED_XYZ")
	f := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(f, []byte(`{"webhooks":[{"url":"${FORGE_TEST_UNDEFINED_XYZ}"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(f); err == nil {
		t.Fatal("expected error for undefined env var")
	}
}

func TestApply_CreateUpdateNoop(t *testing.T) {
	a := newAppliers(t)
	f := config.File{
		Repositories: []repo.Repository{
			{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true},
		},
		CleanupPolicies: []cleanup.NamedPolicy{
			{Name: "keep-10", KeepVersions: 10},
		},
	}

	// First apply: all creates.
	res, err := config.Apply(f, a)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res.Repositories.Created != 1 || res.CleanupPolicies.Created != 1 {
		t.Errorf("first apply: repos=%+v cleanup=%+v", res.Repositories, res.CleanupPolicies)
	}

	// Second apply: idempotent noop.
	res, err = config.Apply(f, a)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Repositories.Noop != 1 || res.CleanupPolicies.Noop != 1 {
		t.Errorf("second apply (noop): repos=%+v cleanup=%+v", res.Repositories, res.CleanupPolicies)
	}

	// Modify and apply: should update.
	f.Repositories[0].AnonymousRead = true
	res, err = config.Apply(f, a)
	if err != nil {
		t.Fatalf("update apply: %v", err)
	}
	if res.Repositories.Updated != 1 {
		t.Errorf("update: want 1 updated, got %+v", res.Repositories)
	}
}

func TestApply_WebhookBlankSecretIsNoop(t *testing.T) {
	a := newAppliers(t)
	f := config.File{
		Webhooks: []webhook.Subscription{
			{Name: "hook", URL: "https://example.com/hook", Enabled: true, Secret: "original"},
		},
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}
	// Apply again with blank secret (export-style); must be noop, not update.
	f.Webhooks[0].Secret = ""
	res, err := config.Apply(f, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Webhooks.Updated != 0 {
		t.Errorf("blank secret should be noop, got %+v", res.Webhooks)
	}
}

func TestApply_Prune(t *testing.T) {
	a := newAppliers(t)
	f := config.File{
		Prune: true,
		Repositories: []repo.Repository{
			{Name: "a", Format: "npm", Kind: repo.Hosted, Enabled: true},
			{Name: "b", Format: "helm", Kind: repo.Hosted, Enabled: true},
		},
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}

	// Remove "b" and re-apply with Prune.
	f.Repositories = f.Repositories[:1]
	res, err := config.Apply(f, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Repositories.Deleted != 1 {
		t.Errorf("prune: want 1 deleted, got %+v", res.Repositories)
	}
	if _, ok := a.Repos.Get("a"); !ok {
		t.Error("prune: deleted 'a' but it should be kept")
	}
	if _, ok := a.Repos.Get("b"); ok {
		t.Error("prune: 'b' still exists after prune")
	}
}

func TestApply_PruneSpareUnmanaged(t *testing.T) {
	a := newAppliers(t)
	// Simulate a repo created via REST/UI (not by config).
	if err := a.Repos.Add(repo.Repository{Name: "ui-created", Format: "npm", Kind: repo.Hosted, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Apply a config with Prune=true and a different repo.
	f := config.File{
		Prune: true,
		Repositories: []repo.Repository{
			{Name: "config-managed", Format: "npm", Kind: repo.Hosted, Enabled: true},
		},
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}
	// "ui-created" must NOT be pruned.
	if _, ok := a.Repos.Get("ui-created"); !ok {
		t.Error("prune deleted a UI-created repo — that is wrong")
	}
}

func TestApply_SecurityDefault(t *testing.T) {
	a := newAppliers(t)
	f := config.File{
		SecurityDefault: &vuln.Policy{Mode: vuln.ModeWarn, Threshold: vuln.SeverityHigh, FailOpen: true},
	}
	res, err := config.Apply(f, a)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SecurityDefaultSet {
		t.Error("SecurityDefaultSet should be true")
	}
	got, err := a.Vuln.Default()
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != vuln.ModeWarn {
		t.Errorf("security default mode: got %q", got.Mode)
	}
}

func TestPlan_ValidationErrors(t *testing.T) {
	a := newAppliers(t)

	t.Run("missing group member", func(t *testing.T) {
		f := config.File{
			Repositories: []repo.Repository{
				{Name: "g", Format: "npm", Kind: repo.Group, Members: []string{"nonexistent"}},
			},
		}
		if _, err := config.Plan(f, a); err == nil {
			t.Error("expected validation error for missing group member")
		}
	})

	t.Run("missing cleanup policy ref", func(t *testing.T) {
		f := config.File{
			Repositories: []repo.Repository{
				{Name: "r", Format: "npm", Kind: repo.Hosted, Enabled: true, CleanupPolicyName: "no-such-policy"},
			},
		}
		if _, err := config.Plan(f, a); err == nil {
			t.Error("expected validation error for missing cleanup policy")
		}
	})

	t.Run("missing security policy ref", func(t *testing.T) {
		f := config.File{
			Repositories: []repo.Repository{
				{Name: "r", Format: "npm", Kind: repo.Hosted, Enabled: true, SecurityPolicyName: "no-such-policy"},
			},
		}
		if _, err := config.Plan(f, a); err == nil {
			t.Error("expected validation error for missing security policy")
		}
	})

	t.Run("cross-ref resolved within file", func(t *testing.T) {
		f := config.File{
			CleanupPolicies: []cleanup.NamedPolicy{{Name: "p", KeepVersions: 3}},
			Repositories: []repo.Repository{
				{Name: "r", Format: "npm", Kind: repo.Hosted, Enabled: true, CleanupPolicyName: "p"},
			},
		}
		if _, err := config.Plan(f, a); err != nil {
			t.Errorf("policy in file should resolve: %v", err)
		}
	})
}

func TestExport_RoundTrip(t *testing.T) {
	a := newAppliers(t)
	f := config.File{
		Repositories:    []repo.Repository{{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true}},
		CleanupPolicies: []cleanup.NamedPolicy{{Name: "keep-5", KeepVersions: 5}},
		Roles:           []auth.CustomRole{{Name: "ci", BaseRole: "write"}},
		Webhooks: []webhook.Subscription{
			{Name: "my-hook", URL: "https://example.com/hook", Enabled: true, Secret: "s3cr3t"},
		},
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}

	exported, err := config.Export(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range exported.Webhooks {
		if s.Secret != "" {
			t.Errorf("export: webhook secret not blanked, got %q", s.Secret)
		}
	}
	for _, r := range exported.Repositories {
		if r.ProxyAuth != "" {
			t.Errorf("export: proxyAuth not blanked for %q", r.Name)
		}
	}

	// Write exported JSON and reload.
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	tf := filepath.Join(t.TempDir(), "exported.json")
	if err := os.WriteFile(tf, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(tf)
	if err != nil {
		t.Fatal(err)
	}

	// Second apply should be noop for non-secret fields.
	res, err := config.Apply(reloaded, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Repositories.Changes()+res.CleanupPolicies.Changes()+res.Roles.Changes() != 0 {
		t.Errorf("export round-trip: expected noop, got repos=%+v cleanup=%+v roles=%+v",
			res.Repositories, res.CleanupPolicies, res.Roles)
	}
}

// --- YAML support (C1) ---------------------------------------------------

// TestIsYAMLPath covers extension detection, including case-insensitivity.
func TestIsYAMLPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"forge.config.yaml", true},
		{"forge.config.yml", true},
		{"/etc/forge/CONFIG.YAML", true},
		{"forge.config.json", false},
		{"config", false},
		{"a.yaml.json", false},
	} {
		if got := config.IsYAMLPath(tc.path); got != tc.want {
			t.Errorf("config.IsYAMLPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestLoad_YAMLEquivalentToJSON is the core guarantee of C1: the same logical
// config authored as YAML and as JSON must produce an identical File.
func TestLoad_YAMLEquivalentToJSON(t *testing.T) {
	dir := t.TempDir()

	jsonSrc := `{
	  "prune": true,
	  "repositories": [
	    {"name":"maven-central","format":"maven","kind":"proxy",
	     "upstream":"https://repo1.maven.org/maven2","anonymousRead":true,"enabled":true}
	  ],
	  "roles": [
	    {"name":"backend","grants":[{"repo":"maven-*","actions":["read","write"]}]}
	  ],
	  "webhooks": [{"name":"slack","url":"https://example.com/hook"}]
	}`
	yamlSrc := `
prune: true
repositories:
  - name: maven-central
    format: maven
    kind: proxy
    upstream: https://repo1.maven.org/maven2
    anonymousRead: true
    enabled: true
roles:
  - name: backend
    grants:
      - repo: "maven-*"
        actions: [read, write]
webhooks:
  - name: slack
    url: https://example.com/hook
`
	jp := filepath.Join(dir, "cfg.json")
	yp := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(jp, []byte(jsonSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yp, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	fj, err := config.Load(jp)
	if err != nil {
		t.Fatalf("load json: %v", err)
	}
	fy, err := config.Load(yp)
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}
	if !sameJSON(fj, fy) {
		gj, _ := json.Marshal(fj)
		gy, _ := json.Marshal(fy)
		t.Fatalf("YAML and JSON disagree:\n json=%s\n yaml=%s", gj, gy)
	}
}

// TestLoad_JSONFileStillParsesAsJSON guards backward compatibility: existing
// .json configs must keep working untouched.
func TestLoad_JSONFileStillParsesAsJSON(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(f, []byte(`{"repositories":[{"name":"r","format":"npm"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(f)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Repositories) != 1 || got.Repositories[0].Name != "r" {
		t.Fatalf("got %+v", got)
	}
}

// TestLoad_YAMLEnvExpand verifies ${VAR} indirection is format-agnostic —
// expansion runs on raw text before parsing.
func TestLoad_YAMLEnvExpand(t *testing.T) {
	t.Setenv("TEST_YAML_UPSTREAM", "https://example.com")
	t.Setenv("TEST_YAML_SECRET", "s3cr3t")
	f := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(f, []byte(`
repositories:
  - name: proxy
    format: npm
    kind: proxy
    upstream: ${TEST_YAML_UPSTREAM}
webhooks:
  - name: hook
    url: https://example.com/h
    secret: ${TEST_YAML_SECRET}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(f)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Repositories[0].Upstream != "https://example.com" {
		t.Errorf("upstream = %q", got.Repositories[0].Upstream)
	}
	if got.Webhooks[0].Secret != "s3cr3t" {
		t.Errorf("secret = %q", got.Webhooks[0].Secret)
	}
}

// TestLoad_YAMLUndefinedEnvVar — an unset placeholder must fail loudly in YAML
// exactly as it does in JSON, never silently blank.
func TestLoad_YAMLUndefinedEnvVar(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(f, []byte("webhooks:\n  - url: ${FORGE_TEST_UNDEFINED_YAML_XYZ}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(f); err == nil {
		t.Fatal("expected error for undefined env var, got nil")
	}
}

// TestLoad_MalformedYAML reports a parse error rather than silently producing
// an empty File (which Apply would treat as "delete everything" under prune).
func TestLoad_MalformedYAML(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(f, []byte("repositories:\n  - name: a\n   format: bad-indent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(f); err == nil {
		t.Fatal("expected parse error for malformed YAML, got nil")
	}
}

// TestMarshal_RoundTrip covers both export formats surviving a re-parse.
func TestMarshal_RoundTrip(t *testing.T) {
	orig := config.File{
		Prune:        true,
		Repositories: []repo.Repository{{Name: "r", Format: "maven", Kind: repo.Hosted, Enabled: true}},
	}
	for _, asYAML := range []bool{false, true} {
		out, err := config.Marshal(orig, asYAML)
		if err != nil {
			t.Fatalf("marshal(yaml=%v): %v", asYAML, err)
		}
		got, err := config.Unmarshal(out, asYAML)
		if err != nil {
			t.Fatalf("unmarshal(yaml=%v): %v", asYAML, err)
		}
		if !sameJSON(orig, got) {
			t.Errorf("yaml=%v round-trip mismatch:\n want %+v\n got  %+v", asYAML, orig, got)
		}
	}
}

// sameJSON compares two values by their JSON encoding, matching how the config
// package itself decides equality.
func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}
