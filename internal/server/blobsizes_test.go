package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

func TestGetBlobSizes_Empty(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	srv := New(mgr, reg, b, m, nil)

	sizes := srv.GetBlobSizes()
	if sizes.TotalBytes != 0 {
		t.Errorf("want 0 before walk, got %d", sizes.TotalBytes)
	}
}

func TestWalkBlobSizes(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()

	mgr.Add(repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted}) //nolint:errcheck

	// Seed two blobs.
	b.Put("maven-hosted/com/example/foo-1.0.jar", strings.NewReader("hello"))    //nolint:errcheck
	b.Put("maven-hosted/com/example/foo-1.1.jar", strings.NewReader("hi there")) //nolint:errcheck

	srv := New(mgr, reg, b, m, nil)
	srv.walkBlobSizes()

	sizes := srv.GetBlobSizes()
	if sizes.TotalBytes == 0 {
		t.Error("want non-zero total after walk")
	}
	if sizes.ByFormat["maven"] == 0 {
		t.Error("want non-zero maven bytes after walk")
	}
	if sizes.ComputedAt.IsZero() {
		t.Error("want ComputedAt set after walk")
	}
}

func TestWithBlobWalker_StartStop(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	srv := New(mgr, reg, b, m, nil)

	ctx, cancel := context.WithCancel(context.Background())
	got := srv.WithBlobWalker(ctx)
	if got != srv {
		t.Error("WithBlobWalker must return the same server instance")
	}
	cancel() // signal goroutine to stop — just verifies no panic
}
