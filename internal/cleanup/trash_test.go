package cleanup_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/cleanup"
)

func hasTrashKey(t *testing.T, b blob.Store, ts *cleanup.Tombstone) bool {
	t.Helper()
	for _, bl := range ts.Blobs {
		if !strings.HasPrefix(bl.Trash, cleanup.TrashPrefix) {
			t.Fatalf("trash key %q not under %q", bl.Trash, cleanup.TrashPrefix)
		}
		if _, ok, _ := b.Stat(bl.Trash); ok {
			return true
		}
	}
	return false
}

func TestTrashVersion_Maven_MovesOutOfRepoSpace(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "maven-hosted/com/example/app/1.0.0/app-1.0.0.jar")
	putBlob(t, b, "maven-hosted/com/example/app/1.0.0/app-1.0.0.pom")
	putBlob(t, b, "maven-hosted/com/example/app/2.0.0/app-2.0.0.jar")

	ts, err := cleanup.TrashVersion("maven-hosted", "maven", "com.example:app", "1.0.0", "alice", b, m)
	if err != nil {
		t.Fatalf("TrashVersion: %v", err)
	}
	if len(ts.Blobs) != 2 {
		t.Fatalf("trashed blobs = %d, want 2", len(ts.Blobs))
	}
	// Original keys gone from the repo's key space (so quota/integrity ignore them).
	if _, ok, _ := b.Stat("maven-hosted/com/example/app/1.0.0/app-1.0.0.jar"); ok {
		t.Error("1.0.0 jar should be moved out of repo space")
	}
	if keys, _ := b.List("maven-hosted/"); len(keys) != 1 {
		t.Errorf("repo space should retain only 2.0.0 (1 key), got %d: %v", len(keys), keys)
	}
	if !hasTrashKey(t, b, ts) {
		t.Error("bytes should exist under the trash prefix")
	}

	// Restore round-trip.
	if _, err := cleanup.RestoreVersion(m, b, ts.Repo, ts.ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if _, ok, _ := b.Stat("maven-hosted/com/example/app/1.0.0/app-1.0.0.jar"); !ok {
		t.Error("1.0.0 jar should be restored to repo space")
	}
	if _, ok, _ := cleanup.GetTombstone(m, ts.ID); ok {
		t.Error("tombstone should be gone after restore")
	}
}

