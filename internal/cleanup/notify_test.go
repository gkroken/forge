package cleanup

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

func notifyStores(t *testing.T) (blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

// seedHelmVersions writes blob + meta records for each version of one chart.
func seedHelmVersions(t *testing.T, b blob.Store, m meta.Store, repoName, chart string, versions ...string) {
	t.Helper()
	ns := repoName + ":helm"
	for _, v := range versions {
		filename := chart + "-" + v + ".tgz"
		if _, err := b.Put(repoName+"/"+filename, strings.NewReader("data")); err != nil {
			t.Fatal(err)
		}
		rec := helmRecord{Name: chart, Version: v, Filename: filename}
		if err := m.PutJSON(ns, chart+"-"+v, rec); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNotify_Gating covers the synchronous decision: only a hosted repo whose
// policy has RunOnPublish set schedules a run, and the per-repo cooldown
// coalesces rapid notifications into one.
func TestNotify_Gating(t *testing.T) {
	b, m := notifyStores(t)
	mgr := repo.NewManager()
	pm := NewPolicyManager(m)

	if err := pm.Put(NamedPolicy{Name: "on-pub", KeepVersions: 2, RunOnPublish: true}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Put(NamedPolicy{Name: "sched-only", KeepVersions: 2}); err != nil {
		t.Fatal(err)
	}
	mustAdd := func(r repo.Repository) {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(repo.Repository{Name: "hosted-pub", Format: "helm", Kind: repo.Hosted, CleanupPolicyName: "on-pub"})
	mustAdd(repo.Repository{Name: "hosted-sched", Format: "helm", Kind: repo.Hosted, CleanupPolicyName: "sched-only"})
	mustAdd(repo.Repository{Name: "hosted-none", Format: "helm", Kind: repo.Hosted})
	mustAdd(repo.Repository{Name: "proxy-pub", Format: "helm", Kind: repo.Proxy, CleanupPolicyName: "on-pub"})

	s := NewScheduler(mgr, pm, b, m)
	clock := time.Now()
	s.now = func() time.Time { return clock }

	if s.Notify("nonexistent") {
		t.Error("unknown repo should not schedule a run")
	}
	if s.Notify("hosted-none") {
		t.Error("repo without a policy should not schedule a run")
	}
	if s.Notify("hosted-sched") {
		t.Error("policy without RunOnPublish should not schedule a run")
	}
	if s.Notify("proxy-pub") {
		t.Error("proxy repo should not schedule a run")
	}

	// First publish on the opted-in repo fires.
	if !s.Notify("hosted-pub") {
		t.Fatal("first publish on RunOnPublish repo should schedule a run")
	}
	// A second publish within the cooldown is coalesced away.
	if s.Notify("hosted-pub") {
		t.Error("second publish within cooldown should be coalesced")
	}
	// After the cooldown elapses, it fires again.
	clock = clock.Add(publishCooldown + time.Second)
	if !s.Notify("hosted-pub") {
		t.Error("publish after cooldown should schedule a run")
	}
}

// TestNotify_RunsDeletion verifies the end-to-end path: an opted-in publish
// actually prunes via the background goroutine.
func TestNotify_RunsDeletion(t *testing.T) {
	b, m := notifyStores(t)
	mgr := repo.NewManager()
	pm := NewPolicyManager(m)
	if err := pm.Put(NamedPolicy{Name: "keep-2", KeepVersions: 2, RunOnPublish: true}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Add(repo.Repository{
		Name: "charts", Format: "helm", Kind: repo.Hosted, CleanupPolicyName: "keep-2",
	}); err != nil {
		t.Fatal(err)
	}
	// Three versions present; keep-2 should prune the lowest (0.1.0).
	seedHelmVersions(t, b, m, "charts", "myapp", "0.1.0", "0.2.0", "0.3.0")

	s := NewScheduler(mgr, pm, b, m)
	if !s.Notify("charts") {
		t.Fatal("expected publish to schedule a run")
	}

	// Wait for the background goroutine to prune 0.1.0.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, exists, _ := b.Stat("charts/myapp-0.1.0.tgz")
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("on-publish run did not delete myapp-0.1.0 within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Newest two must survive.
	for _, v := range []string{"0.2.0", "0.3.0"} {
		if _, exists, _ := b.Stat("charts/myapp-" + v + ".tgz"); !exists {
			t.Fatalf("expected myapp-%s to be kept", v)
		}
	}
}
