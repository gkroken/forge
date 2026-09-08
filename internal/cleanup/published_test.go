package cleanup_test

import (
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/ledger"
	"forge/internal/repo"
)

// TestNPM_DeleteOlderThanDays_NowFires is the regression this whole ledger
// exists for. npm builds its cleanup records from meta key names, so the
// record's own uploadedAt was structurally always zero and the age rule could
// never fire — silently, since a missing timestamp means "skip", not "old".
func TestNPM_DeleteOlderThanDays_NowFires(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/left-pad/-/left-pad-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "left-pad:1.0.0", map[string]any{
		"name": "left-pad", "version": "1.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	ledger.RecordAt(m, "npm-hosted", "left-pad", "1.0.0",
		time.Now().UTC().AddDate(0, 0, -60))

	res, err := cleanup.Run(rp("npm-hosted", "npm"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 — the age rule did not fire", res.Deleted)
	}
	if _, exists, _ := b.Stat("npm-hosted/left-pad/-/left-pad-1.0.0.tgz"); exists {
		t.Error("tarball still present after deletion")
	}
}

// TestNPM_RecentPublishSurvives — the rule must still respect the cutoff.
func TestNPM_RecentPublishSurvives(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/fresh/-/fresh-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "fresh:1.0.0", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	ledger.RecordAt(m, "npm-hosted", "fresh", "1.0.0",
		time.Now().UTC().AddDate(0, 0, -2))

	res, err := cleanup.Run(rp("npm-hosted", "npm"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0 — a 2-day-old package is not older than 30 days", res.Deleted)
	}
}

// TestMavenRelease_DeleteOlderThanDays_NowFires — releases have no snapshot
// record, so before the ledger they were undatable and never aged out.
func TestMavenRelease_DeleteOlderThanDays_NowFires(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "maven-hosted/com/acme/demo/1.0.0/demo-1.0.0.jar")
	ledger.RecordFromMavenPath(m, "maven-hosted", "com/acme/demo/1.0.0/demo-1.0.0.jar")
	// Backdate it: RecordPublishFromMavenPath stamps "now".
	ledger.Forget(m, "maven-hosted", "com/acme/demo", "1.0.0")
	ledger.RecordAt(m, "maven-hosted", "com/acme/demo", "1.0.0",
		time.Now().UTC().AddDate(0, 0, -90))

	res, err := cleanup.Run(rp("maven-hosted", "maven"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 — a release never aged out before the ledger", res.Deleted)
	}
}

// TestLedgerKeyMatchesMavenGrouping guards the one way this silently breaks:
// the ledger component must equal the key maven's cleanup pass groups by.
func TestLedgerKeyMatchesMavenGrouping(t *testing.T) {
	_, m := stores(t)
	ledger.RecordFromMavenPath(m, "r", "com/acme/deep/nested/demo/2.1.0/demo-2.1.0.jar")
	idx := ledger.Load(m, "r")
	want := ledger.Key("com/acme/deep/nested/demo", "2.1.0")
	if _, ok := idx[want]; !ok {
		t.Fatalf("ledger key %q not found; got %v", want, keysOf(idx))
	}
}

// TestRecordPublishDoesNotOverwrite — re-publishing or a proxy re-caching must
// not make an old artifact look new, or retention would never catch up with it.
func TestRecordPublishDoesNotOverwrite(t *testing.T) {
	_, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -100)
	ledger.RecordAt(m, "r", "pkg", "1.0.0", old)
	ledger.Record(m, "r", "pkg", "1.0.0") // would stamp "now"

	got := ledger.Load(m, "r")[ledger.Key("pkg", "1.0.0")]
	if got.Sub(old).Abs() > time.Second {
		t.Errorf("publish time was overwritten: %v, want ~%v", got, old)
	}
}

// TestForgetPublishOnDelete — a deleted version must not leave a ledger entry
// that would make a later republish look instantly old.
func TestForgetPublishOnDelete(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/gone/-/gone-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "gone:1.0.0", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	ledger.RecordAt(m, "npm-hosted", "gone", "1.0.0",
		time.Now().UTC().AddDate(0, 0, -60))

	if _, err := cleanup.Run(rp("npm-hosted", "npm"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.Load(m, "npm-hosted")[ledger.Key("gone", "1.0.0")]; ok {
		t.Error("ledger entry survived the deletion")
	}
}

// TestDryRun_ReportsUnevaluable — an age rule that cannot judge a version must
// say so, instead of returning "no candidates" and reading as "nothing is old
// enough yet".
func TestDryRun_ReportsUnevaluable(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/mystery/-/mystery-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "mystery:1.0.0", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// Deliberately no ledger entry: this version's publish time is unknown.

	res, err := cleanup.DryRun(rp("npm-hosted", "npm"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("must not delete what it cannot date, got %+v", res.Candidates)
	}
	if len(res.Unevaluable) != 1 {
		t.Fatalf("Unevaluable = %+v, want 1 entry", res.Unevaluable)
	}
	if res.Unevaluable[0].Component != "mystery" || res.Unevaluable[0].Version != "1.0.0" {
		t.Errorf("wrong entry: %+v", res.Unevaluable[0])
	}
}

// TestDryRun_NoAgeRuleNoNoise — Unevaluable is about age rules; a policy
// without one must not report every version as a problem.
func TestDryRun_NoAgeRuleNoNoise(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "npm-hosted/quiet/-/quiet-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "quiet:1.0.0", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	res, err := cleanup.DryRun(rp("npm-hosted", "npm"), formats(), &repo.CleanupPolicy{KeepVersions: 5}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unevaluable) != 0 {
		t.Errorf("no age rule configured, but reported %+v", res.Unevaluable)
	}
}

func keysOf(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
