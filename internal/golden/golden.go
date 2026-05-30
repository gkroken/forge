// Package golden provides golden-file test helpers.
//
// Golden files live in testdata/ relative to the test's package directory.
// Run any test with -update to regenerate them:
//
//	go test ./internal/format/maven/ -update
package golden

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "update golden files in testdata/")

// Assert compares got against testdata/<name>. Fails with a clear diff on
// mismatch. Run with -update to write/refresh the golden file.
func Assert(t *testing.T, got []byte, name string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("golden: mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("golden: write %s: %v", path, err)
		}
		t.Logf("golden: updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden: read %s (run with -update to create): %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden: mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}
