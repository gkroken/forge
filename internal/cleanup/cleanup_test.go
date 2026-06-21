package cleanup_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/meta"
	"forge/internal/repo"
)

func stores(t *testing.T) (blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func putBlob(t *testing.T, b blob.Store, key string) {
	t.Helper()
	if _, err := b.Put(key, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
}

// ── CRAN ─────────────────────────────────────────────────────────────────────

type cranRec struct {
	Package    string    `json:"package"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func seedCRAN(t *testing.T, b blob.Store, m meta.Store, repoName string, recs []cranRec) {
	t.Helper()
	ns := repoName + ":cran"
	for _, r := range recs {
		putBlob(t, b, repoName+"/src/contrib/"+r.Package+"_"+r.Version+".tar.gz")
		if err := m.PutJSON(ns, r.Package+"_"+r.Version, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCRAN_NoPolicy(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "ggplot2", Version: "3.0.0"},
		{Package: "ggplot2", Version: "3.1.0"},
	})
	res, err := cleanup.Run("cran", "cran", nil, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 {
		t.Fatalf("expected 0 deletions without a policy, got %d", res.Deleted)
	}
}

func TestCRAN_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "ggplot2", Version: "1.0.0"},
		{Package: "ggplot2", Version: "2.0.0"},
		{Package: "ggplot2", Version: "3.0.0"},
	})
	res, err := cleanup.Run("cran", "cran", &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (keep 2 of 3), got %d", res.Deleted)
	}
	// Oldest version should be gone.
	_, exists, _ := b.Stat("cran/src/contrib/ggplot2_1.0.0.tar.gz")
	if exists {
		t.Fatal("expected ggplot2_1.0.0 to be deleted")
	}
	// Newest two should remain.
	for _, v := range []string{"2.0.0", "3.0.0"} {
		_, exists, _ := b.Stat("cran/src/contrib/ggplot2_" + v + ".tar.gz")
		if !exists {
			t.Fatalf("expected ggplot2_%s to be kept", v)
		}
	}
}

// TestCRAN_KeepVersions_SemverSort guards the lexicographic-sort regression:
// keep-2 over {1.8.0, 1.9.0, 1.10.0} must keep the two numerically-highest
// (1.9.0, 1.10.0) and delete 1.8.0. The old string sort kept 1.8.0+1.9.0 and
// deleted 1.10.0 — the just-published version.
func TestCRAN_KeepVersions_SemverSort(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "data.table", Version: "1.8.0"},
		{Package: "data.table", Version: "1.9.0"},
		{Package: "data.table", Version: "1.10.0"},
	})
	res, err := cleanup.Run("cran", "cran", &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (keep 2 of 3), got %d", res.Deleted)
	}
	if _, exists, _ := b.Stat("cran/src/contrib/data.table_1.8.0.tar.gz"); exists {
		t.Fatal("expected data.table_1.8.0 (lowest) to be deleted")
	}
	for _, v := range []string{"1.9.0", "1.10.0"} {
		if _, exists, _ := b.Stat("cran/src/contrib/data.table_" + v + ".tar.gz"); !exists {
			t.Fatalf("expected data.table_%s to be kept", v)
		}
	}
}

func TestCRAN_DeleteOlderThanDays(t *testing.T) {
	b, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -60)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "dplyr", Version: "1.0.0", UploadedAt: old},
		{Package: "dplyr", Version: "2.0.0"}, // no timestamp → skipped
		{Package: "dplyr", Version: "3.0.0", UploadedAt: time.Now().UTC()},
	})
	res, err := cleanup.Run("cran", "cran", &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (old timestamped version), got %d", res.Deleted)
	}
	_, exists, _ := b.Stat("cran/src/contrib/dplyr_1.0.0.tar.gz")
	if exists {
		t.Fatal("expected dplyr_1.0.0 to be deleted")
	}
}

// ── Helm ─────────────────────────────────────────────────────────────────────

type helmRec struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Filename   string    `json:"filename"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func seedHelm(t *testing.T, b blob.Store, m meta.Store, repoName string, recs []helmRec) {
	t.Helper()
	ns := repoName + ":helm"
	for _, r := range recs {
		filename := r.Name + "-" + r.Version + ".tgz"
		putBlob(t, b, repoName+"/"+filename)
		r.Filename = filename
		if err := m.PutJSON(ns, r.Name+"-"+r.Version, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHelm_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedHelm(t, b, m, "helm", []helmRec{
		{Name: "myapp", Version: "0.1.0"},
		{Name: "myapp", Version: "0.2.0"},
		{Name: "myapp", Version: "0.3.0"},
	})
	res, err := cleanup.Run("helm", "helm", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 {
		t.Fatalf("expected 2 deletions (keep 1 of 3), got %d", res.Deleted)
	}
	// Only the newest should remain.
	_, exists, _ := b.Stat("helm/myapp-0.3.0.tgz")
	if !exists {
		t.Fatal("expected myapp-0.3.0 to be kept")
	}
}

// ── Maven ─────────────────────────────────────────────────────────────────────

func TestMaven_KeepReleasesOnly(t *testing.T) {
	b, m := stores(t)
	// Seed two versions: one release, one SNAPSHOT.
	putBlob(t, b, "mvn/com/acme/lib/1.0.0/lib-1.0.0.jar")
	putBlob(t, b, "mvn/com/acme/lib/2.0.0-SNAPSHOT/lib-2.0.0-SNAPSHOT.jar")

	res, err := cleanup.Run("mvn", "maven", &repo.CleanupPolicy{KeepReleasesOnly: true}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (SNAPSHOT), got %d", res.Deleted)
	}
	_, exists, _ := b.Stat("mvn/com/acme/lib/2.0.0-SNAPSHOT/lib-2.0.0-SNAPSHOT.jar")
	if exists {
		t.Fatal("expected SNAPSHOT to be deleted")
	}
	_, exists, _ = b.Stat("mvn/com/acme/lib/1.0.0/lib-1.0.0.jar")
	if !exists {
		t.Fatal("expected release to be kept")
	}
}

// ── npm ───────────────────────────────────────────────────────────────────────

type npmVerRec struct {
	Package    string    `json:"name"`
	Version    string    `json:"version"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
}

func seedNPM(t *testing.T, b blob.Store, m meta.Store, repoName string, pkg string, versions []npmVerRec) {
	t.Helper()
	versNS := repoName + ":npm:v"
	pkgNS := repoName + ":npm"
	packument := map[string]any{
		"name":     pkg,
		"versions": map[string]any{},
	}
	for _, v := range versions {
		blobKey := repoName + "/" + pkg + "/-/" + pkg + "-" + v.Version + ".tgz"
		putBlob(t, b, blobKey)
		if err := m.PutJSON(versNS, pkg+":"+v.Version, v); err != nil {
			t.Fatal(err)
		}
		packument["versions"].(map[string]any)[v.Version] = map[string]any{}
	}
	if err := m.PutJSON(pkgNS, pkg, packument); err != nil {
		t.Fatal(err)
	}
}

func TestNPM_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedNPM(t, b, m, "npm", "lodash", []npmVerRec{
		{Package: "lodash", Version: "1.0.0"},
		{Package: "lodash", Version: "2.0.0"},
		{Package: "lodash", Version: "3.0.0"},
	})
	res, err := cleanup.Run("npm", "npm", &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deletion (keep 2 of 3), got %d", res.Deleted)
	}
	_, exists, _ := b.Stat("npm/lodash/-/lodash-1.0.0.tgz")
	if exists {
		t.Fatal("expected lodash@1.0.0 to be deleted")
	}
}

func TestNPM_KeepReleasesOnly(t *testing.T) {
	b, m := stores(t)
	seedNPM(t, b, m, "npm", "react", []npmVerRec{
		{Package: "react", Version: "18.0.0"},
		{Package: "react", Version: "19.0.0-beta"},
		{Package: "react", Version: "19.0.0-rc"},
	})
	res, err := cleanup.Run("npm", "npm", &repo.CleanupPolicy{KeepReleasesOnly: true}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 {
		t.Fatalf("expected 2 deletions (pre-releases), got %d", res.Deleted)
	}
	_, exists, _ := b.Stat("npm/react/-/react-18.0.0.tgz")
	if !exists {
		t.Fatal("expected react@18.0.0 release to be kept")
	}
}
