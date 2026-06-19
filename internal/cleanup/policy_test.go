package cleanup_test

import (
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

func TestPolicyManager_PutAndGet(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	p := cleanup.NamedPolicy{
		Name:            "keep-3",
		Description:     "Keep last 3 versions",
		KeepVersions:    3,
		KeepReleasesOnly: false,
		Interval:        24 * time.Hour,
	}
	if err := pm.Put(p); err != nil {
		t.Fatal(err)
	}

	got, ok, err := pm.Get("keep-3")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected policy to exist")
	}
	if got.Name != p.Name || got.KeepVersions != p.KeepVersions || got.Interval != p.Interval {
		t.Errorf("got %+v, want %+v", got, p)
	}
}

func TestPolicyManager_List(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := pm.Put(cleanup.NamedPolicy{Name: name, KeepVersions: 5}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := pm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 policies, got %d", len(list))
	}
}

func TestPolicyManager_Delete(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	if err := pm.Put(cleanup.NamedPolicy{Name: "to-delete", KeepVersions: 1}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Delete("to-delete"); err != nil {
		t.Fatal(err)
	}
	_, ok, err := pm.Get("to-delete")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected policy to be gone after delete")
	}
}

func TestPolicyManager_InvalidName(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	for _, bad := range []string{"", "Has-Upper", "has spaces", "-leading-dash"} {
		if err := pm.Put(cleanup.NamedPolicy{Name: bad}); err == nil {
			t.Errorf("expected error for name %q, got none", bad)
		}
	}
}

func TestPolicyManager_GetMissing(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	_, ok, err := pm.Get("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected ok=false for missing policy")
	}
}

func TestNamedPolicy_ToCleanupPolicy(t *testing.T) {
	p := cleanup.NamedPolicy{
		Name:                "my-policy",
		KeepVersions:        5,
		KeepReleasesOnly:    true,
		DeleteOlderThanDays: 30,
		DeleteSnapshotsDays: 7,
		Interval:            48 * time.Hour,
	}
	cp := p.ToCleanupPolicy()
	if cp.KeepVersions != 5 || !cp.KeepReleasesOnly || cp.DeleteOlderThanDays != 30 ||
		cp.DeleteSnapshotsDays != 7 || cp.Interval != 48*time.Hour {
		t.Errorf("ToCleanupPolicy mismatch: %+v", cp)
	}
}

func TestNamedPolicy_JSONRoundtrip(t *testing.T) {
	_, m := stores(t)
	pm := cleanup.NewPolicyManager(m)

	original := cleanup.NamedPolicy{
		Name:        "roundtrip",
		Description: "test",
		KeepVersions: 10,
		Interval:    72 * time.Hour,
	}
	if err := pm.Put(original); err != nil {
		t.Fatal(err)
	}
	got, ok, err := pm.Get("roundtrip")
	if err != nil || !ok {
		t.Fatalf("get failed: ok=%v err=%v", ok, err)
	}
	if got.Interval != 72*time.Hour {
		t.Errorf("interval not preserved: got %v, want %v", got.Interval, 72*time.Hour)
	}
}

func TestReclaimable_Empty(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if got := cleanup.Reclaimable(pm, mgr, b, m); got != 0 {
		t.Errorf("want 0 for empty store, got %d", got)
	}
}

func TestReclaimable_NoPolicyName(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	mgr.Add(repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted}) //nolint:errcheck
	// No CleanupPolicyName set — should return 0 without panicking.
	if got := cleanup.Reclaimable(pm, mgr, b, m); got != 0 {
		t.Errorf("want 0 when no policy assigned, got %d", got)
	}
}

func TestReclaimable_WithPolicy(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)

	pm.Put(cleanup.NamedPolicy{Name: "keep-1", KeepVersions: 1}) //nolint:errcheck
	mgr.Add(repo.Repository{                                       //nolint:errcheck
		Name: "helm-hosted", Format: "helm", Kind: repo.Hosted,
		CleanupPolicyName: "keep-1",
	})
	// No blobs in the store — reclaimable should be 0.
	if got := cleanup.Reclaimable(pm, mgr, b, m); got != 0 {
		t.Errorf("want 0 with empty blob store, got %d", got)
	}
}
