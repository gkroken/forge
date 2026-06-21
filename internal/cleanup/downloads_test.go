package cleanup

import (
	"testing"
	"time"

	"forge/internal/repo"
)

func TestRecordDownload_AndLookup(t *testing.T) {
	_, m := notifyStores(t)

	if got := lastDownloadTime(m, "repo/a.jar"); !got.IsZero() {
		t.Fatalf("unrecorded key should be zero, got %v", got)
	}

	RecordDownload(m, "repo/a.jar")
	first := lastDownloadTime(m, "repo/a.jar")
	if first.IsZero() {
		t.Fatal("expected a recorded download time")
	}

	// Throttled: a second immediate record must not advance the timestamp.
	RecordDownload(m, "repo/a.jar")
	if got := lastDownloadTime(m, "repo/a.jar"); !got.Equal(first) {
		t.Fatalf("throttle failed: timestamp advanced from %v to %v", first, got)
	}

	// lastDownloadTime returns the max across keys.
	if err := m.PutJSON(downloadNS, "repo/b.jar", downloadRecord{DownloadedAt: first.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := lastDownloadTime(m, "repo/a.jar", "repo/b.jar"); !got.Equal(first.Add(time.Hour)) {
		t.Fatalf("expected max across keys, got %v", got)
	}
}

// TestRun_LastDownloadedDays verifies the rule deletes a version whose last
// download is older than the cutoff, keeps a recently-downloaded one, and skips
// a version with neither a download nor an upload time.
func TestRun_LastDownloadedDays(t *testing.T) {
	b, m := notifyStores(t)
	seedHelmVersions(t, b, m, "charts", "myapp", "1.0.0", "2.0.0", "3.0.0")

	stamp := func(v string, at time.Time) {
		key := "charts/myapp-" + v + ".tgz"
		if err := m.PutJSON(downloadNS, key, downloadRecord{DownloadedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	stamp("1.0.0", now.AddDate(0, 0, -40)) // stale → delete
	stamp("2.0.0", now.AddDate(0, 0, -2))  // fresh → keep
	// 3.0.0 never downloaded, no upload time → skipped (kept).

	res, err := Run("charts", "helm", &repo.CleanupPolicy{LastDownloadedDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (stale download), got %d", res.Deleted)
	}
	if _, exists, _ := b.Stat("charts/myapp-1.0.0.tgz"); exists {
		t.Error("expected myapp-1.0.0 (stale) to be deleted")
	}
	for _, v := range []string{"2.0.0", "3.0.0"} {
		if _, exists, _ := b.Stat("charts/myapp-" + v + ".tgz"); !exists {
			t.Errorf("expected myapp-%s to be kept", v)
		}
	}
}
