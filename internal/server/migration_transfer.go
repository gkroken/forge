package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"forge/internal/format"
	"forge/internal/nexus"
)

// Content transfer for the Nexus migration.
//
// Every asset lands through forge's own format handler in-process: the
// migrator synthesizes the same request a publishing client would send and
// calls Handler.Serve directly (the spine integrity/browse/scan already
// use). That keeps every format-specific side effect of a real publish —
// checksum records, packument builds, DESCRIPTION parsing, chart records —
// in exactly one place: the handler.

// memRecorder is a minimal in-process http.ResponseWriter.
type memRecorder struct {
	hdr  http.Header
	code int
	body bytes.Buffer
}

func newMemRecorder() *memRecorder { return &memRecorder{hdr: http.Header{}, code: http.StatusOK} }

func (m *memRecorder) Header() http.Header         { return m.hdr }
func (m *memRecorder) WriteHeader(code int)        { m.code = code }
func (m *memRecorder) Write(b []byte) (int, error) { return m.body.Write(b) }

func (m *memRecorder) ok() bool { return m.code >= 200 && m.code < 300 }

// err summarises a failed internal request for the failure log.
func (m *memRecorder) err() error {
	msg := strings.TrimSpace(m.body.String())
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("forge handler returned %d: %s", m.code, msg)
}

// internalServe routes one synthesized request to the target repo's format
// handler with a full server context — the in-process equivalent of
// handleRepo minus HTTP middleware.
func (s *Server) internalServe(ctx context.Context, method, repoName, sub, rawQuery string, body io.Reader, header http.Header, publicBase string) *memRecorder {
	rec := newMemRecorder()
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		rec.code = http.StatusNotFound
		fmt.Fprintf(&rec.body, "no such repository: %s", repoName)
		return rec
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		rec.code = http.StatusNotImplemented
		fmt.Fprintf(&rec.body, "no handler for format: %s", rp.Format)
		return rec
	}

	scheme, host := "http", "localhost"
	if u, err := url.Parse(publicBase); err == nil && u.Host != "" {
		scheme, host = u.Scheme, u.Host
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	req := &http.Request{
		Method: method,
		URL:    &url.URL{Scheme: scheme, Host: host, Path: "/repository/" + repoName + "/" + sub, RawQuery: rawQuery},
		Host:   host,
		Header: header,
		Body:   io.NopCloser(body),
	}
	if header == nil {
		req.Header = http.Header{}
	}
	if scheme == "https" {
		// npm's publicBase derives the tarball-URL scheme from r.TLS.
		req.TLS = &tls.ConnectionState{}
	}
	req = req.WithContext(ctx)

	// A migration writes through a handler in process and so misses the quota
	// gate in handleRepo. Refusing here keeps an import from silently filling a
	// repository past a limit its owner set.
	if _, _, over := s.quotaExceeded(rp, req.ContentLength); over && method != http.MethodGet {
		rec.code = http.StatusInsufficientStorage
		rec.body.WriteString("storage quota exceeded for " + rp.Name)
		return rec
	}

	h.Serve(rec, req, &format.Context{
		Repo: rp, Blob: s.repoBlob(rp), Meta: s.Meta, HTTP: s.client, Sub: sub,
		Repos: s.Repos, Queue: s.Queue, Metrics: s.Metrics,
	})
	return rec
}

// migrateRepoContent dispatches to the per-format transfer strategy.
func (s *Server) migrateRepoContent(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan, st *repoMigState) {
	switch rp.TargetFormat {
	case "maven", "cran":
		s.migratePathAssets(ctx, client, spec, rp, st)
	case "npm":
		s.migrateNPM(ctx, client, spec, rp, st)
	case "helm":
		s.migrateHelm(ctx, client, spec, rp, st)
	case "oci":
		s.migrateOCI(ctx, client, spec, rp, st)
	default:
		st.Note = "no transfer strategy for format " + rp.TargetFormat
	}
}

// stateEvery is how many assets between progress persists.
const stateEvery = 25

func (s *Server) maybePersistState(st *repoMigState) {
	if (st.Migrated+st.Skipped+st.Failed)%stateEvery == 0 {
		s.migPutState(st)
	}
}

// --- maven / cran: 1 asset = 1 PUT at the same path ---------------------------

