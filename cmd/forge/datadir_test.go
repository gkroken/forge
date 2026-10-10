package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareDataDir_CreatesNested(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := prepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("write probe left behind: %v", entries)
	}
}

func TestPrepareDataDir_PermissionDeniedNamesUID(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	err := prepareDataDir(filepath.Join(parent, "data"))
	if err == nil {
		t.Fatal("want error on unwritable parent")
	}
	for _, want := range []string{"not writable by uid", "65532", parent} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestPrepareDataDir_ExistingButUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	// The real failure: /data exists (a root-owned tmpfs) so MkdirAll succeeds,
	// and only the first write inside it fails.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := prepareDataDir(dir)
	if err == nil || !strings.Contains(err.Error(), "not writable by uid") {
		t.Fatalf("want explained permission error, got %v", err)
	}
}
