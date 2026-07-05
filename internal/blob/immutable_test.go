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
	// Delete then re-put is allowed at the store level (the server blocks
	// delete on immutable repos; the wrapper only guards live overwrites).
	if err := im.Delete("repo/a/1.0/a.jar"); err != nil {
		t.Fatal(err)
	}
	if _, err := im.Put("repo/a/1.0/a.jar", strings.NewReader("v4")); err != nil {
		t.Fatalf("re-put after delete: %v", err)
	}
}
