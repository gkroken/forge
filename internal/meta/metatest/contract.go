// Package metatest provides a reusable contract suite for meta.Store.
//
// Every implementation (FS today; Postgres later) must pass RunContract. This
// is what makes swapping metadata backends safe: one suite, all backends.
package metatest

import (
	"sort"
	"testing"

	"forge/internal/meta"
)

type record struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// RunContract runs the full behavioral contract against s.
func RunContract(t *testing.T, s meta.Store) {
	t.Helper()

	t.Run("PutGetRoundTrip", func(t *testing.T) {
		if err := s.PutJSON("ns", "key1", record{"alice", 42}); err != nil {
			t.Fatalf("put: %v", err)
		}
		var got record
		ok, err := s.GetJSON("ns", "key1", &got)
		if err != nil {
			t.Fatalf("get err: %v", err)
		}
		if !ok {
			t.Fatal("expected ok=true for existing key")
		}
		if got.Name != "alice" || got.N != 42 {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("MissingReturnsFalse", func(t *testing.T) {
		var got record
		ok, err := s.GetJSON("ns", "does-not-exist", &got)
		if err != nil {
			t.Fatalf("expected nil err for missing key, got %v", err)
		}
		if ok {
			t.Fatal("expected ok=false for missing key")
		}
	})

	t.Run("OverwriteReplaces", func(t *testing.T) {
		s.PutJSON("ow", "k", record{"first", 1})
		s.PutJSON("ow", "k", record{"second", 2})
		var got record
		ok, err := s.GetJSON("ow", "k", &got)
		if err != nil || !ok {
			t.Fatalf("get after overwrite: ok=%v err=%v", ok, err)
		}
		if got.Name != "second" {
			t.Fatalf("overwrite did not replace: %+v", got)
		}
	})

	t.Run("KeyWithSlashRoundTrips", func(t *testing.T) {
		if err := s.PutJSON("slash", "a/b/c", record{"slash", 7}); err != nil {
			t.Fatalf("put: %v", err)
		}
		var got record
		ok, err := s.GetJSON("slash", "a/b/c", &got)
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		if got.Name != "slash" {
			t.Fatalf("slash key round-trip mismatch: %+v", got)
		}
	})

	t.Run("ListReturnsAllKeysInNamespace", func(t *testing.T) {
		s.PutJSON("list", "alpha", record{"a", 1})
		s.PutJSON("list", "beta", record{"b", 2})
		s.PutJSON("list", "scoped/gamma", record{"c", 3})
		keys, err := s.List("list")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		sort.Strings(keys)
		want := []string{"alpha", "beta", "scoped/gamma"}
		if len(keys) != len(want) {
			t.Fatalf("list len: got %v want %v", keys, want)
		}
		for i, k := range keys {
			if k != want[i] {
				t.Errorf("list[%d]: got %q want %q", i, k, want[i])
			}
		}
	})

	t.Run("ListEmptyNamespaceReturnsNil", func(t *testing.T) {
		keys, err := s.List("empty-ns-that-does-not-exist")
		if err != nil {
			t.Fatalf("list empty ns: %v", err)
		}
		if len(keys) != 0 {
			t.Fatalf("expected empty list, got %v", keys)
		}
	})

	t.Run("NamespaceIsolation", func(t *testing.T) {
		s.PutJSON("iso-a", "shared-key", record{"in-a", 1})
		s.PutJSON("iso-b", "shared-key", record{"in-b", 2})
		var got record
		s.GetJSON("iso-a", "shared-key", &got)
		if got.Name != "in-a" {
			t.Fatalf("namespace isolation broken: got %+v", got)
		}
	})

	t.Run("DeleteRemovesKey", func(t *testing.T) {
		s.PutJSON("del", "gone", record{"bye", 0})
		if err := s.Delete("del", "gone"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var got record
		ok, err := s.GetJSON("del", "gone", &got)
		if err != nil || ok {
			t.Fatalf("after delete: ok=%v err=%v", ok, err)
		}
	})

	t.Run("DeleteMissingKeyIsNil", func(t *testing.T) {
		if err := s.Delete("del", "never-existed"); err != nil {
			t.Fatalf("delete of missing key should return nil, got %v", err)
		}
	})
}