func TestTrashVersion_NPM_RestoresPackumentEntry(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/leftpad/-/leftpad-1.0.0.tgz")
	// Per-version record + a two-version packument.
	m.PutJSON("npm-hosted:npm:v", "leftpad:1.0.0", map[string]any{"name": "leftpad", "version": "1.0.0"}) //nolint:errcheck
	m.PutJSON("npm-hosted:npm", "leftpad", map[string]any{
		"name": "leftpad",
		"versions": map[string]any{
			"1.0.0": map[string]any{"version": "1.0.0"},
			"2.0.0": map[string]any{"version": "2.0.0"},
		},
	}) //nolint:errcheck

	ts, err := cleanup.TrashVersion("npm-hosted", "npm", "leftpad", "1.0.0", "bob", b, m)
	if err != nil {
		t.Fatalf("TrashVersion: %v", err)
	}
	if ts.PkgDoc == nil {
		t.Fatal("expected packument entry to be captured")
	}
	// Packument no longer advertises 1.0.0; the per-version record is gone.
	var pk map[string]any
	m.GetJSON("npm-hosted:npm", "leftpad", &pk) //nolint:errcheck
	if vers := pk["versions"].(map[string]any); vers["1.0.0"] != nil {
		t.Error("packument should not list 1.0.0 after trash")
	} else if vers["2.0.0"] == nil {
		t.Error("packument should still list 2.0.0")
	}
	if ok, _ := m.GetJSON("npm-hosted:npm:v", "leftpad:1.0.0", &map[string]any{}); ok {
		t.Error("per-version record should be removed")
	}

	// Restore re-adds the version to the packument and the per-version record.
	if _, err := cleanup.RestoreVersion(m, b, ts.Repo, ts.ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	m.GetJSON("npm-hosted:npm", "leftpad", &pk) //nolint:errcheck
	if vers := pk["versions"].(map[string]any); vers["1.0.0"] == nil {
		t.Error("packument should list 1.0.0 again after restore")
	}
	if ok, _ := m.GetJSON("npm-hosted:npm:v", "leftpad:1.0.0", &map[string]any{}); !ok {
		t.Error("per-version record should be restored")
	}
	if _, ok, _ := b.Stat("npm-hosted/leftpad/-/leftpad-1.0.0.tgz"); !ok {
		t.Error("tarball should be restored")
	}
}

func TestTrashVersion_NotFound(t *testing.T) {
	b, m := stores(t)
	if _, err := cleanup.TrashVersion("maven-hosted", "maven", "com.example:app", "9.9.9", "", b, m); err == nil {
		t.Error("trashing a missing version should error")
	}
}

func TestListAndPurgeTrash(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "maven-hosted/com/example/app/1.0.0/app-1.0.0.jar")
	ts, err := cleanup.TrashVersion("maven-hosted", "maven", "com.example:app", "1.0.0", "", b, m)
	if err != nil {
		t.Fatalf("TrashVersion: %v", err)
	}
	list, _ := cleanup.ListTrash(m, "maven-hosted")
	if len(list) != 1 {
		t.Fatalf("ListTrash = %d, want 1", len(list))
	}
	// A different repo doesn't see it.
	if other, _ := cleanup.ListTrash(m, "npm-hosted"); len(other) != 0 {
		t.Errorf("cross-repo leak: %d", len(other))
	}
	freed, err := cleanup.PurgeTombstone(m, b, ts.Repo, ts.ID)
	if err != nil {
		t.Fatalf("PurgeTombstone: %v", err)
	}
	if freed <= 0 {
		t.Error("purge should free bytes")
	}
	if _, ok, _ := b.Stat(ts.Blobs[0].Trash); ok {
		t.Error("trash blob should be hard-deleted on purge")
	}
	if list, _ := cleanup.ListTrash(m, "maven-hosted"); len(list) != 0 {
		t.Error("tombstone should be gone after purge")
	}
}

func TestPurgeExpired(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "maven-hosted/com/example/old/1.0.0/old-1.0.0.jar")
	putBlob(t, b, "maven-hosted/com/example/new/1.0.0/new-1.0.0.jar")
	old, _ := cleanup.TrashVersion("maven-hosted", "maven", "com.example:old", "1.0.0", "", b, m)
	fresh, _ := cleanup.TrashVersion("maven-hosted", "maven", "com.example:new", "1.0.0", "", b, m)

	// Age the "old" tombstone past retention by rewriting its DeletedAt.
	old.DeletedAt = time.Now().UTC().Add(-10 * 24 * time.Hour)
	m.PutJSON(cleanup.TrashNS, old.ID, old) //nolint:errcheck

	purged, freed, err := cleanup.PurgeExpired(m, b, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only the aged one)", purged)
	}
	if freed <= 0 {
		t.Error("expected freed bytes")
	}
	// The fresh one survives.
	if _, ok, _ := cleanup.GetTombstone(m, fresh.ID); !ok {
		t.Error("fresh tombstone should survive retention")
	}
	if _, ok, _ := cleanup.GetTombstone(m, old.ID); ok {
		t.Error("aged tombstone should be purged")
	}
}

