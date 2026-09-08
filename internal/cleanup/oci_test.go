package cleanup_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/meta"
	"forge/internal/repo"
)

// pushImage writes the meta and blobs a real push would leave behind: a
// manifest listing a config and layers, the blobs themselves, a tag pointing at
// the manifest digest, and a tag-time.
func pushImage(t *testing.T, b blob.Store, m meta.Store, repoName, image, tag string, layers []string, pushed time.Time) string {
	t.Helper()
	manifest := map[string]any{
		"config": map[string]string{"digest": "sha256:cfg-" + image},
		"layers": func() []map[string]string {
			out := make([]map[string]string, 0, len(layers))
			for _, l := range layers {
				out = append(out, map[string]string{"digest": l})
			}
			return out
		}(),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dgst := fmt.Sprintf("sha256:mf-%s-%s", strings.ReplaceAll(image, "/", "_"), tag)
	if _, err := b.Put(repoName+"/manifests/"+dgst, strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(repoName+"/blobs/sha256:cfg-"+image, strings.NewReader("config")); err != nil {
		t.Fatal(err)
	}
	for _, l := range layers {
		if _, err := b.Put(repoName+"/blobs/"+l, strings.NewReader("layerdata-"+l)); err != nil {
			t.Fatal(err)
		}
	}
	ns := repoName + ":oci"
	if err := m.PutJSON(ns, "manifests/"+dgst, map[string]string{"imageName": image}); err != nil {
		t.Fatal(err)
	}
	if err := m.PutJSON(ns, "tags/"+image+"/"+tag, dgst); err != nil {
		t.Fatal(err)
	}
	if err := m.PutJSON(ns, "tag-times/"+image+"/"+tag, pushed.UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return dgst
}

func exists(t *testing.T, b blob.Store, key string) bool {
	t.Helper()
	_, ok, _ := b.Stat(key)
	return ok
}

// TestOCI_DeleteOlderThanDays — the headline gap: every retention policy used to
// be a no-op on image repositories, because cleanup had no oci case at all.
func TestOCI_DeleteOlderThanDays(t *testing.T) {
	b, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -60)
	pushImage(t, b, m, "docker", "acme/api", "v1", []string{"sha256:l1"}, old)

	res, err := cleanup.Run(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", res.Deleted)
	}
	if res.FreedBytes == 0 {
		t.Error("freed 0 bytes — the manifest and its blobs were not swept")
	}
	if exists(t, b, "docker/blobs/sha256:l1") {
		t.Error("orphaned layer survived the sweep")
	}
	if ok, _ := m.GetJSON("docker:oci", "tags/acme/api/v1", new(string)); ok {
		t.Error("tag record survived")
	}
}

// TestOCI_SharedLayerSurvives is the correctness property that makes the sweep
// safe: two tags sharing a layer, one deleted, the layer must remain because the
// surviving image still needs it.
func TestOCI_SharedLayerSurvives(t *testing.T) {
	b, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -60)
	recent := time.Now().UTC().AddDate(0, 0, -1)
	pushImage(t, b, m, "docker", "acme/api", "old", []string{"sha256:shared", "sha256:only-old"}, old)
	pushImage(t, b, m, "docker", "acme/api", "new", []string{"sha256:shared", "sha256:only-new"}, recent)

	res, err := cleanup.Run(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the old tag)", res.Deleted)
	}
	if !exists(t, b, "docker/blobs/sha256:shared") {
		t.Error("a layer still used by the surviving image was deleted — image is now broken")
	}
	if !exists(t, b, "docker/blobs/sha256:only-new") {
		t.Error("the surviving image's own layer was deleted")
	}
	if exists(t, b, "docker/blobs/sha256:only-old") {
		t.Error("the deleted image's exclusive layer was not reclaimed")
	}
}

// TestOCI_InFlightPushUntouched — docker uploads blobs before the manifest that
// references them. A registry-wide mark-and-sweep would treat those as garbage;
// this sweep only considers what a deletion orphaned, so they are safe.
func TestOCI_InFlightPushUntouched(t *testing.T) {
	b, m := stores(t)
	pushImage(t, b, m, "docker", "acme/api", "v1", []string{"sha256:l1"},
		time.Now().UTC().AddDate(0, 0, -60))
	// A concurrent push: layer uploaded, manifest not written yet.
	if _, err := b.Put("docker/blobs/sha256:inflight", strings.NewReader("half a push")); err != nil {
		t.Fatal(err)
	}

	if _, err := cleanup.Run(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m); err != nil {
		t.Fatal(err)
	}
	if !exists(t, b, "docker/blobs/sha256:inflight") {
		t.Fatal("an in-flight upload's layer was collected — the push would fail on manifest PUT")
	}
}

// TestOCI_KeepVersions — the non-age rules work on tags too.
func TestOCI_KeepVersions(t *testing.T) {
	b, m := stores(t)
	now := time.Now().UTC()
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0"} {
		pushImage(t, b, m, "docker", "acme/api", v, []string{"sha256:l-" + v}, now)
	}
	res, err := cleanup.Run(rp("docker", "oci"), formats(), &repo.CleanupPolicy{KeepVersions: 2}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 of 4 tags", res.Deleted)
	}
}

