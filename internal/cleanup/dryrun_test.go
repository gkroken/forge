package cleanup_test

import (
	"testing"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

func TestDryRun_NoPolicy(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "ggplot2", Version: "1.0.0"},
	})
	result, err := cleanup.DryRun("cran", "cran", nil, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("expected no candidates without a policy, got %d", len(result.Candidates))
	}
}

func TestDryRun_CRAN_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "ggplot2", Version: "1.0.0"},
		{Package: "ggplot2", Version: "2.0.0"},
		{Package: "ggplot2", Version: "3.0.0"},
	})
	result, err := cleanup.DryRun("cran", "cran", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("expected 2 candidates (keep 1 of 3), got %d", len(result.Candidates))
	}
	// Dry-run must not delete anything.
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		_, exists, _ := b.Stat("cran/src/contrib/ggplot2_" + v + ".tar.gz")
		if !exists {
			t.Errorf("dry-run deleted ggplot2_%s — should not have", v)
		}
	}
	for _, c := range result.Candidates {
		if c.Reason == "" {
			t.Errorf("candidate missing reason: %+v", c)
		}
		if c.Component == "" || c.Version == "" {
			t.Errorf("candidate missing component/version: %+v", c)
		}
	}
}

func TestDryRun_CRAN_KeepReleasesOnly(t *testing.T) {
	b, m := stores(t)
	seedCRAN(t, b, m, "cran", []cranRec{
		{Package: "pkg", Version: "1.0.0"},
		{Package: "pkg", Version: "2.0.0-beta"},
	})
	result, err := cleanup.DryRun("cran", "cran", &repo.CleanupPolicy{KeepReleasesOnly: true}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate (pre-release), got %d", len(result.Candidates))
	}
	if result.Candidates[0].Reason != "keep_releases_only" {
		t.Errorf("unexpected reason: %s", result.Candidates[0].Reason)
	}
}

func TestDryRun_Helm_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedHelm(t, b, m, "helm", []helmRec{
		{Name: "app", Version: "0.1.0"},
		{Name: "app", Version: "0.2.0"},
	})
	result, err := cleanup.DryRun("helm", "helm", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(result.Candidates))
	}
	// Blobs must be intact.
	for _, v := range []string{"0.1.0", "0.2.0"} {
		_, exists, _ := b.Stat("helm/app-" + v + ".tgz")
		if !exists {
			t.Errorf("dry-run deleted helm/app-%s — should not have", v)
		}
	}
}

func TestDryRun_NPM_KeepVersions(t *testing.T) {
	b, m := stores(t)
	seedNPM(t, b, m, "npm", "express", []npmVerRec{
		{Package: "express", Version: "4.0.0"},
		{Package: "express", Version: "5.0.0"},
	})
	result, err := cleanup.DryRun("npm", "npm", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(result.Candidates))
	}
	// Blobs must be intact.
	for _, v := range []string{"4.0.0", "5.0.0"} {
		_, exists, _ := b.Stat("npm/express/-/express-" + v + ".tgz")
		if !exists {
			t.Errorf("dry-run deleted express@%s — should not have", v)
		}
	}
}

func TestDryRun_Maven_KeepVersions(t *testing.T) {
	b, m := stores(t)
	putBlob(t, b, "mvn/com/acme/lib/1.0.0/lib-1.0.0.jar")
	putBlob(t, b, "mvn/com/acme/lib/2.0.0/lib-2.0.0.jar")

	result, err := cleanup.DryRun("mvn", "maven", &repo.CleanupPolicy{KeepVersions: 1}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(result.Candidates))
	}
	// Blobs must be intact.
	for _, v := range []string{"1.0.0", "2.0.0"} {
		_, exists, _ := b.Stat("mvn/com/acme/lib/" + v + "/lib-" + v + ".jar")
		if !exists {
			t.Errorf("dry-run deleted maven artifact %s — should not have", v)
		}
	}
}
