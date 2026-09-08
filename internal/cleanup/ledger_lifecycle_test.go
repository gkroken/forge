package cleanup_test

import (
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/ledger"
	"forge/internal/repo"
)

// A version deleted through a format's own API (npm unpublish, helm DELETE,
// maven DELETE) leaves its ledger entry behind, because only cleanup's two
// paths call Forget. Re-publishing the same coordinates then inherits the OLD
// publish date — and retention deletes the brand-new artifact immediately.
func TestLedger_StaleEntryKillsRepublishedArtifact(t *testing.T) {
	b, m := stores(t)

	// Published 60 days ago, then removed by the format's own delete endpoint
	// (which does not touch the ledger).
	ledger.RecordAt(m, "npm-hosted", "p", "1.0.0", time.Now().UTC().AddDate(0, 0, -60))

	// Re-published today.
	putBlob(t, b, "npm-hosted/p/-/p-1.0.0.tgz")
	if err := m.PutJSON("npm-hosted:npm:v", "p:1.0.0", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	ledger.Record(m, "npm-hosted", "p", "1.0.0") // the publish handler's call

	res, err := cleanup.Run(rp("npm-hosted", "npm"), formats(),
		&repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("retention deleted a freshly published artifact (%d) — it inherited a stale ledger date", res.Deleted)
	}
}

// TestLedger_RepublishResetsAge — the flip side: a version genuinely
// re-published is new, and its age restarts. Retention must not treat it as old
// on the strength of a previous publish.
func TestLedger_RepublishResetsAge(t *testing.T) {
	_, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -100)
	ledger.RecordAt(m, "r", "pkg", "1.0.0", old)
	ledger.Record(m, "r", "pkg", "1.0.0")

	got := ledger.Load(m, "r")[ledger.Key("pkg", "1.0.0")]
	if time.Since(got) > time.Minute {
		t.Errorf("publish time = %v, want ~now — a republish must reset the clock", got)
	}
}

// TestLedger_ForgetRemovesEntry — Forget still matters for keeping the ledger
// from growing without bound; it is just no longer load-bearing for correctness.
func TestLedger_ForgetRemovesEntry(t *testing.T) {
	_, m := stores(t)
	ledger.Record(m, "r", "pkg", "1.0.0")
	ledger.Forget(m, "r", "pkg", "1.0.0")
	if _, ok := ledger.Load(m, "r")[ledger.Key("pkg", "1.0.0")]; ok {
		t.Error("Forget left the entry behind")
	}
}

