package blob

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestImmutable_RejectsOverwrite(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	im := Immutable(fs)

	// First write of a fresh key succeeds.
	if _, err := im.Put("repo/a/1.0/a.jar", strings.NewReader("v1")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	// Overwriting the same key is refused with ErrImmutable...
	if _, err := im.Put("repo/a/1.0/a.jar", strings.NewReader("v2")); !errors.Is(err, ErrImmutable) {
		t.Fatalf("overwrite: want ErrImmutable, got %v", err)
	}
	// ...and the original bytes are intact (the overwrite never happened).
	rc, err := im.Get("repo/a/1.0/a.jar")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v1" {
		t.Fatalf("bytes changed: want v1, got %q", got)
	}
	// A different (new) key still writes normally.
	if _, err := im.Put("repo/a/1.1/a.jar", strings.NewReader("v3")); err != nil {
		t.Fatalf("new version put: %v", err)
	}
	// Delete must be refused too. This assertion previously read the other way,
	// on the reasoning that "the server blocks delete on immutable repos" — but
	// that was only true of the admin API. A protocol-level DELETE reached the
	// store and succeeded, and the coordinate could then be re-published with
	// different bytes, so the test was locking in the bypass as intended
	// behaviour.
	if err := im.Delete("repo/a/1.0/a.jar"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("delete of an existing artifact = %v, want ErrImmutable — "+
			"delete-then-republish is the mutation this wrapper exists to prevent", err)
	}
	rc, err = im.Get("repo/a/1.0/a.jar")
	if err != nil {
		t.Fatalf("artifact removed despite the refusal: %v", err)
	}
	defer rc.Close()
	got, _ = io.ReadAll(rc)
	if string(got) != "v1" {
		t.Fatalf("bytes changed after a refused delete: %q", got)
	}
	// Deleting a key that does not exist is not an overwrite, so it passes through.
	if err := im.Delete("repo/a/9.9/missing.jar"); err != nil {
		t.Fatalf("delete of an absent key: %v", err)
	}
}

// TestImmutable_DeleteThenRepublishIsRefused states the guarantee end to end:
// an immutable coordinate cannot be made to hold different bytes by any route.
func TestImmutable_DeleteThenRepublishIsRefused(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	im := Immutable(fs)
	if _, err := im.Put("repo/a/1.0/a.jar", strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	if err := im.Delete("repo/a/1.0/a.jar"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("delete = %v, want ErrImmutable", err)
	}
	if _, err := im.Put("repo/a/1.0/a.jar", strings.NewReader("mutated")); !errors.Is(err, ErrImmutable) {
		t.Fatalf("re-put = %v, want ErrImmutable", err)
	}
	rc, err := im.Get("repo/a/1.0/a.jar")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "original" {
		t.Errorf("bytes are %q, want the original — immutability was bypassed", got)
	}
}
