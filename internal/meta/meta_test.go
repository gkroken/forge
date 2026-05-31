package meta_test

import (
	"testing"

	"forge/internal/meta"
	"forge/internal/meta/metatest"
)

// The FS store must satisfy the same contract every backend will.
func TestFS_Contract(t *testing.T) {
	s, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metatest.RunContract(t, s)
}

// TestFS_Traversal verifies that crafted ns/key values cannot escape the root.
func TestFS_Traversal(t *testing.T) {
	s, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	traversalNS := []string{
		"../evil",
		"../../etc",
		"ns/../../escape",
	}

	for _, ns := range traversalNS {
		t.Run("GetJSON ns="+ns, func(t *testing.T) {
			var v any
			_, err := s.GetJSON(ns, "key", &v)
			if err == nil {
				t.Errorf("GetJSON(%q, %q): expected traversal error, got nil", ns, "key")
			}
		})
		t.Run("PutJSON ns="+ns, func(t *testing.T) {
			err := s.PutJSON(ns, "key", "value")
			if err == nil {
				t.Errorf("PutJSON(%q, %q): expected traversal error, got nil", ns, "key")
			}
		})
		t.Run("List ns="+ns, func(t *testing.T) {
			_, err := s.List(ns)
			if err == nil {
				t.Errorf("List(%q): expected traversal error, got nil", ns)
			}
		})
		t.Run("Delete ns="+ns, func(t *testing.T) {
			err := s.Delete(ns, "key")
			if err == nil {
				t.Errorf("Delete(%q, %q): expected traversal error, got nil", ns, "key")
			}
		})
	}
}
