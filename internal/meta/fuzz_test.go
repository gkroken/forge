package meta_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/meta"
)

// FuzzMetaFSNS drives the FS store with arbitrary namespace strings.
//
// Invariant: meta.FS.resolve rejects any ns that would escape the root, so
// PutJSON must return a non-nil error for traversal namespaces. When PutJSON
// succeeds, no file may exist in the store's parent directory outside the root.
func FuzzMetaFSNS(f *testing.F) {
	seeds := []string{
		"../evil",
		"../../etc",
		"ns/../../escape",
		"npm-packuments",
		"auth:tokens",
		"",
		"/absolute",
		strings.Repeat("../", 10) + "x",
		"a\x00b",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ns string) {
		// Use an isolated parent so any escape into the parent is detectable.
		parent, err := os.MkdirTemp("", "forge-fuzz-meta-ns-*")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(parent)

		root := filepath.Join(parent, "store")
		store, err := meta.NewFS(root)
		if err != nil {
			t.Fatal(err)
		}

		putErr := store.PutJSON(ns, "key", "value")
		if putErr != nil {
			return // rejection is correct for traversal inputs
		}

		// PutJSON succeeded: verify no file escaped the store root.
		escaped := false
		filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasPrefix(p, root) {
				escaped = true
			}
			return nil
		})
		if escaped {
			t.Fatalf("PutJSON(ns=%q) wrote a file outside store root %q", ns, root)
		}

		// Consistency: the written record must be readable back.
		var got string
		ok, getErr := store.GetJSON(ns, "key", &got)
		if getErr != nil || !ok {
			t.Fatalf("PutJSON(ns=%q) succeeded but GetJSON returned ok=%v err=%v", ns, ok, getErr)
		}
	})
}

// FuzzMetaFSKey drives the FS store with arbitrary key strings.
//
// Keys have their "/" replaced with "__" before joining, so traversal via key
// is already neutralised. The invariant here is no panic + consistency.
func FuzzMetaFSKey(f *testing.F) {
	seeds := []string{
		"../escape",
		"../../etc/passwd",
		"@scope/pkg",
		"pkg/1.0.0",
		"",
		strings.Repeat("a/", 50),
		"\x00null",
		"a\nb",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, key string) {
		store, err := meta.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		putErr := store.PutJSON("valid-ns", key, "value")
		if putErr != nil {
			return // error is fine
		}

		var got string
		ok, getErr := store.GetJSON("valid-ns", key, &got)
		if getErr != nil || !ok {
			t.Fatalf("PutJSON(key=%q) succeeded but GetJSON returned ok=%v err=%v", key, ok, getErr)
		}
	})
}
