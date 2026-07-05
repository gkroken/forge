package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"forge/internal/repo"
)

func addHosted(t *testing.T, s *Server, name, format string) {
	t.Helper()
	if err := s.Repos.Add(repo.Repository{Name: name, Format: format, Kind: repo.Hosted, Enabled: true}); err != nil {
		t.Fatalf("add repo %s: %v", name, err)
	}
}

func setImmutable(t *testing.T, s *Server, name string) {
	t.Helper()
	rp, _ := s.Repos.Get(name)
	rp.Immutable = true
	if err := s.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}
}

func seedBlobBytes(t *testing.T, s *Server, method, repoName, sub string, body []byte) {
	t.Helper()
	rec := s.internalServe(context.Background(), method, repoName, sub, "", bytes.NewReader(body), nil, "http://localhost")
	if !rec.ok() {
		t.Fatalf("seed %s %s/%s: %d %s", method, repoName, sub, rec.code, rec.body.String())
	}
}

func targetBlob(t *testing.T, s *Server, key string) []byte {
	t.Helper()
	rc, err := s.Blob.Get(key)
	if err != nil {
		t.Fatalf("target blob %s missing: %v", key, err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return data
}

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestPromoteMaven_ByteIdenticalWithProvenance(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "mvn-stage", "maven")
	addHosted(t, s, "mvn-prod", "maven")

	jar := []byte("PK\x03\x04 fake jar bytes for app 1.0")
	pom := []byte("<project><artifactId>app</artifactId></project>")
	seedBlobBytes(t, s, http.MethodPut, "mvn-stage", "com/acme/app/1.0/app-1.0.jar", jar)
	seedBlobBytes(t, s, http.MethodPut, "mvn-stage", "com/acme/app/1.0/app-1.0.pom", pom)

	src, _ := s.Repos.Get("mvn-stage")
	tgt, _ := s.Repos.Get("mvn-prod")
	rec, copied, err := s.promoteComponent(context.Background(), "tester", src, tgt, "com.acme:app", "1.0", "http://localhost")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Byte-for-byte copy of the primary artifact.
	if got := targetBlob(t, s, "mvn-prod/com/acme/app/1.0/app-1.0.jar"); !bytes.Equal(got, jar) {
		t.Fatalf("jar not copied byte-identically")
	}
	if got := targetBlob(t, s, "mvn-prod/com/acme/app/1.0/app-1.0.pom"); !bytes.Equal(got, pom) {
		t.Fatalf("pom not copied")
	}
	if copied != int64(len(jar)+len(pom)) {
		t.Fatalf("copied bytes = %d, want %d", copied, len(jar)+len(pom))
	}
	// Provenance digest is the primary (jar) sha256, recorded and retrievable.
	if rec.SourceDigest != sha256hex(jar) {
		t.Fatalf("provenance digest = %s, want jar sha %s", rec.SourceDigest, sha256hex(jar))
	}
	got, ok := s.GetProvenance("mvn-prod", "com.acme:app", "1.0")
	if !ok || got.SourceRepo != "mvn-stage" || got.PromotedBy != "tester" {
		t.Fatalf("provenance not stored correctly: %+v ok=%v", got, ok)
	}
}

func npmPublishBody(pkg, version string, tarball []byte) []byte {
	fname := pkg + "-" + version + ".tgz"
	doc := map[string]any{
		"_id":      pkg,
		"name":     pkg,
		"versions": map[string]any{version: map[string]any{"name": pkg, "version": version}},
		"_attachments": map[string]any{
			fname: map[string]string{"data": base64.StdEncoding.EncodeToString(tarball)},
		},
		"dist-tags": map[string]string{"latest": version},
	}
	b, _ := json.Marshal(doc)
	return b
}

func TestPromoteNPM_CopiesTarballAndRecord(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-stage", "npm")
	addHosted(t, s, "npm-prod", "npm")

	tarball := []byte("fake npm tarball gzip bytes")
	seedBlobBytes(t, s, http.MethodPut, "npm-stage", "leftpad", npmPublishBody("leftpad", "1.2.3", tarball))

	src, _ := s.Repos.Get("npm-stage")
	tgt, _ := s.Repos.Get("npm-prod")
	rec, _, err := s.promoteComponent(context.Background(), "tester", src, tgt, "leftpad", "1.2.3", "http://localhost")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if got := targetBlob(t, s, "npm-prod/leftpad/-/leftpad-1.2.3.tgz"); !bytes.Equal(got, tarball) {
		t.Fatalf("tarball not copied byte-identically")
	}
	// The per-version record exists on the target (packument regenerated).
	var vobj map[string]any
	if ok, _ := s.Meta.GetJSON("npm-prod:npm:v", "leftpad:1.2.3", &vobj); !ok {
		t.Fatalf("target version record missing")
	}
	if rec.SourceDigest != sha256hex(tarball) {
		t.Fatalf("provenance digest wrong")
	}
}

func TestPromote_ImmutableTargetRejectsOverwrite(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "cran-stage", "cran")
	addHosted(t, s, "cran-prod", "cran")

	body := tgzWithFile(t, "mypkg/DESCRIPTION", "Package: mypkg\nVersion: 1.0\nLicense: MIT\n")
	seedBlobBytes(t, s, http.MethodPut, "cran-stage", "src/contrib/mypkg_1.0.tar.gz", body)

	src, _ := s.Repos.Get("cran-stage")
	tgt, _ := s.Repos.Get("cran-prod")
	// First promote succeeds.
	if _, _, err := s.promoteComponent(context.Background(), "t", src, tgt, "mypkg", "1.0", "http://localhost"); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	// Make the target immutable; a re-promote of the same version is a 409.
	setImmutable(t, s, "cran-prod")
	tgt, _ = s.Repos.Get("cran-prod")
	_, _, err := s.promoteComponent(context.Background(), "t", src, tgt, "mypkg", "1.0", "http://localhost")
	pe, ok := err.(*promoteError)
	if !ok || pe.Status != http.StatusConflict {
		t.Fatalf("want 409 promoteError, got %v", err)
	}
}

