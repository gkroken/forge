package cleanup

import (
	"time"

	"forge/internal/meta"
)

// downloadNS is the meta.Store namespace mapping a blob key to its most recent
// download time. Keys are blob keys ("{repo}/{sub-path}"); meta.FS encodes the
// slashes. Consumed by the LastDownloadedDays retention rule.
const downloadNS = "download-times"

// downloadThrottle bounds write volume: a download is re-stamped at most once
// per this window, so a hot artifact doesn't cause a write on every GET.
const downloadThrottle = time.Hour

type downloadRecord struct {
	DownloadedAt time.Time `json:"downloadedAt"`
}

// RecordDownload stamps blobKey as downloaded now. It is throttled — if an
// existing stamp is newer than downloadThrottle it skips the write — so it is
// cheap to call on every artifact GET. Safe to call from a goroutine; errors
// are ignored (a missed stamp only delays last-downloaded eviction).
func RecordDownload(m meta.Store, blobKey string) {
	if m == nil || blobKey == "" {
		return
	}
	var rec downloadRecord
	if ok, _ := m.GetJSON(downloadNS, blobKey, &rec); ok {
		if time.Since(rec.DownloadedAt) < downloadThrottle {
			return
		}
	}
	_ = m.PutJSON(downloadNS, blobKey, downloadRecord{DownloadedAt: time.Now().UTC()})
}

// lastDownloadTime returns the most recent recorded download time across
// blobKeys, or the zero time if none were ever downloaded.
func lastDownloadTime(m meta.Store, blobKeys ...string) time.Time {
	var latest time.Time
	for _, k := range blobKeys {
		var rec downloadRecord
		if ok, _ := m.GetJSON(downloadNS, k, &rec); ok && rec.DownloadedAt.After(latest) {
			latest = rec.DownloadedAt
		}
	}
	return latest
}
