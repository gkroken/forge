package cleanup_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

func TestCleanupPolicy_IntervalJSON(t *testing.T) {
	p := repo.CleanupPolicy{KeepVersions: 5, Interval: 24 * time.Hour}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"keepVersions":5,"interval":"24h0m0s"}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
	var got repo.CleanupPolicy
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Interval != 24*time.Hour {
		t.Fatalf("interval: got %v, want 24h", got.Interval)
	}
	if got.KeepVersions != 5 {
		t.Fatalf("keepVersions: got %d, want 5", got.KeepVersions)
	}
}

func TestCleanupPolicy_IntervalJSON_Zero(t *testing.T) {
	p := repo.CleanupPolicy{KeepVersions: 3}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got repo.CleanupPolicy
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Interval != 0 {
		t.Fatalf("expected zero interval, got %v", got.Interval)
	}
}

func TestCleanupPolicy_IntervalJSON_Invalid(t *testing.T) {
	var p repo.CleanupPolicy
	if err := json.Unmarshal([]byte(`{"interval":"notaduration"}`), &p); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestScheduler_StartsAndStops(t *testing.T) {
	_, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	b, _ := stores(t)
	ctx, cancel := context.WithCancel(context.Background())
	cleanup.NewScheduler(mgr, pm, b, m).Start(ctx)
	cancel()
}

func TestScheduler_RunDue_SkipsNoPolicy(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if err := mgr.Add(repo.Repository{Name: "r", Format: "helm", Kind: repo.Hosted}); err != nil {
		t.Fatal(err)
	}
	// No CleanupPolicyName set — RunDue should be a no-op (no panic, no error).
	cleanup.NewScheduler(mgr, pm, b, m).RunDue(time.Now(), map[string]time.Time{})
}

func TestScheduler_RunDue_SkipsBeforeInterval(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if err := pm.Put(cleanup.NamedPolicy{Name: "keep-1", KeepVersions: 1, Interval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "r", Format: "helm", Kind: repo.Hosted,
		CleanupPolicyName: "keep-1",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastRun := map[string]time.Time{"r": now.Add(-30 * time.Minute)} // not yet due
	cleanup.NewScheduler(mgr, pm, b, m).RunDue(now, lastRun)
	// lastRun should be unchanged since we didn't fire.
	if !lastRun["r"].Equal(now.Add(-30 * time.Minute)) {
		t.Fatal("lastRun should not have been updated")
	}
}

func TestScheduler_RunDue_FiresWhenDue(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if err := pm.Put(cleanup.NamedPolicy{Name: "keep-1", KeepVersions: 1, Interval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "r", Format: "helm", Kind: repo.Hosted,
		CleanupPolicyName: "keep-1",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastRun := map[string]time.Time{"r": now.Add(-2 * time.Hour)} // overdue
	cleanup.NewScheduler(mgr, pm, b, m).RunDue(now, lastRun)
	// lastRun should be updated to now.
	if !lastRun["r"].Equal(now) {
		t.Fatalf("lastRun not updated: got %v, want %v", lastRun["r"], now)
	}
}

func TestScheduler_LastRuns_Empty(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	sched := cleanup.NewScheduler(mgr, pm, b, m)
	if got := sched.LastRuns(); len(got) != 0 {
		t.Errorf("want empty LastRuns, got %v", got)
	}
}

// Proxy repos are now processed by the scheduler (cache eviction), so a due
// proxy repo with a scheduled policy advances lastRun.
func TestScheduler_RunDue_ProcessesProxyRepo(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if err := pm.Put(cleanup.NamedPolicy{Name: "cache-30", LastDownloadedDays: 30, Interval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "r", Format: "cran", Kind: repo.Proxy,
		CleanupPolicyName: "cache-30",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastRun := map[string]time.Time{"r": now.Add(-2 * time.Hour)}
	cleanup.NewScheduler(mgr, pm, b, m).RunDue(now, lastRun)
	if !lastRun["r"].Equal(now) {
		t.Fatal("lastRun should be updated — proxy repos are now scheduled for cache eviction")
	}
}

// Group repos own no storage and are always skipped.
func TestScheduler_RunDue_SkipsGroupRepo(t *testing.T) {
	b, m := stores(t)
	mgr := repo.NewManager()
	pm := cleanup.NewPolicyManager(m)
	if err := pm.Put(cleanup.NamedPolicy{Name: "cache-30", LastDownloadedDays: 30, Interval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "g", Format: "maven", Kind: repo.Group, Members: []string{"x"},
		CleanupPolicyName: "cache-30",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastRun := map[string]time.Time{"g": now.Add(-2 * time.Hour)}
	cleanup.NewScheduler(mgr, pm, b, m).RunDue(now, lastRun)
	if lastRun["g"].Equal(now) {
		t.Fatal("lastRun was updated for a group repo — should have been skipped")
	}
}
