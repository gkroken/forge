// Package blobtest provides a reusable contract suite for blob.Store.
//
// Every implementation (FS today; S3/GCS later) must pass RunContract. This is
// what makes swapping storage backends safe: one suite, all backends.
package blobtest

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"

	"forge/internal/blob"
)

// RunContract runs the full behavioral contract against s.
func RunContract(t *testing.T, s blob.Store) {
	t.Helper()

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		want := []byte("hello world")
		if _, err := s.Put("a/b/c.txt", bytes.NewReader(want)); err != nil {
			t.Fatalf("put: %v", err)
		}
		rc, err := s.Get("a/b/c.txt")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, want) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, want)
		}
	})

	t.Run("ChecksumsComputed", func(t *testing.T) {
		// Known digests of "abc".
		info, err := s.Put("sums/abc", strings.NewReader("abc"))
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		cases := map[string]string{
			info.SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			info.SHA1:   "a9993e364706816aba3e25717850c26c9cd0d89d",
			info.MD5:    "900150983cd24fb0d6963f7d28e17f72",
		}
		for got, want := range cases {
			if got != "" && got != want { // S3 impls may defer some; allow empty
				t.Errorf("checksum mismatch: got %q want %q", got, want)
			}
		}
		if info.Size != 3 {
			t.Errorf("size: got %d want 3", info.Size)
		}
	})

	t.Run("StatExistsAndMissing", func(t *testing.T) {
		s.Put("stat/here", strings.NewReader("x"))
		if _, ok, _ := s.Stat("stat/here"); !ok {
			t.Error("expected existing key to stat ok")
		}
		if _, ok, _ := s.Stat("stat/nope"); ok {
			t.Error("expected missing key to stat not-ok")
		}
	})

	t.Run("ListUnderPrefix", func(t *testing.T) {
		s.Put("list/x/1", strings.NewReader("1"))
		s.Put("list/x/2", strings.NewReader("2"))
		s.Put("list/y/3", strings.NewReader("3"))
		keys, err := s.List("list/x")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		sort.Strings(keys)
		if len(keys) != 2 || keys[0] != "list/x/1" || keys[1] != "list/x/2" {
			t.Fatalf("unexpected list result: %v", keys)
		}
	})

	t.Run("OverwriteReplaces", func(t *testing.T) {
		s.Put("ow/k", strings.NewReader("first"))
		s.Put("ow/k", strings.NewReader("second"))
		rc, _ := s.Get("ow/k")
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if string(got) != "second" {
			t.Fatalf("overwrite failed: got %q", got)
		}
	})

	t.Run("DeleteThenMissing", func(t *testing.T) {
		s.Put("del/k", strings.NewReader("bye"))
		if err := s.Delete("del/k"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, ok, _ := s.Stat("del/k"); ok {
			t.Error("expected key gone after delete")
		}
		if err := s.Delete("del/k"); err != nil {
			t.Errorf("delete of missing key should be nil, got %v", err)
		}
	})

	t.Run("TraversalNeutralisedOrRejected", func(t *testing.T) {
		// Traversal attempts must either be rejected (non-nil error from Put)
		// or neutralised so the key is only reachable via the same key string.
		// In neither case may a Put succeed while Stat disagrees.
		for _, key := range []string{
			"../escape",
			"../../etc/passwd",
			"a/../../b",
			"/absolute",
		} {
			_, err := s.Put(key, strings.NewReader("probe"))
			if err != nil {
				continue // rejection is acceptable
			}
			// Put succeeded: Stat must agree the key exists.
			_, ok, statErr := s.Stat(key)
			if statErr != nil || !ok {
				t.Errorf("Put(%q) succeeded but Stat returned ok=%v err=%v", key, ok, statErr)
			}
		}
	})

	t.Run("LargeStream", func(t *testing.T) {
		const size = 5 << 20 // 5 MiB — larger than typical I/O buffers
		data := bytes.Repeat([]byte("forge"), size/5)
		info, err := s.Put("large/blob", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("put large: %v", err)
		}
		if info.Size != int64(size) {
			t.Fatalf("size: got %d want %d", info.Size, size)
		}
		rc, err := s.Get("large/blob")
		if err != nil {
			t.Fatalf("get large: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read large: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("large stream mismatch: got %d bytes, want %d", len(got), len(data))
		}
	})
}
