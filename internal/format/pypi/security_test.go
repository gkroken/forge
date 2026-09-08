package pypi

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// The simple pages are HTML that browsers render, and everything in them comes
// from whoever published the package. Two layers keep that safe: upload refuses
// values outside PyPI's own character sets, and rendering escapes whatever is
// stored. Each is tested on its own, because either one alone is one bug away
// from a stored XSS against anyone browsing the repository.

func securityCtx(t *testing.T) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(dir + "/b")
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(dir + "/m")
	if err != nil {
		t.Fatal(err)
	}
	return &format.Context{
		Repo: repo.Repository{Name: "p", Format: "pypi", Kind: repo.Hosted},
		Blob: b,
		Meta: m,
	}
}

func upload(t *testing.T, h *Handler, c *format.Context, name, version, filename string) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("version", version)
	fw, err := mw.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("bytes"))
	_ = mw.Close()

	r := httptest.NewRequest("POST", "/", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	c.Sub = ""
	h.Serve(w, r, c)
	return w.Code
}

// TestUpload_RejectsHostileValues — layer one: bad values never reach the store.
func TestUpload_RejectsHostileValues(t *testing.T) {
	for _, tc := range []struct{ desc, name, version, filename string }{
		{"markup in name", `<img src=x onerror=alert(1)>`, "1.0.0", "ok-1.0.0-py3-none-any.whl"},
		{"quote-break in filename", "ok", "1.0.0", `evil"><img src=y onerror=alert(2)>.whl`},
		{"markup in version", "ok", `1.0.0<script>`, "ok-1.0.0-py3-none-any.whl"},
		{"extension pip cannot install", "ok", "1.0.0", "ok-1.0.0.exe"},
		{"name starting with a separator", "-ok", "1.0.0", "ok-1.0.0-py3-none-any.whl"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			h, c := New(), securityCtx(t)
			if code := upload(t, h, c, tc.name, tc.version, tc.filename); code != 400 {
				t.Errorf("upload accepted %s (status %d); it must be refused", tc.desc, code)
			}
			if recs, _ := h.records(c); len(recs) != 0 {
				t.Errorf("%d record(s) stored for a refused upload: %+v", len(recs), recs)
			}
		})
	}
}

// TestUpload_AcceptsRealDistributions — the guard above must not refuse the
// filenames setuptools actually produces.
func TestUpload_AcceptsRealDistributions(t *testing.T) {
	for _, fn := range []string{
		"my_package-1.0.0-py3-none-any.whl",
		"my_package-1.0.0.tar.gz",
		"numpy-2.1.0-cp312-cp312-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
		"zope.interface-6.1.zip",
	} {
		h, c := New(), securityCtx(t)
		if code := upload(t, h, c, "My.Package", "1.0.0+local.1", fn); code != 200 {
			t.Errorf("upload refused a legitimate distribution %q (status %d)", fn, code)
		}
	}
}

// TestSimplePages_EscapeStoredValues — layer two, tested independently of layer
// one by writing hostile records straight into the meta store, the way a record
// predating the upload guard would look.
func TestSimplePages_EscapeStoredValues(t *testing.T) {
	h, c := New(), securityCtx(t)
	const project = `<img src=x onerror=alert(1)>`
	const filename = `evil"><img src=y onerror=alert(2)>.whl`
	rec := fileRecord{
		Project: project, Version: "1.0.0", Filename: filename,
		SHA256: "deadbeef", RequiresPython: `">alert(3)`,
	}
	if err := c.Meta.PutJSON(h.ns(c), recordKey(project, "1.0.0", filename), rec); err != nil {
		t.Fatal(err)
	}

	render := func(sub string) string {
		w := httptest.NewRecorder()
		c.Sub = sub
		// The target stays "/" — routing reads c.Sub, and a raw project name
		// like this one is not a parseable URL for httptest to build.
		h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
		return w.Body.String()
	}

	for _, tc := range []struct{ desc, page string }{
		{"simple index", render("simple")},
		{"project page", render("simple/" + project)},
	} {
		// The break-out sequences, not the payload text: a stored value may
		// still read "onerror=" inside a percent-encoded href, where "<", ">"
		// and the quote are all encoded and no tag or attribute can start.
		// Payload-specific sequences: a generic `"><` also matches the page's
		// own <meta content="1.0"><title> boilerplate.
		for _, bad := range []string{"<img", `evil">`, "<script"} {
			if strings.Contains(tc.page, bad) {
				t.Errorf("%s renders %q unescaped:\n%s", tc.desc, bad, tc.page)
			}
		}
	}
}

// TestGroupKindRefused — group is the one kind with no path yet, and it must
// say so: an empty but valid index would make pip report "no matching
// distribution" with no hint that the repository kind is the cause.
func TestGroupKindRefused(t *testing.T) {
	h, c := New(), securityCtx(t)
	c.Repo.Kind = repo.Group
	for _, sub := range []string{"", "simple", "simple/anything", "packages/x/y-1.0.whl"} {
		w := httptest.NewRecorder()
		c.Sub = sub
		h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
		if w.Code != 501 {
			t.Errorf("group sub=%q: got %d, want 501", sub, w.Code)
		}
	}
}
