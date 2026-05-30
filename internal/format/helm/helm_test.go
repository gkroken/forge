package helm

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/format"
	"forge/internal/golden"
	"forge/internal/meta"
	"forge/internal/repo"
)

var fixedNow = time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

func TestBuildIndex_Golden(t *testing.T) {
	recs := []chartRecord{
		{
			Name: "webapp", Version: "0.4.1", AppVersion: "2.0",
			Description: "A demo web application chart",
			Digest:      "aaabbbccc", Created: "2024-01-15T10:00:00Z",
			Filename: "webapp-0.4.1.tgz",
		},
		{
			Name: "webapp", Version: "0.3.0",
			Description: "A demo web application chart",
			Digest:      "dddeeefff", Created: "2024-01-14T09:00:00Z",
			Filename: "webapp-0.3.0.tgz",
		},
		{
			Name: "redis", Version: "1.0.0",
			Description: "Redis chart",
			Digest:      "111222333", Created: "2024-01-13T08:00:00Z",
			Filename: "redis-1.0.0.tgz",
		},
	}
	got := []byte(buildIndex(recs, fixedNow))
	golden.Assert(t, got, "index_two_charts.yaml")
}

func TestBuildIndex_Empty(t *testing.T) {
	got := buildIndex(nil, fixedNow)
	want := "apiVersion: v1\nentries:\ngenerated: 2024-01-15T12:00:00Z\n"
	if got != want {
		t.Fatalf("empty index mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestGroup_IndexMerge verifies that a group repo merges chart records from
// all members and deduplicates overlapping name+version pairs.
func TestGroup_IndexMerge(t *testing.T) {
	dir := t.TempDir()
	m, _ := meta.NewFS(filepath.Join(dir, "m"))

	seed := func(repoName, chartName, version string) {
		ns := repoName + ":helm"
		rec := chartRecord{
			Name: chartName, Version: version,
			Digest: "abc", Created: "2024-01-01T00:00:00Z",
			Filename: chartName + "-" + version + ".tgz",
		}
		if err := m.PutJSON(ns, chartName+"-"+version, rec); err != nil {
			t.Fatal(err)
		}
	}

	seed("helm-a", "webapp", "1.0.0")
	seed("helm-a", "webapp", "1.1.0")
	seed("helm-b", "webapp", "1.1.0") // duplicate
	seed("helm-b", "redis", "2.0.0")

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "helm-a", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-b", Format: "helm", Kind: repo.Hosted},
		{Name: "helm-group", Format: "helm", Kind: repo.Group, Members: []string{"helm-a", "helm-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	// Need a blob store for the Context even though groupRecords only reads meta.
	blobDir := filepath.Join(dir, "b")
	groupRepo, _ := mgr.Get("helm-group")
	// Use a nil blob store — groupRecords only touches meta.
	c := &format.Context{
		Repo:  groupRepo,
		Meta:  m,
		Sub:   "index.yaml",
		Repos: mgr,
	}

	h := New()
	recs := h.groupRecords(c)

	// Should have: webapp 1.0.0, webapp 1.1.0 (deduped), redis 2.0.0 = 3 total.
	if len(recs) != 3 {
		t.Errorf("expected 3 records, got %d", len(recs))
	}
	found := map[string]bool{}
	for _, r := range recs {
		found[r.Name+"-"+r.Version] = true
	}
	for _, key := range []string{"webapp-1.0.0", "webapp-1.1.0", "redis-2.0.0"} {
		if !found[key] {
			t.Errorf("expected record %s in merged index", key)
		}
	}

	// Verify via buildIndex output.
	idx := buildIndex(recs, fixedNow)
	if !strings.Contains(idx, "redis") {
		t.Error("index missing redis chart from second member")
	}
	_ = blobDir // silence unused warning
}
