package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

func TestTrashPurgeTick_RespectsRetentionAndInterval(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	mgr.Add(repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted}) //nolint:errcheck

	srv := New(mgr, reg, b, m, nil)
	srv.WithTrashRetention(7 * 24 * time.Hour)

	// One aged tombstone, one fresh.
	b.Put("maven-hosted/com/example/old/1.0.0/old-1.0.0.jar", strings.NewReader("data")) //nolint:errcheck
	b.Put("maven-hosted/com/example/new/1.0.0/new-1.0.0.jar", strings.NewReader("data")) //nolint:errcheck
	old, _ := cleanup.TrashVersion("maven-hosted", "maven", "com.example:old", "1.0.0", "", b, m)
	fresh, _ := cleanup.TrashVersion("maven-hosted", "maven", "com.example:new", "1.0.0", "", b, m)
	old.DeletedAt = time.Now().UTC().Add(-10 * 24 * time.Hour)
	m.PutJSON(cleanup.TrashNS, old.ID, old) //nolint:errcheck

	lastRun := map[string]time.Time{}
	now := time.Now()
	srv.TrashPurgeTick(now, lastRun)

	if _, ok, _ := cleanup.GetTombstone(m, old.ID); ok {
		t.Error("aged tombstone should be purged")
	}
	if _, ok, _ := cleanup.GetTombstone(m, fresh.ID); !ok {
		t.Error("fresh tombstone should survive")
	}
	if lastRun[trashPurgeKey].IsZero() {
		t.Error("last-run should be stamped after a sweep")
	}

	// A second tick within the interval is a no-op (does not re-stamp differently).
	stamp := lastRun[trashPurgeKey]
	srv.TrashPurgeTick(now.Add(time.Hour), lastRun)
	if !lastRun[trashPurgeKey].Equal(stamp) {
		t.Error("sweep should not run again within the interval")
	}
}

func TestTrashPurgeTick_DisabledWhenRetentionZero(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	srv := New(repo.NewManager(), format.NewRegistry(), b, m, nil)
	// TrashRetention left at 0 → disabled.
	lastRun := map[string]time.Time{}
	srv.TrashPurgeTick(time.Now(), lastRun)
	if !lastRun[trashPurgeKey].IsZero() {
		t.Error("disabled retention should not run or stamp the sweep")
	}
}
