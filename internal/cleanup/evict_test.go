package cleanup

import (
	"strings"
	"testing"
	"time"

	"forge/internal/repo"
)

// TestEvictProxyCache evicts cached blobs whose last download is older than the
// cutoff, keeps warm ones, and never touches a blob that was never downloaded.
func TestEvictProxyCache(t *testing.T) {
	b, m := notifyStores(t)

	put := func(key string) {
		if _, err := b.Put(key, strings.NewReader("cached")); err != nil {
			t.Fatal(err)
		}
	}
	put("npm-proxy/left-pad/-/left-pad-1.0.0.tgz") // cold → evict
	put("npm-proxy/chalk/-/chalk-5.0.0.tgz")        // warm → keep
	put("npm-proxy/uncounted/-/uncounted-1.0.0.tgz") // never downloaded → keep

	now := time.Now().UTC()
	stamp := func(key string, at time.Time) {
		if err := m.PutJSON(downloadNS, key, downloadRecord{DownloadedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	stamp("npm-proxy/left-pad/-/left-pad-1.0.0.tgz", now.AddDate(0, 0, -45))
	stamp("npm-proxy/chalk/-/chalk-5.0.0.tgz", now.AddDate(0, 0, -3))

	p := &repo.CleanupPolicy{LastDownloadedDays: 30}
	res, err := EvictProxyCache("npm-proxy", p, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 eviction, got %d", res.Deleted)
	}
	if _, ok, _ := b.Stat("npm-proxy/left-pad/-/left-pad-1.0.0.tgz"); ok {
		t.Error("expected cold left-pad to be evicted")
	}
	for _, k := range []string{
		"npm-proxy/chalk/-/chalk-5.0.0.tgz",
		"npm-proxy/uncounted/-/uncounted-1.0.0.tgz",
	} {
		if _, ok, _ := b.Stat(k); !ok {
			t.Errorf("expected %s to be kept", k)
		}
	}
	// The evicted blob's stale stamp is cleaned up.
	if got := lastDownloadTime(m, "npm-proxy/left-pad/-/left-pad-1.0.0.tgz"); !got.IsZero() {
		t.Error("expected the evicted blob's download stamp to be removed")
	}
}

// TestEvictProxyCache_NoDownloadRule is a no-op without LastDownloadedDays
// (count/snapshot rules don't apply to a cache).
func TestEvictProxyCache_NoDownloadRule(t *testing.T) {
	b, m := notifyStores(t)
	if _, err := b.Put("p/a.tgz", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := m.PutJSON(downloadNS, "p/a.tgz", downloadRecord{DownloadedAt: time.Now().AddDate(0, 0, -100)}); err != nil {
		t.Fatal(err)
	}
	res, err := EvictProxyCache("p", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("expected no eviction without LastDownloadedDays, got %d", res.Deleted)
	}
}

// TestRunForRepo_Dispatch routes hosted→retention, proxy→eviction.
func TestRunForRepo_Dispatch(t *testing.T) {
	b, m := notifyStores(t)
	seedHelmVersions(t, b, m, "h", "app", "1.0.0", "2.0.0", "3.0.0")

	// Hosted: keep-2 retention deletes the lowest version.
	hosted := repo.Repository{Name: "h", Format: "helm", Kind: repo.Hosted}
	res, err := RunForRepo(hosted, &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("hosted keep-2 expected 1 deletion, got %d", res.Deleted)
	}

	// Proxy: keep-2 is ignored (cache), nothing deleted.
	proxy := repo.Repository{Name: "h", Format: "helm", Kind: repo.Proxy}
	res, err = RunForRepo(proxy, &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("proxy should ignore count-based rules, got %d deletions", res.Deleted)
	}
}
