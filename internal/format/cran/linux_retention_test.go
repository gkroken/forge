package cran

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/ledger"
	"forge/internal/repo"
)

// linuxRetentionRepo holds lb 1.0 source, and lb binaries: noble 1.0 and 1.1,
// jammy 1.0. Uploads are dated by age (days ago); downloads by stamp.
func linuxRetentionRepo(t *testing.T) *format.Context {
	t.Helper()
	var reads int
	c := linuxCtx(t, &reads)
	if rw := cranServe(c, http.MethodPut, "src/contrib/lb_1.0.tar.gz", bytesReader(makeCRANPkg(t, "lb", "1.0"))); rw.Code != http.StatusCreated {
		t.Fatalf("source: %d", rw.Code)
	}
	for _, b := range []struct {
		platform, rver, ver string
		age                 int
	}{
		{"linux/noble-x86_64", "4.6", "1.0", 90},
		{"linux/noble-x86_64", "4.6", "1.1", 5},
		{"linux/jammy-x86_64", "4.6", "1.0", 90},
	} {
		sub := "bin/" + b.platform + "/contrib/" + b.rver + "/lb_" + b.ver + ".tar.gz"
		if _, err := c.Blob.Put(c.Key(sub), bytesReader([]byte("bin"))); err != nil {
			t.Fatal(err)
		}
		rec := pkgRecord{Package: "lb", Version: b.ver, UploadedAt: time.Now().AddDate(0, 0, -b.age)}
		c.Meta.PutJSON(New().binNS(c, b.platform, b.rver), "lb_"+b.ver, rec) //nolint:errcheck
	}
	return c
}

func exists(c *format.Context, sub string) bool {
	_, ok, _ := c.Blob.Stat(c.Key(sub))
	return ok
}

func runPolicy(t *testing.T, c *format.Context, p repo.CleanupPolicy) {
	t.Helper()
	reg := format.NewRegistry()
	reg.Register(New())
	if _, err := cleanup.Run(c.Repo, reg, &p, c.Blob, c.Meta); err != nil {
		t.Fatal(err)
	}
}

const (
	nobleOld = "bin/linux/noble-x86_64/contrib/4.6/lb_1.0.tar.gz"
	nobleNew = "bin/linux/noble-x86_64/contrib/4.6/lb_1.1.tar.gz"
	jammyOld = "bin/linux/jammy-x86_64/contrib/4.6/lb_1.0.tar.gz"
	srcOld   = "src/contrib/lb_1.0.tar.gz"
)

// Each Linux binary is judged by its own downloads: the noble 1.0 binary in
// use stays although the jammy build of the same version is evicted, and the
// source is never touched by binary eviction.
func TestLinuxRetention_LastDownloadedPerBinary(t *testing.T) {
	c := linuxRetentionRepo(t)
	cleanup.RecordDownload(c.Meta, c.Key(nobleOld))
	cleanup.RecordDownload(c.Meta, c.Key(srcOld))
	// A version that exists only as one unused binary.
	solo := "bin/linux/jammy-x86_64/contrib/4.6/solo_0.1.tar.gz"
	c.Blob.Put(c.Key(solo), bytesReader([]byte("bin"))) //nolint:errcheck
	c.Meta.PutJSON(New().binNS(c, "linux/jammy-x86_64", "4.6"), "solo_0.1",
		pkgRecord{Package: "solo", Version: "0.1", UploadedAt: time.Now().AddDate(0, 0, -90)}) //nolint:errcheck
	ledger.RecordAt(c.Meta, c.Repo.Name, "solo", "0.1", time.Now().AddDate(0, 0, -90))
	runPolicy(t, c, repo.CleanupPolicy{LastDownloadedDays: 30})

	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("solo", "0.1")]; ok || exists(c, solo) {
		t.Error("evicting the last artifact of a version left its ledger row (or the binary)")
	}

	if exists(c, jammyOld) {
		t.Error("unused jammy binary was kept")
	}
	if !exists(c, nobleOld) {
		t.Error("the noble binary in use was evicted with its unused jammy sibling")
	}
	if !exists(c, nobleNew) {
		t.Error("a binary uploaded 5 days ago was evicted")
	}
	if !exists(c, srcOld) {
		t.Error("binary eviction removed the source")
	}
	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("lb", "1.0")]; !ok {
		t.Error("ledger row for lb 1.0 dropped while its source and noble binary remain")
	}
	idx := cranServe(c, http.MethodGet, "bin/linux/jammy-x86_64/contrib/4.6/PACKAGES", nil).Body.String()
	if idx != "" {
		t.Errorf("evicted binary still indexed:\n%s", idx)
	}
}

// keepVersions counts per platform: noble keeps its newest, jammy its only one.
func TestLinuxRetention_KeepVersionsPerPlatform(t *testing.T) {
	c := linuxRetentionRepo(t)
	// A newer source version makes source 1.0 an eviction candidate too; it
	// goes on its own, without jammy's 1.0 binary.
	if rw := cranServe(c, http.MethodPut, "src/contrib/lb_1.1.tar.gz", bytesReader(makeCRANPkg(t, "lb", "1.1"))); rw.Code != http.StatusCreated {
		t.Fatalf("source 1.1: %d", rw.Code)
	}
	runPolicy(t, c, repo.CleanupPolicy{KeepVersions: 1})
	if exists(c, srcOld) {
		t.Error("source 1.0 was kept")
	}
	if exists(c, nobleOld) || !exists(c, nobleNew) {
		t.Errorf("noble: old kept=%v new kept=%v, want only the newest", exists(c, nobleOld), exists(c, nobleNew))
	}
	if !exists(c, jammyOld) {
		t.Error("jammy's only binary was counted against noble's versions")
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
