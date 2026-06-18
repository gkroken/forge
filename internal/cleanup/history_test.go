package cleanup_test

import (
	"testing"
	"time"

	"forge/internal/cleanup"
)

func TestHistory_RecordAndGet(t *testing.T) {
	_, m := stores(t)

	now := time.Now().UTC()
	run := cleanup.CleanupRun{
		Timestamp:  now,
		PolicyName: "keep-3",
		Deleted:    5,
		FreedBytes: 1024,
		DurationMs: 120,
	}
	if err := cleanup.RecordRun(m, "myrepo", run); err != nil {
		t.Fatal(err)
	}

	history, err := cleanup.GetHistory(m, "myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	got := history[0]
	if got.Deleted != 5 || got.FreedBytes != 1024 || got.PolicyName != "keep-3" {
		t.Errorf("unexpected history entry: %+v", got)
	}
}

func TestHistory_NewestFirst(t *testing.T) {
	_, m := stores(t)

	base := time.Now().UTC()
	for i := range 5 {
		run := cleanup.CleanupRun{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Deleted:   i,
		}
		if err := cleanup.RecordRun(m, "repo", run); err != nil {
			t.Fatal(err)
		}
	}

	history, err := cleanup.GetHistory(m, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(history))
	}
	// Newest first.
	for i := 1; i < len(history); i++ {
		if !history[i-1].Timestamp.After(history[i].Timestamp) {
			t.Errorf("entry %d is not after entry %d", i-1, i)
		}
	}
}

func TestHistory_TrimTo20(t *testing.T) {
	_, m := stores(t)

	base := time.Now().UTC()
	for i := range 25 {
		run := cleanup.CleanupRun{
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Deleted:   i,
		}
		if err := cleanup.RecordRun(m, "repo", run); err != nil {
			t.Fatal(err)
		}
	}

	history, err := cleanup.GetHistory(m, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 20 {
		t.Fatalf("expected 20 entries after trim, got %d", len(history))
	}
	// Should keep the 20 newest (Deleted = 5..24).
	if history[0].Deleted != 24 {
		t.Errorf("expected newest entry Deleted=24, got %d", history[0].Deleted)
	}
	if history[19].Deleted != 5 {
		t.Errorf("expected oldest kept entry Deleted=5, got %d", history[19].Deleted)
	}
}

func TestHistory_EmptyRepo(t *testing.T) {
	_, m := stores(t)

	history, err := cleanup.GetHistory(m, "no-such-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d entries", len(history))
	}
}

func TestHistory_IsolatedByRepo(t *testing.T) {
	_, m := stores(t)

	now := time.Now().UTC()
	cleanup.RecordRun(m, "repo-a", cleanup.CleanupRun{Timestamp: now, Deleted: 1})       //nolint:errcheck
	cleanup.RecordRun(m, "repo-b", cleanup.CleanupRun{Timestamp: now, Deleted: 99})      //nolint:errcheck

	hA, _ := cleanup.GetHistory(m, "repo-a")
	hB, _ := cleanup.GetHistory(m, "repo-b")
	if len(hA) != 1 || hA[0].Deleted != 1 {
		t.Errorf("repo-a history wrong: %+v", hA)
	}
	if len(hB) != 1 || hB[0].Deleted != 99 {
		t.Errorf("repo-b history wrong: %+v", hB)
	}
}
