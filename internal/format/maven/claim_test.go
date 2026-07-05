package maven

import (
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/repo"
)

func TestClaimPath(t *testing.T) {
	h := New()
	tests := []struct {
		sub  string
		comp string
		ok   bool
	}{
		{"com/acme/app/1.0.0/app-1.0.0.jar", "com/acme/app", true},
		{"com/acme/app/1.0.0/app-1.0.0.pom", "com/acme/app", true},
		{"com/acme/app/1.0.0/app-1.0.0.jar.sha1", "com/acme/app", true},
		{"com/acme/app/maven-metadata.xml", "com/acme/app", true},
		{"com/acme/app/maven-metadata.xml.md5", "com/acme/app", true},
		{"com/acme/app/1.0-SNAPSHOT/maven-metadata.xml", "com/acme/app", true},
		{"org/example/deep/group/lib/2.1/lib-2.1.war", "org/example/deep/group/lib", true},
		// not claim targets
		{"com/acme", "", false},                  // no version segment
		{"app/1.0/app-1.0.jar", "", false},       // version too shallow (no groupId)
		{"maven-metadata.xml", "", false},        // bare metadata at root
		{"com/maven-metadata.xml", "", false},    // metadata with only one parent segment
	}
	for _, tc := range tests {
		comp, ok := h.ClaimPath(tc.sub)
		if ok != tc.ok || comp != tc.comp {
			t.Errorf("ClaimPath(%q) = (%q,%v) want (%q,%v)", tc.sub, comp, ok, tc.comp, tc.ok)
		}
	}
}

func TestOwnsComponent(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	c := &format.Context{
		Repo: repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
		Blob: b,
	}
	b.Put("maven-hosted/com/acme/app/1.0/app-1.0.jar", strings.NewReader("jar"))

	h := New()
	if !h.OwnsComponent(c, "com/acme/app") {
		t.Error("expected ownership of com/acme/app")
	}
	if h.OwnsComponent(c, "com/acme/app-extra") {
		t.Error("sibling artifact app-extra must not be owned")
	}
	if h.OwnsComponent(c, "com/acme/ap") {
		t.Error("prefix com/acme/ap must not be owned")
	}
	if h.OwnsComponent(c, "org/other/lib") {
		t.Error("unpublished artifact must not be owned")
	}
}
