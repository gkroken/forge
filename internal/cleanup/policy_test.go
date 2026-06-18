package cleanup_test

import (
	"testing"
	"time"

	"forge/internal/cleanup"
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
