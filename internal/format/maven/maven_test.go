package maven

import (
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/golden"
	"forge/internal/meta"
	"forge/internal/repo"
)

// ctxWith builds a Context backed by temp FS stores, pre-seeded with blob keys.
func ctxWith(t *testing.T, sub string, blobKeys ...string) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	for _, k := range blobKeys {
		if _, err := b.Put(k, strings.NewReader("artifact-bytes")); err != nil {
			t.Fatal(err)
		}
	}
	return &format.Context{
		Repo: repo.Repository{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
		Blob: b, Meta: m, Sub: sub,
	}
}

// sharedStores returns a shared blob+meta store pair backed by the same temp dir.
func sharedStores(t *testing.T) (blob.Store, meta.Store) {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return b, m
}

func TestGenerateMetadata_Golden(t *testing.T) {
	c := ctxWith(t, "com/acme/lib/maven-metadata.xml",
		"maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar",
		"maven-hosted/com/acme/lib/1.3.0/lib-1.3.0.jar",
	)
	got, ok := New().generateMetadata(c)
	if !ok {
		t.Fatal("expected metadata to generate")
	}
	golden.Assert(t, got, "metadata_two_versions.xml")
}

func TestGenerateMetadata_EmptyArtifact(t *testing.T) {
	c := ctxWith(t, "com/acme/none/maven-metadata.xml") // no blobs
	if _, ok := New().generateMetadata(c); ok {
		t.Fatal("expected no metadata for absent artifact")
	}
}

// A requested .sha1 sidecar with no stored file is synthesized from the base
// artifact; verify it matches a direct checksum of the same bytes.
func TestChecksumHelpersMatch(t *testing.T) {
	body := []byte("artifact-bytes")
	if blob.SHA1(body) != checksumOf("sha1", body) {
		t.Fatal("sha1 helper mismatch")
	}
	if blob.MD5(body) != checksumOf("md5", body) {
		t.Fatal("md5 helper mismatch")
	}
	if blob.SHA256(body) != checksumOf("sha256", body) {
		t.Fatal("sha256 helper mismatch")
	}
}

// TestGroup_MetadataMerge verifies that a group repo merges versions from all
// member repos and deduplicates overlapping versions.
func TestGroup_MetadataMerge(t *testing.T) {
	b, m := sharedStores(t)

	seed := func(repoName, version string) {
		key := repoName + "/com/acme/lib/" + version + "/lib-" + version + ".jar"
		if _, err := b.Put(key, strings.NewReader("bytes")); err != nil {
			t.Fatal(err)
		}
	}
	seed("maven-a", "1.0.0")
	seed("maven-a", "1.1.0")
	seed("maven-b", "1.1.0") // duplicate; group should deduplicate
	seed("maven-b", "2.0.0")

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "maven-a", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-b", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-group", Format: "maven", Kind: repo.Group, Members: []string{"maven-a", "maven-b"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}

	groupRepo, _ := mgr.Get("maven-group")
	c := &format.Context{
		Repo:  groupRepo,
		Blob:  b,
		Meta:  m,
		Sub:   "com/acme/lib/maven-metadata.xml",
		Repos: mgr,
	}

	xml, ok := New().groupMetadataBytes(c)
	if !ok {
		t.Fatal("expected group metadata to generate")
	}
	body := string(xml)
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if !strings.Contains(body, "<version>"+v+"</version>") {
			t.Errorf("expected version %s in merged metadata", v)
		}
	}
	// 1.1.0 should appear exactly once (deduplicated).
	if count := strings.Count(body, "<version>1.1.0</version>"); count != 1 {
		t.Errorf("1.1.0 appears %d times, want 1", count)
	}
}