// A CRAN version is its source tarball and every binary of it. Trashing only
// the source left the binaries installable — and in a binary-only repository
// (the Linux binary cache) there was nothing to trash at all.
func TestTrashVersion_CRAN_TrashesBinariesWithSource(t *testing.T) {
	b, m := stores(t)
	keep := []string{
		"cran-hosted/src/contrib/pkg_2.0.tar.gz",
		"cran-hosted/bin/windows/contrib/4.6/pkg_2.0.zip",
		"cran-hosted/bin/windows/contrib/4.6/pkgx_1.0.zip", // other package, same version
	}
	gone := []string{
		"cran-hosted/src/contrib/pkg_1.0.tar.gz",
		"cran-hosted/bin/windows/contrib/4.6/pkg_1.0.zip",
		"cran-hosted/bin/macosx/big-sur-arm64/contrib/4.6/pkg_1.0.tgz",
		"cran-hosted/bin/linux/noble-x86_64/contrib/4.6/pkg_1.0.tar.gz",
	}
	for _, k := range append(keep, gone...) {
		putBlob(t, b, k)
	}
	m.PutJSON("cran-hosted+cran", "pkg_1.0", map[string]any{"package": "pkg"})                              //nolint:errcheck
	m.PutJSON("cran-hosted+cran+bin+macosx+big-sur-arm64+4.6", "pkg_1.0", map[string]any{"package": "pkg"}) //nolint:errcheck
	m.PutJSON("cran-hosted+cran+bin+linux+noble-x86_64+4.6", "pkg_1.0", map[string]any{"package": "pkg"})   //nolint:errcheck

	ts, err := cleanup.TrashVersion("cran-hosted", "cran", "pkg", "1.0", "alice", b, m)
	if err != nil {
		t.Fatalf("TrashVersion: %v", err)
	}
	if len(ts.Blobs) != len(gone) || len(ts.Metas) != 3 {
		t.Fatalf("trashed %d blobs / %d metas, want %d / 3", len(ts.Blobs), len(ts.Metas), len(gone))
	}
	for _, k := range gone {
		if _, ok, _ := b.Stat(k); ok {
			t.Errorf("%s should be in trash", k)
		}
	}
	for _, k := range keep {
		if _, ok, _ := b.Stat(k); !ok {
			t.Errorf("%s should be untouched", k)
		}
	}
	if ok, _ := m.GetJSON("cran-hosted+cran+bin+linux+noble-x86_64+4.6", "pkg_1.0", &map[string]any{}); ok {
		t.Error("linux binary meta record should be captured into the tombstone")
	}

	if _, err := cleanup.RestoreVersion(m, b, ts.Repo, ts.ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	for _, k := range gone {
		if _, ok, _ := b.Stat(k); !ok {
			t.Errorf("%s should be restored", k)
		}
	}
	if ok, _ := m.GetJSON("cran-hosted+cran+bin+linux+noble-x86_64+4.6", "pkg_1.0", &map[string]any{}); !ok {
		t.Error("linux binary meta record should be restored")
	}
}

func TestTrashVersion_CRAN_BinaryOnlyRepo(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "cran-linux/bin/linux/noble-x86_64/contrib/4.6/pkg_1.0.tar.gz")
	ts, err := cleanup.TrashVersion("cran-linux", "cran", "pkg", "1.0", "alice", b, m)
	if err != nil {
		t.Fatalf("TrashVersion: %v", err)
	}
	if len(ts.Blobs) != 1 {
		t.Fatalf("trashed %d blobs, want 1", len(ts.Blobs))
	}
}

// Restoring over an artifact published since the delete would swap newer bytes
// for older ones; it is refused, and nothing is half-restored.
func TestRestoreVersion_RefusesOccupiedLocation(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "cran-linux/bin/linux/noble-x86_64/contrib/4.6/pkg_1.0.tar.gz")
	putBlob(t, b, "cran-linux/bin/linux/noble-x86_64/contrib/4.5/pkg_1.0.tar.gz")
	ts, err := cleanup.TrashVersion("cran-linux", "cran", "pkg", "1.0", "alice", b, m)
	if err != nil {
		t.Fatal(err)
	}
	putBlob(t, b, "cran-linux/bin/linux/noble-x86_64/contrib/4.6/pkg_1.0.tar.gz") // rebuilt since

	if _, err := cleanup.RestoreVersion(m, b, ts.Repo, ts.ID); !errors.Is(err, cleanup.ErrRestoreConflict) {
		t.Fatalf("want ErrRestoreConflict, got %v", err)
	}
	if _, ok, _ := b.Stat("cran-linux/bin/linux/noble-x86_64/contrib/4.5/pkg_1.0.tar.gz"); ok {
		t.Error("a refused restore moved part of the version back")
	}
	if _, ok, _ := cleanup.GetTombstone(m, ts.ID); !ok {
		t.Error("a refused restore must keep the tombstone")
	}
}