func TestPromote_TargetQuotaBlocks(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "mvn-stage", "maven")
	addHosted(t, s, "mvn-prod", "maven")
	// Tiny quota on the target.
	tgt, _ := s.Repos.Get("mvn-prod")
	q := 0.0000000001 // ~0 bytes
	tgt.QuotaGB = &q
	s.Repos.Update(tgt)

	seedBlobBytes(t, s, http.MethodPut, "mvn-stage", "com/acme/app/1.0/app-1.0.jar", []byte("some bytes over quota"))
	src, _ := s.Repos.Get("mvn-stage")
	tgt, _ = s.Repos.Get("mvn-prod")
	_, _, err := s.promoteComponent(context.Background(), "t", src, tgt, "com.acme:app", "1.0", "http://localhost")
	pe, ok := err.(*promoteError)
	if !ok || pe.Status != http.StatusInsufficientStorage {
		t.Fatalf("want 507 promoteError, got %v", err)
	}
}

func TestPromote_Errors(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "a", "maven")
	addHosted(t, s, "b", "npm")
	addHosted(t, s, "c", "maven")

	a, _ := s.Repos.Get("a")
	b, _ := s.Repos.Get("b")
	c, _ := s.Repos.Get("c")

	// format mismatch → 400
	if _, _, err := s.promoteComponent(context.Background(), "t", a, b, "x:y", "1", "h"); err == nil || err.(*promoteError).Status != http.StatusBadRequest {
		t.Fatalf("format mismatch: want 400, got %v", err)
	}
	// missing source component → 404
	if _, _, err := s.promoteComponent(context.Background(), "t", a, c, "com.x:y", "1", "h"); err == nil || err.(*promoteError).Status != http.StatusNotFound {
		t.Fatalf("missing source: want 404, got %v", err)
	}
	// same repo → 400
	if _, _, err := s.promoteComponent(context.Background(), "t", a, a, "com.x:y", "1", "h"); err == nil || err.(*promoteError).Status != http.StatusBadRequest {
		t.Fatalf("same repo: want 400, got %v", err)
	}
}

func TestPromote_HTTPHandler(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "helm-stage", "helm")
	addHosted(t, s, "helm-prod", "helm")

	chart := makeChartTGZ(t, "web", "2.0.0")
	seedBlobBytes(t, s, http.MethodPost, "helm-stage", "api/charts", chart)

	body, _ := json.Marshal(map[string]string{"sourceRepo": "helm-stage", "component": "web", "version": "2.0.0"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/helm-prod/promote", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	s.handleAdminRepos(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("promote HTTP: %d %s", rw.Code, rw.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rw.Body.Bytes(), &resp)
	if resp["promoted"] != true {
		t.Fatalf("response not promoted: %v", resp)
	}
	// The chart is downloadable from the target.
	if _, ok, _ := s.Blob.Stat("helm-prod/web-2.0.0.tgz"); !ok {
		t.Fatalf("promoted chart not stored on target")
	}
}
