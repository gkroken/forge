package cleanup

import (
	"testing"
	"time"
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
