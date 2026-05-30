package blob_test

import (
	"os"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/blob/blobtest"
)

// The FS store must satisfy the same contract every backend will.
func TestFS_Contract(t *testing.T) {
	s, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobtest.RunContract(t, s)
}

// FS-specific: traversal keys must be contained within root, never escape it.
func TestFS_TraversalContained(t *testing.T) {
	root := t.TempDir()
	s, _ := blob.NewFS(root)
	// Attempt to escape; must not write outside root.
	if _, err := s.Put("../../../tmp/forge-escape-probe", strings.NewReader("x")); err != nil {
		// erroring is also acceptable containment
		return
	}
	if _, err := os.Stat("/tmp/forge-escape-probe"); err == nil {
		os.Remove("/tmp/forge-escape-probe")
		t.Fatal("traversal escaped storage root")
	}
}
