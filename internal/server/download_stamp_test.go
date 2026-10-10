package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

func stamped(t *testing.T, s *Server, key string) bool {
	t.Helper()
	var rec struct {
		DownloadedAt time.Time `json:"downloadedAt"`
	}
	for i := 0; i < 50; i++ { // RecordDownload is async
		if ok, _ := s.Meta.GetJSON("download-times", key, &rec); ok {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Last-downloaded retention runs on a repository's own blobs. A download
// through a group used to be stamped under the group's path — a key no blob
// has — so an artifact everyone installed through the group looked
// never-downloaded to its repository's retention, and was evicted.
func TestDownloadStamp_CreditsTheBlobServed(t *testing.T) {
	s := newMigrationServer(t)
	s.Cleanup = cleanup.NewPolicyManager(s.Meta)
	addHosted(t, s, "cran-a", "cran")
	if err := s.Repos.Add(repo.Repository{Name: "cran-g", Format: "cran", Kind: repo.Group, Enabled: true, Members: []string{"cran-a"}}); err != nil {
		t.Fatal(err)
	}
	seedBlobBytes(t, s, http.MethodPut, "cran-a", "src/contrib/p_1.0.tar.gz", tgzWithFile(t, "p/DESCRIPTION", "Package: p\nVersion: 1.0\n"))
	seedBlobBytes(t, s, http.MethodPut, "cran-a", "src/contrib/q_1.0.tar.gz", tgzWithFile(t, "q/DESCRIPTION", "Package: q\nVersion: 1.0\n"))
	get := func(method, path string) {
		rw := httptest.NewRecorder()
		s.Routes().ServeHTTP(rw, httptest.NewRequest(method, path, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("%s %s: %d", method, path, rw.Code)
		}
	}

	get(http.MethodGet, "/repository/cran-g/src/contrib/p_1.0.tar.gz")
	if !stamped(t, s, "cran-a/src/contrib/p_1.0.tar.gz") {
		t.Error("a download through the group did not credit the member blob")
	}
	if ok, _ := s.Meta.GetJSON("download-times", "cran-g/src/contrib/p_1.0.tar.gz", &struct{}{}); ok {
		t.Error("stamped the group path, which names no blob")
	}
	get(http.MethodGet, "/repository/cran-a/src/contrib/q_1.0.tar.gz")
	if !stamped(t, s, "cran-a/src/contrib/q_1.0.tar.gz") {
		t.Error("a direct download was not stamped")
	}
}

func TestDownloadStamp_HeadIsNotADownload(t *testing.T) {
	s := newMigrationServer(t)
	s.Cleanup = cleanup.NewPolicyManager(s.Meta)
	addHosted(t, s, "cran-a", "cran")
	seedBlobBytes(t, s, http.MethodPut, "cran-a", "src/contrib/p_1.0.tar.gz", tgzWithFile(t, "p/DESCRIPTION", "Package: p\nVersion: 1.0\n"))
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodHead, "/repository/cran-a/src/contrib/p_1.0.tar.gz", nil))
	if stamped(t, s, "cran-a/src/contrib/p_1.0.tar.gz") {
		t.Error("a HEAD was recorded as a download")
	}
}
