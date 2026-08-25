package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/config"
)

func loadErr(t *testing.T, name, body string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(p)
	return err
}

// TestCheckKeys_RejectsTypo is the point of the whole check: before this, a
// misspelled key was silently discarded and the config quietly did the wrong
// thing.
func TestCheckKeys_RejectsTypo(t *testing.T) {
	err := loadErr(t, "c.yaml", `
repositories:
  - name: npm-proxy
    format: npm
    kind: proxy
    anonymusRead: true
`)
	if err == nil {
		t.Fatal("expected an error for a misspelled field")
	}
	for _, want := range []string{"repositories[0].anonymusRead", "anonymousRead"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestCheckKeys_ReportsPathAndAllProblems — one run should surface every bad
// key, not just the first, so a config is fixed in one pass.
func TestCheckKeys_ReportsPathAndAllProblems(t *testing.T) {
	err := loadErr(t, "c.yaml", `
prune: true
madeUpTopLevel: 1
repositories:
  - name: a
    format: npm
    kind: hosted
    nope: 1
  - name: b
    format: npm
    kind: hosted
    alsoNope: 2
`)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"madeUpTopLevel", "repositories[0].nope", "repositories[1].alsoNope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

// TestCheckKeys_AcceptsWireOnlyKeys — durations are time.Duration in the struct
// (json:"-") but strings on the wire, handled by custom unmarshalers. They must
// not be reported as unknown.
func TestCheckKeys_AcceptsWireOnlyKeys(t *testing.T) {
	if err := loadErr(t, "c.yaml", `
repositories:
  - name: p
    format: npm
    kind: proxy
    upstream: https://registry.npmjs.org
    contentMaxAge: 10m
    metadataMaxAge: 5m
    proxyTTL: 1h
cleanupPolicies:
  - name: keep
    interval: 24h
roles:
  - name: devs
    grants:
      - repo: "*"
        role: write
`); err != nil {
		t.Fatalf("wire-only keys must be accepted: %v", err)
	}
}

// TestCheckKeys_AcceptsTheShippedExample guards against the checker drifting
// away from the example we tell people to copy.
func TestCheckKeys_AcceptsTheShippedExample(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("WEBHOOK_SECRET", "s3cr3t")
	t.Setenv("LDAP_BIND_PASSWORD", "pw")
	if _, err := config.Load(filepath.Join("..", "..", "deploy", "config", "forge.example.yaml")); err != nil {
		t.Fatalf("deploy/config/forge.example.yaml must load cleanly: %v", err)
	}
}

// TestCheckKeys_RejectsDuplicateYAMLKeys — a duplicate silently keeps the last
// value, which is exactly the class of bug this check exists to stop.
func TestCheckKeys_RejectsDuplicateYAMLKeys(t *testing.T) {
	err := loadErr(t, "c.yaml", `
prune: true
prune: false
`)
	if err == nil {
		t.Fatal("expected an error for a duplicate YAML key")
	}
}

// TestCheckKeys_JSONToo — strictness must not be YAML-only.
func TestCheckKeys_JSONToo(t *testing.T) {
	err := loadErr(t, "c.json", `{"repositories":[{"name":"a","format":"npm","kind":"hosted","bogus":1}]}`)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected the JSON path to reject unknown keys, got: %v", err)
	}
}

// TestCheckKeys_AllowsEmptyAndPartial — a partial file is valid and additive.
func TestCheckKeys_AllowsEmptyAndPartial(t *testing.T) {
	for name, body := range map[string]string{
		"empty.yaml":   "",
		"partial.yaml": "prune: true\n",
		"nulls.yaml":   "repositories:\ncleanupPolicies:\n",
	} {
		if err := loadErr(t, name, body); err != nil {
			t.Errorf("%s should load: %v", name, err)
		}
	}
}

// TestShippedExample_Applies goes further than checking the example parses:
// it applies it to a fresh store and asserts every declared object actually
// lands. A parse-only check would have missed a repository being dropped or
// renamed during an edit.
func TestShippedExample_Applies(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("WEBHOOK_SECRET", "s3cr3t")
	t.Setenv("LDAP_BIND_PASSWORD", "pw")

	f, err := config.Load(filepath.Join("..", "..", "deploy", "config", "forge.example.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a := newAppliers(t)
	res, err := config.Apply(f, a)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Everything in the file must be created, and nothing left over.
	if got, want := res.Repositories.Created, len(f.Repositories); got != want {
		t.Errorf("repositories created = %d, want %d", got, want)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("a fresh store must produce no conflicts, got %+v", res.Conflicts)
	}

	// Group members must resolve — a renamed or dropped member repo is exactly
	// the kind of edit that a load-only check waves through.
	for _, r := range f.Repositories {
		got, ok := a.Repos.Get(r.Name)
		if !ok {
			t.Errorf("repository %q was not created", r.Name)
			continue
		}
		for _, m := range got.Members {
			if _, ok := a.Repos.Get(m); !ok {
				t.Errorf("repository %q lists member %q, which does not exist", r.Name, m)
			}
		}
	}

	// Cross-references from repos to policies must resolve too.
	for _, r := range f.Repositories {
		if r.CleanupPolicyName != "" {
			if _, ok, _ := a.Cleanup.Get(r.CleanupPolicyName); !ok {
				t.Errorf("repository %q references missing cleanup policy %q", r.Name, r.CleanupPolicyName)
			}
		}
		if r.SecurityPolicyName != "" {
			if _, ok, _ := a.Vuln.Get(r.SecurityPolicyName); !ok {
				t.Errorf("repository %q references missing security policy %q", r.Name, r.SecurityPolicyName)
			}
		}
	}

	// Re-applying must be a no-op, or the example is not idempotent.
	res2, err := config.Apply(f, a)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res2.Repositories.Changes() != 0 || res2.CleanupPolicies.Changes() != 0 ||
		res2.SecurityPolicies.Changes() != 0 || res2.Webhooks.Changes() != 0 {
		t.Errorf("re-applying the example is not idempotent: %+v", res2)
	}
}
