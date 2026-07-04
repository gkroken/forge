package maven

import (
	"net/http"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/integrity"
)

// kinds returns the finding kinds present in a result, deduplicated.
func kinds(res integrity.Result) map[string]int {
	m := map[string]int{}
	for _, f := range res.Findings {
		m[f.Kind]++
	}
	return m
}

func TestVerifyIntegrity_CleanHosted(t *testing.T) {
	c := ctxWith(t, "")
	body := "artifact-bytes"
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader(body))
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar.sha1", strings.NewReader(blob.SHA1([]byte(body))))
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar.md5", strings.NewReader(blob.MD5([]byte(body))+"  lib-1.0.0.jar"))

	res, err := New().VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean, got %+v", res.Findings)
	}
	if res.BlobsChecked != 3 || res.BytesRead == 0 {
		t.Errorf("stats wrong: %+v", res)
	}
}

func TestVerifyIntegrity_Mismatch(t *testing.T) {
	c := ctxWith(t, "")
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader("original"))
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar.sha1", strings.NewReader(blob.SHA1([]byte("original"))))
	// Corrupt the artifact behind the sidecar's back.
	c.Blob.Put("maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader("flipped")) //nolint:errcheck

	res, _ := New().VerifyIntegrity(c, integrity.ModeFull)
	if kinds(res)[integrity.KindMismatch] != 1 {
		t.Fatalf("expected 1 mismatch, got %+v", res.Findings)
	}
	f := res.Findings[0]
	if f.Component != "com.acme:lib" || f.Version != "1.0.0" {
		t.Errorf("attribution wrong: %+v", f)
	}

	// Quick mode must not hash → no mismatch reported.
	qres, _ := New().VerifyIntegrity(c, integrity.ModeQuick)
	if kinds(qres)[integrity.KindMismatch] != 0 || qres.BytesRead != 0 {
		t.Errorf("quick mode hashed blobs: %+v", qres)
	}
}

func TestVerifyIntegrity_OrphanSidecarAndCompRecord(t *testing.T) {
	c := ctxWith(t, "")
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar", strings.NewReader("bytes"))
	serveReq(c, http.MethodPut, "com/acme/lib/1.0.0/lib-1.0.0.jar.sha1", strings.NewReader(blob.SHA1([]byte("bytes"))))
	// Delete the artifact, leaving the sidecar dangling. The compNS record is
	// NOT orphaned yet — the sidecar blob still lives under the component tree.
	c.Blob.Delete("maven-hosted/com/acme/lib/1.0.0/lib-1.0.0.jar") //nolint:errcheck

	res, _ := New().VerifyIntegrity(c, integrity.ModeFull)
	k := kinds(res)
	if k[integrity.KindOrphan] != 1 {
		t.Fatalf("expected 1 orphan (dangling sidecar), got %+v", res.Findings)
	}

	// Component record whose whole tree is gone → orphan meta record.
	c.Meta.PutJSON("maven-hosted:maven:comp", "org.gone:artifact", compMeta{}) //nolint:errcheck
	res, _ = New().VerifyIntegrity(c, integrity.ModeFull)
	if kinds(res)[integrity.KindOrphan] != 2 {
		t.Fatalf("expected dangling sidecar + empty component record, got %+v", res.Findings)
	}
}

func TestVerifyIntegrity_SnapshotMissing(t *testing.T) {
	c := ctxWith(t, "")
	sub := "com/acme/lib/1.0-SNAPSHOT/lib-1.0-20260101.120000-1.jar"
	serveReq(c, http.MethodPut, sub, strings.NewReader("snap-bytes"))

	res, _ := New().VerifyIntegrity(c, integrity.ModeQuick)
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean, got %+v", res.Findings)
	}

	c.Blob.Delete("maven-hosted/" + sub) //nolint:errcheck
	res, _ = New().VerifyIntegrity(c, integrity.ModeQuick)
	k := kinds(res)
	if k[integrity.KindMissing] != 1 {
		t.Fatalf("expected 1 missing (snapshot record), got %+v", res.Findings)
	}
}
