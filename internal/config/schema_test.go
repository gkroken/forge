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
