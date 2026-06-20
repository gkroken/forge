package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"

	"forge/internal/format"
	"forge/internal/repo"
)

type uploadPage struct {
	Title     string
	ActiveNav string
	Repo      repo.Repository
	RepoURL   string
	RepoHost  string
	Error     string
	Flash     string
}

const maxUploadSize = 512 << 20 // 512 MiB

func (s *Server) uiUpload(w http.ResponseWriter, r *http.Request, repoName string) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	rp, ok := s.Repos.Get(repoName)
	if !ok {
		http.NotFound(w, r)
		return
	}

	base := publicBase(r)
	page := uploadPage{
		Title:     "Upload — " + repoName,
		ActiveNav: "repos",
		Repo:      rp,
		RepoURL:   base + "/repository/" + repoName + "/",
		RepoHost:  r.Host,
	}

	if r.Method == http.MethodPost {
		s.processUpload(w, r, rp, page)
		return
	}

	render(w, tmplUpload, "admin_shell.html", page)
}

func (s *Server) processUpload(w http.ResponseWriter, r *http.Request, rp repo.Repository, page uploadPage) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil { // #nosec G120 -- body already bounded by MaxBytesReader above
		page.Error = "could not parse upload: " + err.Error()
		render(w, tmplUpload, "admin_shell.html", page)
		return
	}

	f, hdr, err := r.FormFile("file")
	if err != nil {
		page.Error = "no file in upload"
		render(w, tmplUpload, "admin_shell.html", page)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		page.Error = "failed to read upload"
		render(w, tmplUpload, "admin_shell.html", page)
		return
	}

	var uploadErr error
	switch rp.Format {
	case "helm":
		uploadErr = s.uploadHelm(r, rp, hdr.Filename, data)
	case "cran":
		uploadErr = s.uploadCRAN(r, rp, hdr.Filename, data)
	case "npm":
		uploadErr = s.uploadNPM(r, rp, data)
	default:
		page.Error = "browser upload not supported for " + rp.Format
		render(w, tmplUpload, "admin_shell.html", page)
		return
	}

	if uploadErr != nil {
		page.Error = uploadErr.Error()
		render(w, tmplUpload, "admin_shell.html", page)
		return
	}

	page.Flash = "Upload successful."
	render(w, tmplUpload, "admin_shell.html", page)
}

// callHandler constructs a synthetic request and dispatches it through the
// named format handler, returning any non-2xx error as a Go error.
func (s *Server) callHandler(origR *http.Request, rp repo.Repository, method, sub string, body []byte, contentType string) error {
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		return fmt.Errorf("no handler for format %s", rp.Format)
	}

	// Reject path traversal and absolute URLs before building the URL.
	// path.Clean resolves ".." components; if the result differs from the
	// input, the sub-path attempted traversal.
	if path.Clean("/"+sub) != "/"+sub || strings.Contains(sub, "://") {
		return fmt.Errorf("invalid sub-path %q", sub)
	}

	// Dispatched to h.Serve via httptest.NewRecorder — no outbound network call.
	req, err := http.NewRequest(method, "/repository/"+rp.Name+"/"+sub, bytes.NewReader(body)) // #nosec G704 -- internal dispatch only, not an outbound request
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	// Forward Host so format handlers can build absolute URLs.
	req.Host = origR.Host
	req.Header.Set("X-Forwarded-Proto", origR.Header.Get("X-Forwarded-Proto"))

	rec := httptest.NewRecorder()
	h.Serve(rec, req, &format.Context{
		Repo:    rp,
		Blob:    s.Blob,
		Meta:    s.Meta,
		HTTP:    s.client,
		Sub:     sub,
		Repos:   s.Repos,
		Queue:   s.Queue,
		Metrics: s.Metrics,
	})

	res := rec.Result()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("publish failed (%d): %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Server) uploadHelm(origR *http.Request, rp repo.Repository, _ string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("chart", "chart.tgz")
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	return s.callHandler(origR, rp, http.MethodPost, "api/charts", buf.Bytes(), mw.FormDataContentType())
}

func (s *Server) uploadCRAN(origR *http.Request, rp repo.Repository, filename string, data []byte) error {
	// Sanitise: accept only the base filename.
	filename = filepath.Base(filename)
	if !strings.HasSuffix(filename, ".tar.gz") && !strings.HasSuffix(filename, ".tgz") {
		return fmt.Errorf("filename must end in .tar.gz or .tgz (got %q)", filename)
	}
	sub := "src/contrib/" + filename
	return s.callHandler(origR, rp, http.MethodPut, sub, data, "application/octet-stream")
}

func (s *Server) uploadNPM(origR *http.Request, rp repo.Repository, data []byte) error {
	name, version, err := readNPMPackageJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("could not read package.json from tarball: %w", err)
	}
	tarName := filepath.Base(name) + "-" + version + ".tgz"
	b64 := base64.StdEncoding.EncodeToString(data)

	payload := map[string]any{
		"name": name,
		"versions": map[string]any{
			version: map[string]any{
				"name":    name,
				"version": version,
				"dist":    map[string]any{"shasum": ""},
			},
		},
		"dist-tags": map[string]any{"latest": version},
		"_attachments": map[string]any{
			tarName: map[string]any{
				"content_type": "application/octet-stream",
				"data":         b64,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.callHandler(origR, rp, http.MethodPut, name, body, "application/json")
}

// readNPMPackageJSON extracts name and version from a package.json inside an
// npm .tgz tarball (standard layout: package/package.json).
func readNPMPackageJSON(r io.Reader) (name, version string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", "", fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		if filepath.Base(hdr.Name) != "package.json" {
			continue
		}
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(tr).Decode(&pkg); err != nil {
			continue
		}
		if pkg.Name != "" && pkg.Version != "" {
			return pkg.Name, pkg.Version, nil
		}
	}
	return "", "", fmt.Errorf("package.json with name and version not found in tarball")
}
