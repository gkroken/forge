package blob_test

import (
	"os"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/blob/blobtest"
)

// TestChecksumHelpers verifies the package-level SHA256/SHA1/MD5 helpers
// against known test vectors. These functions are called by format handlers
// for sidecar synthesis and must be covered by blob's own test binary.
func TestChecksumHelpers(t *testing.T) {
	b := []byte("abc")
	cases := []struct{ fn, got, want string }{
		{"SHA256", blob.SHA256(b), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"SHA1", blob.SHA1(b), "a9993e364706816aba3e25717850c26c9cd0d89d"},
		{"MD5", blob.MD5(b), "900150983cd24fb0d6963f7d28e17f72"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s(%q) = %q, want %q", c.fn, b, c.got, c.want)
		}
	}
}

// TestNewFS_MkdirAll verifies that NewFS creates nested directories.
func TestNewFS_MkdirAll(t *testing.T) {
	root := t.TempDir() + "/a/b/c"
	s, err := blob.NewFS(root)
	if err != nil {
		t.Fatalf("NewFS nested: %v", err)
	}
	if _, err := s.Put("k", strings.NewReader("v")); err != nil {
		t.Fatalf("Put after nested NewFS: %v", err)
	}
}

// TestFS_GetMissing confirms that Get returns an error for an absent key.
func TestFS_GetMissing(t *testing.T) {
	s, _ := blob.NewFS(t.TempDir())
	if _, err := s.Get("no/such/key"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

// TestFS_DeleteIdempotent verifies Delete is a no-op for keys that don't exist.
func TestFS_DeleteIdempotent(t *testing.T) {
	s, _ := blob.NewFS(t.TempDir())
	if err := s.Delete("ghost/key"); err != nil {
		t.Fatalf("Delete missing key: %v", err)
	}
}

// TestFS_ListEmptyDir returns an empty slice (not an error) for an absent prefix.
func TestFS_ListEmpty(t *testing.T) {
	s, _ := blob.NewFS(t.TempDir())
	keys, err := s.List("no/prefix/here")
	if err != nil {
		t.Fatalf("List missing prefix: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected empty list, got %v", keys)
	}
}

// TestFS_TraversalRejected confirms that keys escaping the root are rejected
// (or silently contained) even on systems where the path resolves.
func TestFS_TraversalRejected(t *testing.T) {
	root := t.TempDir()
	s, _ := blob.NewFS(root)
	probe := root + "/../forge-escape-probe"
	_, _ = s.Put("../forge-escape-probe", strings.NewReader("x"))
	if _, err := os.Stat(probe); err == nil {
		os.Remove(probe)
		t.Fatal("traversal escaped storage root")
	}
}

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