// TestOCI_DryRunDoesNotMutate — a preview must not touch blobs or meta.
func TestOCI_DryRunDoesNotMutate(t *testing.T) {
	b, m := stores(t)
	pushImage(t, b, m, "docker", "acme/api", "v1", []string{"sha256:l1"},
		time.Now().UTC().AddDate(0, 0, -60))

	res, err := cleanup.DryRun(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want 1", res.Candidates)
	}
	if got := res.Candidates[0]; got.Component != "acme/api" || got.Version != "v1" {
		t.Errorf("candidate identity wrong: %+v", got)
	}
	if !exists(t, b, "docker/blobs/sha256:l1") || !exists(t, b, "docker/manifests/sha256:mf-acme_api-v1") {
		t.Error("dry run deleted something")
	}
}

// TestOCI_ImageNameWithSlashesRoundTrips — "acme/api:v1" must not be split into
// image "acme" and tag "api/v1".
func TestOCI_ImageNameWithSlashesRoundTrips(t *testing.T) {
	b, m := stores(t)
	pushImage(t, b, m, "docker", "team/group/svc", "v2", []string{"sha256:l1"},
		time.Now().UTC().AddDate(0, 0, -60))
	res, err := cleanup.DryRun(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Component != "team/group/svc" ||
		res.Candidates[0].Version != "v2" {
		t.Fatalf("nested image name mis-parsed: %+v", res.Candidates)
	}
}

// TestOCI_UnevaluableReported — a tag with no known push time is reported, not
// silently skipped.
func TestOCI_UnevaluableReported(t *testing.T) {
	b, m := stores(t)
	pushImage(t, b, m, "docker", "acme/api", "v1", []string{"sha256:l1"}, time.Time{})
	m.Delete("docker:oci", "tag-times/acme/api/v1") //nolint:errcheck

	res, err := cleanup.DryRun(rp("docker", "oci"), formats(), &repo.CleanupPolicy{DeleteOlderThanDays: 30}, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("must not delete an undatable tag: %+v", res.Candidates)
	}
	if len(res.Unevaluable) != 1 {
		t.Fatalf("Unevaluable = %+v, want 1", res.Unevaluable)
	}
}

// TestOCI_DryRunSizeMatchesRealRun — a dry run exists to answer "how much will
// this reclaim". Reporting the manifest alone would answer in kilobytes for a
// deletion that frees hundreds of megabytes of layers.
func TestOCI_DryRunSizeMatchesRealRun(t *testing.T) {
	b, m := stores(t)
	pushImage(t, b, m, "docker", "acme/api", "v1",
		[]string{"sha256:l1", "sha256:l2"}, time.Now().UTC().AddDate(0, 0, -60))
	pol := &repo.CleanupPolicy{DeleteOlderThanDays: 30}

	dr, err := cleanup.DryRun(rp("docker", "oci"), formats(), pol, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(dr.Candidates) != 1 {
		t.Fatalf("candidates = %+v", dr.Candidates)
	}
	predicted := dr.Candidates[0].SizeBytes

	res, err := cleanup.Run(rp("docker", "oci"), formats(), pol, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if predicted != res.FreedBytes {
		t.Errorf("dry run predicted %d bytes, real run freed %d", predicted, res.FreedBytes)
	}
}

// TestOCI_DryRunSizeExcludesSharedLayers — a layer another tag still needs is
// not freed by deleting this one, so it must not be counted. Over-promising
// reclaimable space is worse than under-promising it.
func TestOCI_DryRunSizeExcludesSharedLayers(t *testing.T) {
	b, m := stores(t)
	old := time.Now().UTC().AddDate(0, 0, -60)
	recent := time.Now().UTC().AddDate(0, 0, -1)
	pushImage(t, b, m, "docker", "acme/api", "old", []string{"sha256:shared", "sha256:only-old"}, old)
	pushImage(t, b, m, "docker", "acme/api", "new", []string{"sha256:shared", "sha256:only-new"}, recent)
	pol := &repo.CleanupPolicy{DeleteOlderThanDays: 30}

	dr, err := cleanup.DryRun(rp("docker", "oci"), formats(), pol, b, m)
	if err != nil {
		t.Fatal(err)
	}
	var predicted int64
	for _, c := range dr.Candidates {
		predicted += c.SizeBytes
	}
	res, err := cleanup.Run(rp("docker", "oci"), formats(), pol, b, m)
	if err != nil {
		t.Fatal(err)
	}
	if predicted != res.FreedBytes {
		t.Errorf("predicted %d bytes, freed %d — the shared layer was mis-counted",
			predicted, res.FreedBytes)
	}
	if !exists(t, b, "docker/blobs/sha256:shared") {
		t.Error("shared layer was deleted")
	}
}
