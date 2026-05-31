package blob_test

import (
	"strings"
	"testing"

	"forge/internal/blob"
)

// FuzzBlobFSKey drives the FS store with arbitrary key strings.
//
// Invariant: blob.FS resolves every key to a path under the store root
// (traversal attempts are neutralised by prepending "/" before filepath.Clean,
// not rejected). So a Put that returns nil must leave the blob reachable via
// Stat with the same key.
func FuzzBlobFSKey(f *testing.F) {
	seeds := []string{
		"../escape",
		"../../etc/passwd",
		"a/../../b",
		"repo/../../../outside",
		"valid/key.jar",
		"com/acme/lib/1.0/lib-1.0.jar",
		"",
		"/absolute",
		strings.Repeat("a/", 50) + "x",
		"\x00null",
		"a\nb",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, key string) {
		store, err := blob.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		_, putErr := store.Put(key, strings.NewReader("probe"))
		if putErr != nil {
			return // rejection is fine; no further checks needed
		}

		// Put succeeded: Stat must agree the key exists.
		_, ok, statErr := store.Stat(key)
		if statErr != nil || !ok {
			t.Fatalf("Put(%q) returned nil but Stat returned ok=%v err=%v", key, ok, statErr)
		}
	})
}