// migratePathAssets copies every asset to the identical path on the target.
// Maven checksum sidecars are copied too — they are content in forge's model,
// and the post-migration FULL verify re-hashes each base artifact against
// them, turning the sidecars into end-to-end transfer verification.
// maven-metadata.xml (and its checksums) is skipped: forge generates it.
func (s *Server) migratePathAssets(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan, st *repoMigState) {
	err := client.ListComponents(ctx, rp.Source, func(comp nexus.Component) error {
		st.SourceComponents++
		for _, a := range comp.Assets {
			path := strings.TrimPrefix(a.Path, "/")
			base := path[strings.LastIndex(path, "/")+1:]
			if rp.TargetFormat == "maven" && strings.HasPrefix(base, "maven-metadata.xml") {
				// Generated by forge on demand; copying Nexus's would shadow it.
				continue
			}
			st.SourceAssets++
			key := rp.Target + "/" + path
			if info, ok, _ := s.Blob.Stat(key); ok && (a.FileSize == 0 || info.Size == a.FileSize) {
				st.Skipped++
				s.maybePersistState(st)
				continue
			}
			rc, _, err := client.Download(ctx, a.DownloadURL)
			if err != nil {
				st.fail(path, err)
				continue
			}
			rec := s.internalServe(ctx, http.MethodPut, rp.Target, path, "", rc, nil, spec.PublicBase)
			rc.Close()
			if rec.ok() {
				st.Migrated++
			} else {
				st.fail(path, rec.err())
			}
			s.maybePersistState(st)
		}
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		st.fail("(component listing)", err)
	}
}

// --- npm: rebuild a per-version publish from the source packument -------------

type srcPackument struct {
	Versions map[string]json.RawMessage `json:"versions"`
	DistTags map[string]string          `json:"dist-tags"`
}

func (s *Server) migrateNPM(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan, st *repoMigState) {
	packs := map[string]*srcPackument{}
	getPack := func(pkg string) (*srcPackument, error) {
		if p, ok := packs[pkg]; ok {
			return p, nil
		}
		// Scoped names are fetched in their registry wire form (@scope%2Fname).
		u := spec.URL + "/repository/" + rp.Source + "/" + strings.ReplaceAll(pkg, "/", "%2F")
		rc, _, err := client.Download(ctx, u)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var p srcPackument
		if err := json.NewDecoder(rc).Decode(&p); err != nil {
			return nil, fmt.Errorf("parse packument for %s: %w", pkg, err)
		}
		packs[pkg] = &p
		return &p, nil
	}

	err := client.ListComponents(ctx, rp.Source, func(comp nexus.Component) error {
		st.SourceComponents++
		st.SourceAssets += len(comp.Assets)
		pkg, ver := comp.Name, comp.Version
		label := pkg + "@" + ver

		// Resume: version record already on target.
		var existing json.RawMessage
		if ok, _ := s.Meta.GetJSON(rp.Target+":npm:v", pkg+":"+ver, &existing); ok {
			st.Skipped++
			s.maybePersistState(st)
			return ctx.Err()
		}

		pack, err := getPack(pkg)
		if err != nil {
			st.fail(label, err)
			return ctx.Err()
		}
		vobj, ok := pack.Versions[ver]
		if !ok {
			st.fail(label, fmt.Errorf("version missing from source packument"))
			return ctx.Err()
		}
		if len(comp.Assets) == 0 {
			st.fail(label, fmt.Errorf("component has no tarball asset"))
			return ctx.Err()
		}
		rc, _, err := client.Download(ctx, comp.Assets[0].DownloadURL)
		if err != nil {
			st.fail(label, err)
			return ctx.Err()
		}
		tarball, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			st.fail(label, err)
			return ctx.Err()
		}

		// dist-tags that point at THIS version ride along; the union across
		// versions reconstructs the source tag set without ordering issues.
		tags := map[string]string{}
		for tag, tv := range pack.DistTags {
			if tv == ver {
				tags[tag] = ver
			}
		}

		name := pkg
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			name = pkg[i+1:]
		}
		fname := name + "-" + ver + ".tgz"
		doc := map[string]any{
			"_id":      pkg,
			"name":     pkg,
			"versions": map[string]json.RawMessage{ver: vobj},
			"_attachments": map[string]any{
				fname: map[string]string{"data": base64.StdEncoding.EncodeToString(tarball)},
			},
		}
		if len(tags) > 0 {
			doc["dist-tags"] = tags
		}
		body, err := json.Marshal(doc)
		if err != nil {
			st.fail(label, err)
			return ctx.Err()
		}
		rec := s.internalServe(ctx, http.MethodPut, rp.Target, pkg, "", bytes.NewReader(body), nil, spec.PublicBase)
		if rec.ok() {
			st.Migrated++
		} else {
			st.fail(label, rec.err())
		}
		s.maybePersistState(st)
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		st.fail("(component listing)", err)
	}
}

// --- helm: 1 chart tgz = 1 POST api/charts -------------------------------------

func (s *Server) migrateHelm(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan, st *repoMigState) {
	err := client.ListComponents(ctx, rp.Source, func(comp nexus.Component) error {
		st.SourceComponents++
		st.SourceAssets += len(comp.Assets)
		label := comp.Name + "-" + comp.Version

		var existing json.RawMessage
		if ok, _ := s.Meta.GetJSON(rp.Target+":helm", label, &existing); ok {
			st.Skipped++
			s.maybePersistState(st)
			return ctx.Err()
		}
		if len(comp.Assets) == 0 {
			st.fail(label, fmt.Errorf("component has no chart asset"))
			return ctx.Err()
		}
		rc, _, err := client.Download(ctx, comp.Assets[0].DownloadURL)
		if err != nil {
			st.fail(label, err)
			return ctx.Err()
		}
		rec := s.internalServe(ctx, http.MethodPost, rp.Target, "api/charts", "", rc, nil, spec.PublicBase)
		rc.Close()
		if rec.ok() {
			st.Migrated++
		} else {
			st.fail(label, rec.err())
		}
		s.maybePersistState(st)
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		st.fail("(component listing)", err)
	}
}

// --- oci: registry-protocol copy (manifest walk, blobs first) ------------------

// manifestAccept lists every manifest media type we ask the source for.
var manifestAccept = []string{
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}

// ociManifest is the subset of a manifest document the copier needs.
type ociManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	Manifests []struct { // present in an index / manifest list
		Digest string `json:"digest"`
	} `json:"manifests"`
}

