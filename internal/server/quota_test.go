package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// quotaGBFor returns the *float64 QuotaGB value whose byte budget is exactly n.
func quotaGBFor(n int64) *float64 {
	v := float64(n) / float64(bytesPerGB)
	return &v
}

func newQuotaServer(t *testing.T) (*Server, blob.Store, *repo.Manager) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	return New(mgr, reg, b, m, nil), b, mgr
}

func writeReq(t *testing.T, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/repository/maven-hosted/com/example/app/1.0/app-1.0.jar", strings.NewReader(body))
	r.ContentLength = int64(len(body))
	return httptest.NewRecorder(), r
}

func TestQuotaBlocks_UnderQuota_Allows(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	rp := repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, QuotaGB: quotaGBFor(1000)}
	mgr.Add(rp)                                             //nolint:errcheck
	b.Put("maven-hosted/a.jar", strings.NewReader("hello")) //nolint:errcheck
	srv.walkBlobSizes()

	w, r := writeReq(t, "x")
	if srv.quotaBlocks(w, r, rp) {
		t.Fatal("write under quota should be allowed")
	}
}

func TestQuotaBlocks_OverQuota_Refuses507(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	rp := repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, QuotaGB: quotaGBFor(4)}
	mgr.Add(rp)                                             //nolint:errcheck
	b.Put("maven-hosted/a.jar", strings.NewReader("hello")) //nolint:errcheck  (5 bytes > 4)
	srv.walkBlobSizes()

	w, r := writeReq(t, "x")
	if !srv.quotaBlocks(w, r, rp) {
		t.Fatal("write over quota should be refused")
	}
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status: got %d want 507", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["error"] != "storage quota exceeded" {
		t.Errorf("error field: got %v", body["error"])
	}
	if body["repo"] != "maven-hosted" {
		t.Errorf("repo field: got %v", body["repo"])
	}
}

func TestQuotaBlocks_IncomingBodyPushesOver(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	// Usage 5 bytes, quota 10; a 6-byte upload crosses the line.
	rp := repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, QuotaGB: quotaGBFor(10)}
	mgr.Add(rp)                                             //nolint:errcheck
	b.Put("maven-hosted/a.jar", strings.NewReader("hello")) //nolint:errcheck
	srv.walkBlobSizes()

	w, r := writeReq(t, "abcdef") // 6 bytes; 5+6 > 10
	if !srv.quotaBlocks(w, r, rp) {
		t.Fatal("upload that would cross the quota should be refused")
	}
	// A small upload that stays under is allowed.
	w2, r2 := writeReq(t, "ab") // 2 bytes; 5+2 <= 10
	if srv.quotaBlocks(w2, r2, rp) {
		t.Fatal("upload staying under quota should be allowed")
	}
}

func TestQuotaBlocks_ProxyNeverGated(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	rp := repo.Repository{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy, QuotaGB: quotaGBFor(1)}
	mgr.Add(rp)                                                              //nolint:errcheck
	b.Put("npm-proxy/big.tgz", strings.NewReader("way over the tiny quota")) //nolint:errcheck
	srv.walkBlobSizes()

	w, r := writeReq(t, "more")
	if srv.quotaBlocks(w, r, rp) {
		t.Fatal("proxy cache-fills must never be blocked by quota")
	}
}

func TestQuotaBlocks_NoQuota_Allows(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	rp := repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted} // QuotaGB nil
	mgr.Add(rp)                                                                     //nolint:errcheck
	b.Put("maven-hosted/a.jar", strings.NewReader(strings.Repeat("x", 4096)))       //nolint:errcheck
	srv.walkBlobSizes()

	w, r := writeReq(t, "x")
	if srv.quotaBlocks(w, r, rp) {
		t.Fatal("unlimited (nil quota) repo should never be blocked")
	}
}

func TestQuotaDelta_TightensBeforeWalk_AndReconciles(t *testing.T) {
	srv, b, mgr := newQuotaServer(t)
	rp := repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, QuotaGB: quotaGBFor(10)}
	mgr.Add(rp)                                             //nolint:errcheck
	b.Put("maven-hosted/a.jar", strings.NewReader("hello")) //nolint:errcheck  (5 bytes)
	srv.walkBlobSizes()

	if got := srv.usedBytes("maven-hosted"); got != 5 {
		t.Fatalf("used after walk: got %d want 5", got)
	}
	// A completed-but-not-yet-walked write is reflected via the delta.
	srv.addQuotaDelta("maven-hosted", 6)
	if got := srv.usedBytes("maven-hosted"); got != 11 {
		t.Fatalf("used with in-flight delta: got %d want 11", got)
	}
	// The soft gate sees the overrun immediately, before any new walk.
	w, r := writeReq(t, "z")
	if !srv.quotaBlocks(w, r, rp) {
		t.Fatal("in-flight delta should push the gate over quota")
	}

	// After the bytes land on disk and the walk runs, the delta is reconciled and
	// usage stays correct (no double counting).
	b.Put("maven-hosted/b.jar", strings.NewReader("world!")) //nolint:errcheck  (6 bytes)
	srv.walkBlobSizes()
	if got := srv.usedBytes("maven-hosted"); got != 11 {
		t.Fatalf("used after reconcile: got %d want 11 (5+6, delta cleared)", got)
	}
}
