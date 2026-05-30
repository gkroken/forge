package maven

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

var update = flag.Bool("update", false, "update golden files")

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

func TestGenerateMetadata_Golden(t *testing.T) {
	c := ctxWith(t, "com/acme/lib/maven-metadata.xml",
		"maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar",
		"maven-hosted/com/acme/lib/1.3.0/lib-1.3.0.jar",
	)
	got, ok := New().generateMetadata(c)
	if !ok {
		t.Fatal("expected metadata to generate")
	}
	goldenPath := filepath.Join("testdata", "metadata_two_versions.xml")
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(goldenPath, got, 0o644)
		t.Log("updated golden")
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update first): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("metadata mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
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