func (s *Server) migrateOCI(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan, st *repoMigState) {
	// Some Nexus versions list layer blobs as component assets; collect their
	// downloadUrls so blob fetches can prefer the URL Nexus itself reports.
	blobURL := map[string]string{}
	type tagRef struct{ image, tag string }
	var tagRefs []tagRef

	err := client.ListComponents(ctx, rp.Source, func(comp nexus.Component) error {
		st.SourceComponents++
		st.SourceAssets += len(comp.Assets)
		for _, a := range comp.Assets {
			if i := strings.Index(a.Path, "/blobs/sha256:"); i >= 0 {
				blobURL[a.Path[i+len("/blobs/"):]] = a.DownloadURL
			}
		}
		tagRefs = append(tagRefs, tagRef{comp.Name, comp.Version})
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		st.fail("(component listing)", err)
		return
	}

	fetchBlob := func(image, digest string) (io.ReadCloser, error) {
		if u, ok := blobURL[digest]; ok {
			if rc, _, err := client.Download(ctx, u); err == nil {
				return rc, nil
			}
		}
		rc, _, err := client.Download(ctx, spec.URL+"/repository/"+rp.Source+"/v2/"+image+"/blobs/"+digest)
		return rc, err
	}

	copyBlob := func(image, digest string) error {
		if _, ok, _ := s.Blob.Stat(rp.Target + "/blobs/" + digest); ok {
			st.Skipped++ // content-addressed: existence == identity
			return nil
		}
		rc, err := fetchBlob(image, digest)
		if err != nil {
			return err
		}
		defer rc.Close()
		rec := s.internalServe(ctx, http.MethodPost, rp.Target, image+"/blobs/uploads", "digest="+url.QueryEscape(digest), rc, nil, spec.PublicBase)
		if !rec.ok() {
			return rec.err()
		}
		st.Migrated++
		return nil
	}

	// copyManifest walks one manifest (recursing into index entries), copies
	// the blobs it references, then pushes the manifest bytes verbatim —
	// byte-identical or the digest breaks, which is the point.
	var copyManifest func(image, ref string) error
	copyManifest = func(image, ref string) error {
		rc, hdr, err := client.Download(ctx, spec.URL+"/repository/"+rp.Source+"/v2/"+image+"/manifests/"+ref, manifestAccept...)
		if err != nil {
			return fmt.Errorf("fetch manifest %s: %w", ref, err)
		}
		raw, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		var m ociManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("parse manifest %s: %w", ref, err)
		}
		// Resume: the identical manifest already landed (it is only pushed
		// after all its blobs, so its presence implies theirs).
		sum := sha256.Sum256(raw)
		dgst := "sha256:" + hex.EncodeToString(sum[:])
		if _, ok, _ := s.Blob.Stat(rp.Target + "/manifests/" + dgst); ok {
			if strings.HasPrefix(ref, "sha256:") {
				st.Skipped++
				return nil
			}
			var cur string
			if ok2, _ := s.Meta.GetJSON(rp.Target+":oci", "tags/"+image+"/"+ref, &cur); ok2 && cur == dgst {
				st.Skipped++
				return nil
			}
		}
		for _, sub := range m.Manifests {
			if err := copyManifest(image, sub.Digest); err != nil {
				return err
			}
		}
		if m.Config.Digest != "" {
			if err := copyBlob(image, m.Config.Digest); err != nil {
				return fmt.Errorf("config blob %s: %w", m.Config.Digest, err)
			}
		}
		for _, l := range m.Layers {
			if err := copyBlob(image, l.Digest); err != nil {
				return fmt.Errorf("layer blob %s: %w", l.Digest, err)
			}
		}
		ct := m.MediaType
		if ct == "" {
			ct = hdr.Get("Content-Type")
		}
		h := http.Header{}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		rec := s.internalServe(ctx, http.MethodPut, rp.Target, image+"/manifests/"+ref, "", bytes.NewReader(raw), h, spec.PublicBase)
		if !rec.ok() {
			return fmt.Errorf("push manifest %s: %w", ref, rec.err())
		}
		st.Migrated++
		return nil
	}

	for _, tr := range tagRefs {
		if ctx.Err() != nil {
			return
		}
		if err := copyManifest(tr.image, tr.tag); err != nil {
			st.fail(tr.image+":"+tr.tag, err)
		}
		s.maybePersistState(st)
	}
}
