package integrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/proxy"
)

func newStores(t *testing.T) (*blob.FS, *meta.FS) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(dir + "/meta")
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func TestHashBlob(t *testing.T) {
	b, _ := newStores(t)
	body := []byte("hello integrity")
	if _, err := b.Put("r/a.bin", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	h, err := HashBlob(b, "r/a.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if h.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %s, want %s", h.SHA256, hex.EncodeToString(want[:]))
	}
	if h.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", h.Size, len(body))
	}
	if len(h.SHA512) != 64 || h.SHA1 == "" || h.MD5 == "" {
		t.Error("expected all digests populated")
	}
	if _, err := HashBlob(b, "r/absent"); err == nil {
		t.Error("expected error for absent blob")
	}
}

func TestVerifyProxyCache(t *testing.T) {
	b, m := newStores(t)
	repoName := "npm-proxy"
	cacheNS := repoName + ":proxy"

	// Paired blob + entry: clean.
	b.Put(repoName+"/is-odd/-/is-odd-1.0.0.tgz", strings.NewReader("tar")) //nolint:errcheck
	m.PutJSON(cacheNS, repoName+"/is-odd/-/is-odd-1.0.0.tgz", proxy.CacheEntry{FetchedAt: time.Now()})

	// Orphan blob: no cache entry.
	b.Put(repoName+"/lodash/-/lodash-1.0.0.tgz", strings.NewReader("tar2")) //nolint:errcheck

	// Dangling entry (post-eviction): must NOT be reported.
	m.PutJSON(cacheNS, repoName+"/evicted/-/evicted-1.0.0.tgz", proxy.CacheEntry{FetchedAt: time.Now()})

	// Meta-keyed entry (npm packument convention) + health key: ignored here.
	m.PutJSON(cacheNS, "is-odd", proxy.CacheEntry{FetchedAt: time.Now()})
	m.PutJSON(cacheNS, proxy.HealthKey, proxy.HealthRecord{OK: true})

	res, err := VerifyProxyCache(repoName, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly 1", res.Findings)
	}
	f := res.Findings[0]
	if f.Kind != KindOrphan || f.Object != repoName+"/lodash/-/lodash-1.0.0.tgz" {
		t.Errorf("unexpected finding: %+v", f)
	}
	if res.BlobsChecked != 2 {
		t.Errorf("BlobsChecked = %d, want 2", res.BlobsChecked)
	}
	if res.MetaChecked != 2 { // two blob-keyed entries; pkg-keyed + health skipped
		t.Errorf("MetaChecked = %d, want 2", res.MetaChecked)
	}
}

func TestBuildReport_SortsCountsAndCaps(t *testing.T) {
	var res Result
	res.Add(KindOrphan, "r/z", "", "", "z orphan")
	res.Add(KindMissing, "r/b", "c", "1.0", "gone")
	res.Add(KindMissing, "r/a", "c", "1.0", "gone")
	res.BlobsChecked, res.MetaChecked, res.BytesRead = 5, 3, 42

	q := time.Now().Add(-2 * time.Second)
	st := time.Now().Add(-time.Second)
	rep := BuildReport("r", ModeFull, q, st, res, "note")
	if rep.Status != StatusComplete || rep.TotalFindings != 3 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Findings[0].Object != "r/a" || rep.Findings[0].Kind != KindMissing {
		t.Errorf("expected sorted findings, got %+v", rep.Findings)
	}
	if rep.Counts[KindMissing] != 2 || rep.Counts[KindOrphan] != 1 {
		t.Errorf("counts = %v", rep.Counts)
	}
	if rep.Clean() {
		t.Error("report with findings must not be Clean")
	}

	// Cap: counts stay exact, list truncates.
	var big Result
	for i := 0; i < MaxStoredFindings+10; i++ {
		big.Add(KindOrphan, "r/x", "", "", "d")
	}
	brep := BuildReport("r", ModeQuick, q, st, big, "")
	if !brep.Truncated || len(brep.Findings) != MaxStoredFindings || brep.TotalFindings != MaxStoredFindings+10 {
		t.Errorf("truncation wrong: truncated=%v len=%d total=%d", brep.Truncated, len(brep.Findings), brep.TotalFindings)
	}

	// Empty result → Clean, non-nil findings slice.
	crep := BuildReport("r", ModeFull, q, st, Result{}, "")
	if !crep.Clean() || crep.Findings == nil {
		t.Errorf("clean report wrong: %+v", crep)
	}
}

func TestStore_RoundTripAndReplace(t *testing.T) {
	_, m := newStores(t)
	s := NewStore(m)
	if _, ok, err := s.Get("r"); ok || err != nil {
		t.Fatalf("expected no report, got ok=%v err=%v", ok, err)
	}
	if err := s.Put(Report{Repo: "r", Status: StatusQueued, Mode: ModeFull}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Report{Repo: "r", Status: StatusComplete, Mode: ModeFull, TotalFindings: 2}); err != nil {
		t.Fatal(err)
	}
	rep, ok, err := s.Get("r")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if rep.Status != StatusComplete || rep.TotalFindings != 2 {
		t.Errorf("re-run must replace: %+v", rep)
	}
	if err := s.Delete("r"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("r"); ok {
		t.Error("expected deleted")
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{{"", ModeFull, true}, {"full", ModeFull, true}, {"quick", ModeQuick, true}, {"deep", "", false}} {
		got, ok := ParseMode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseMode(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
