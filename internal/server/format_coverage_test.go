package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Server-side format coverage.
//
// Promotion, Nexus migration and browser upload stay per-format switches on
// purpose. Promotion replays a publish through the target's own handler
// (internalServe), so moving it into a format package would mean either
// duplicating that format's publish logic or making internal/format depend on
// the server's request machinery — both worse than the switch. Migration needs
// Nexus mapping knowledge that is not a format concern, and browser upload is
// HTTP form handling.
//
// What they must not do is go missing quietly. Each announces an unsupported
// format at runtime, which a user only discovers by trying; this table makes a
// new format declare its support up front, and checks the declaration.

type serverCoverage struct {
	promote bool
	upload  bool // browser upload form
}

var serverExpected = map[string]serverCoverage{
	"maven": {promote: true, upload: false}, // deployed by the build tool, not a form
	"npm":   {promote: true, upload: true},
	"helm":  {promote: true, upload: true},
	"cran":  {promote: true, upload: true},
	"oci":   {promote: true, upload: false}, // pushed with docker/crane, not a form
}

func coverageServer(t *testing.T) *Server {
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
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	for _, h := range []format.Handler{maven.New(), npm.New(), helm.New(), cran.New(), oci.New()} {
		reg.Register(h)
		for _, suffix := range []string{"-src", "-dst"} {
			if err := mgr.Add(repo.Repository{
				Name: h.Format() + suffix, Format: h.Format(),
				Kind: repo.Hosted, Enabled: true, AnonymousRead: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return New(mgr, reg, b, m, nil)
}

// TestServerCoverage_RollCall — every registered format must declare whether it
// is promotable and browser-uploadable. Adding a format fails here until someone
// says, which is the point: the alternative is a user meeting a 501.
func TestServerCoverage_RollCall(t *testing.T) {
	srv := coverageServer(t)
	for _, f := range srv.Handlers.Formats() {
		if _, ok := serverExpected[f]; !ok {
			t.Errorf("format %q is registered but not declared in the server coverage table — "+
				"state whether it supports promotion and browser upload, and wire up what it should", f)
		}
	}
}

// TestServerCoverage_Promote reads the strategy table directly.
//
// Probing through promoteComponent looked reasonable and was useless: it checks
// the component exists before it reaches the dispatch, so a nonexistent probe
// component always failed with "not found" and every format looked supported —
// including one whose strategy had been deleted. That is why the switch became
// data.
func TestServerCoverage_Promote(t *testing.T) {
	srv := coverageServer(t)
	strategies := srv.promoteStrategies()
	for f, want := range serverExpected {
		_, supported := strategies[f]
		if supported != want.promote {
			t.Errorf("format %q promotion support = %v, table says %v", f, supported, want.promote)
		}
	}
	for f := range strategies {
		if _, ok := serverExpected[f]; !ok {
			t.Errorf("format %q has a promotion strategy but is not in the coverage table", f)
		}
	}
}

// TestServerCoverage_BrowserUpload probes the upload switch through the real
// route. It must POST: a GET renders the form for any format and never reaches
// the switch, so a GET-based probe would report every format as supported.
func TestServerCoverage_BrowserUpload(t *testing.T) {
	srv := coverageServer(t)
	for f, want := range serverExpected {
		body, contentType := multipartUpload(t, "probe.bin", []byte("x"))
		req := httptest.NewRequest(http.MethodPost, "/ui/repos/"+f+"-src/upload", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)

		supported := !strings.Contains(rec.Body.String(), "browser upload not supported")
		if supported != want.upload {
			t.Errorf("format %q browser upload = %v, table says %v", f, supported, want.upload)
		}
	}
}

// multipartUpload builds the form body the upload page submits.
func multipartUpload(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

// TestServerCoverage_EveryFormatIsCreatable — the repository form's format
// dropdown was a hardcoded list, so a newly registered format could not be
// created from the UI at all, however complete the rest of its wiring was. It
// now comes from the registry; this pins that, because nothing else would
// notice it drifting back.
func TestServerCoverage_EveryFormatIsCreatable(t *testing.T) {
	srv := coverageServer(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/repos/new", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("new-repository form = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, f := range srv.Handlers.Formats() {
		if !strings.Contains(body, ">"+f+"<") && !strings.Contains(body, `value="`+f+`"`) {
			t.Errorf("format %q is registered but the repository form does not offer it — "+
				"it cannot be created from the UI", f)
		}
	}
}
