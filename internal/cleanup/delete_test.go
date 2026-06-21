package cleanup_test

import (
	"strings"
	"testing"

	"forge/internal/cleanup"
)

func TestDeleteVersion_Maven(t *testing.T) {
	b, m := stores(t)
	// Two versions of com.example:app under the maven layout.
	putBlob(t, b, "maven-hosted/com/example/app/1.0.0/app-1.0.0.jar")
	putBlob(t, b, "maven-hosted/com/example/app/1.0.0/app-1.0.0.pom")
	putBlob(t, b, "maven-hosted/com/example/app/2.0.0/app-2.0.0.jar")

	res, err := cleanup.DeleteVersion("maven-hosted", "maven", "com.example:app", "1.0.0", b, m)
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if res.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (jar+pom)", res.Deleted)
	}
	// 1.0.0 gone, 2.0.0 retained.
	if _, ok, _ := b.Stat("maven-hosted/com/example/app/1.0.0/app-1.0.0.jar"); ok {
		t.Error("1.0.0 jar should be deleted")
	}
	if _, ok, _ := b.Stat("maven-hosted/com/example/app/2.0.0/app-2.0.0.jar"); !ok {
		t.Error("2.0.0 jar should be retained")
	}
}

func TestDeleteVersion_NPM(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/leftpad/-/leftpad-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "leftpad:1.0.0", map[string]string{"name": "leftpad", "version": "1.0.0"}); err != nil {
		t.Fatal(err)
	}

	res, err := cleanup.DeleteVersion("npm-hosted", "npm", "leftpad", "1.0.0", b, m)
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if _, ok, _ := b.Stat("npm-hosted/leftpad/-/leftpad-1.0.0.tgz"); ok {
		t.Error("tarball should be deleted")
	}
}

func TestDeleteVersion_NotFound(t *testing.T) {
	b, m := stores(t)
	_, err := cleanup.DeleteVersion("maven-hosted", "maven", "com.example:app", "9.9.9", b, m)
	if err == nil {
		t.Fatal("expected error for missing version")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err)
	}
}

func TestDeleteVersion_Validation(t *testing.T) {
	b, m := stores(t)
	if _, err := cleanup.DeleteVersion("r", "maven", "", "1.0.0", b, m); err == nil {
		t.Error("expected error for empty component")
	}
	if _, err := cleanup.DeleteVersion("r", "weird", "x", "1.0.0", b, m); err == nil {
		t.Error("expected error for unsupported format")
	}
}
