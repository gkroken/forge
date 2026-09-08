package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"forge/internal/format"
	"forge/internal/format/pypi"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/webhook"
)

// Promotion = COPY, not reference.
//
// Promoting a component+version from one hosted repo to another copies its
// bytes into the target so the target stays entirely self-contained: backup,
// cleanup, quotas and integrity all keep working on it without any cross-repo
// coupling. (Reference semantics would need a content-addressed store plus a
// cross-repo refcount GC — the sprawl/data-loss subsystem that would betray
// forge's "few moving parts" edge, and a staging cleanup could then yank bytes
// out from under a promoted release.) Every asset lands through the target's
// own format handler in-process (internalServe), exactly like the Nexus
// migrator, so the target's indexes, checksums and packuments regenerate
// correctly instead of being copied stale.
//
// The one thing the copy carries that a reference could not is provenance:
// "promoted from {sourceRepo}@sha256:{digest} by {actor} at {time}". That is
// metadata — a historical fact recorded at promotion time. Integrity verify
// deliberately does NOT re-check a promoted copy against its source digest:
// the copy is independent, the source may be cleaned or deleted, and coupling
// them would reintroduce exactly the reference-semantics fragility copy avoids.

// ProvenanceRecord captures where a promoted component+version came from. It is
// stored per target repo (meta ns "{target}:provenance", key "{component}@{version}",
// matching vuln.Store's keying) and surfaced on the component detail page.
type ProvenanceRecord struct {
	SourceRepo   string    `json:"sourceRepo"`
	TargetRepo   string    `json:"targetRepo"`
	Component    string    `json:"component"`
	Version      string    `json:"version"`
	SourceDigest string    `json:"sourceDigest"` // sha256 of the primary artifact at promotion time
	PromotedBy   string    `json:"promotedBy"`
	PromotedAt   time.Time `json:"promotedAt"`
}

func provenanceNS(target string) string              { return target + ":provenance" }
func provenanceKey(component, version string) string { return component + "@" + version }

// GetProvenance returns the promotion record for a component+version on a target
// repo, if one exists.
func (s *Server) GetProvenance(target, component, version string) (ProvenanceRecord, bool) {
	var rec ProvenanceRecord
	ok, _ := s.Meta.GetJSON(provenanceNS(target), provenanceKey(component, version), &rec)
	return rec, ok
}

// promoteError carries an HTTP status so the API handler can map a promotion
// failure to the right code (404 unknown source, 409 immutable collision, 507
// target quota, 400 bad request) without string-matching.
type promoteError struct {
	Status int
	Msg    string
}

func (e *promoteError) Error() string { return e.Msg }

func promoteErr(status int, format string, a ...any) *promoteError {
	return &promoteError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// promoteStrategy copies one component+version from src to tgt, returning the
// promoted artifact's digest and the bytes written.
type promoteStrategy func(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error)

// promoteStrategies is the dispatch table, as data rather than a switch so a
// test can enumerate which formats are promotable without having to publish an
// artifact in each one first. Promotion stays here rather than moving onto
// format.Handler because it replays a publish through the target's own handler
// (internalServe); a format package implementing it would either duplicate its
// own publish logic or have to depend on the server's request machinery.
func (s *Server) promoteStrategies() map[string]promoteStrategy {
	return map[string]promoteStrategy{
		"maven": s.promoteMaven,
		"cran":  s.promoteCRAN,
		"helm":  s.promoteHelm,
		"npm":   s.promoteNPM,
		"oci":   s.promoteOCI,
		"pypi":  s.promotePyPI,
	}
}

// promoteComponent copies one component+version from src to tgt through tgt's
// format handler, records provenance, and returns the stored record plus the
// number of bytes copied. Both repos must be hosted and share a format. The
// target's storage quota is enforced (a promote is a hosted write); an
// immutable target that already holds the version refuses the overwrite (409).
func (s *Server) promoteComponent(ctx context.Context, actor string, src, tgt repo.Repository, component, version, publicBase string) (*ProvenanceRecord, int64, error) {
	if component == "" || version == "" {
		return nil, 0, promoteErr(http.StatusBadRequest, "component and version are required")
	}
	if src.Name == tgt.Name {
		return nil, 0, promoteErr(http.StatusBadRequest, "source and target must differ")
	}
	if src.Kind != repo.Hosted || tgt.Kind != repo.Hosted {
		return nil, 0, promoteErr(http.StatusBadRequest, "promotion is only between hosted repositories")
	}
	if src.Format != tgt.Format {
		return nil, 0, promoteErr(http.StatusBadRequest, "format mismatch: %s is %s, %s is %s", src.Name, src.Format, tgt.Name, tgt.Format)
	}

	// The source must actually hold the version.
	if !s.componentExists(src, component, version) {
		return nil, 0, promoteErr(http.StatusNotFound, "%s@%s not found in %s", component, version, src.Name)
	}
	// A collision on an immutable target is a hard stop — that is the point of
	// an immutable release. A mutable target is overwritten (like a re-publish).
	if tgt.IsImmutable() && s.componentExists(tgt, component, version) {
		return nil, 0, promoteErr(http.StatusConflict, "%s@%s already exists in immutable repository %s", component, version, tgt.Name)
	}

	// Quota: a promote is a hosted write, so enforce the target's quota up front
	// (internalServe bypasses the request-path quota gate). Sum the bytes we are
	// about to copy and refuse if they would push the target over.
	incoming := s.componentBytes(src, component, version)
	if tgt.QuotaGB != nil && *tgt.QuotaGB > 0 {
		quotaBytes := int64(*tgt.QuotaGB * bytesPerGB)
		if s.usedBytes(tgt.Name)+incoming > quotaBytes {
			return nil, 0, promoteErr(http.StatusInsufficientStorage,
				"promotion would exceed %s storage quota (used %s + %s > %s)",
				tgt.Name, humanBytes(s.usedBytes(tgt.Name)), humanBytes(incoming), humanBytes(quotaBytes))
		}
	}

	strategy, ok := s.promoteStrategies()[src.Format]
	if !ok {
		return nil, 0, promoteErr(http.StatusNotImplemented, "no promotion strategy for format %s", src.Format)
	}
	digest, copied, err := strategy(ctx, src, tgt, component, version, publicBase)
	if err != nil {
		if _, ok := err.(*promoteError); ok {
			return nil, 0, err
		}
		return nil, 0, promoteErr(http.StatusInternalServerError, "promote %s@%s: %v", component, version, err)
	}

	rec := ProvenanceRecord{
		SourceRepo:   src.Name,
		TargetRepo:   tgt.Name,
		Component:    component,
		Version:      version,
		SourceDigest: digest,
		PromotedBy:   actor,
		PromotedAt:   time.Now().UTC(),
	}
	if err := s.Meta.PutJSON(provenanceNS(tgt.Name), provenanceKey(component, version), rec); err != nil {
		return nil, 0, promoteErr(http.StatusInternalServerError, "record provenance: %v", err)
	}
	s.triggerWalk()             // reflect the target's new bytes in the quota promptly
	s.enqueueVulnScan(tgt.Name) // scan the promoted version on the target
	return &rec, copied, nil
}

// handlePromote serves POST /api/v1/repos/{target}/promote. The body is
// {"sourceRepo","component","version"}. Admin on the target gates the route;
// admin on the source is additionally required (you must be able to admin both
// ends of a copy). On success it records provenance, an audit row, an
// artifact.promoted webhook and the forge_promotions_total metric.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SourceRepo string `json:"sourceRepo"`
		Component  string `json:"component"`
		Version    string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.SourceRepo == "" {
		http.Error(w, "sourceRepo is required", http.StatusBadRequest)
		return
	}
	// Copying reads the source, so require admin there too (writes its own 403).
	if !s.Enforcer.RequireRepoAdmin(w, r, req.SourceRepo) {
		return
	}
	src, ok := s.Repos.Get(req.SourceRepo)
	if !ok {
		http.Error(w, "source repository not found: "+req.SourceRepo, http.StatusNotFound)
		return
	}
	tgt, ok := s.Repos.Get(target)
	if !ok {
		http.Error(w, "target repository not found: "+target, http.StatusNotFound)
		return
	}

	actor := actorLabel(r, s.Auth)
	rec, bytesCopied, err := s.promoteComponent(r.Context(), actor, src, tgt, req.Component, req.Version, publicBase(r))
	if err != nil {
		status := http.StatusInternalServerError
		if pe, ok := err.(*promoteError); ok {
			status = pe.Status
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	s.recordPromotion(r, actor, *rec, bytesCopied)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"promoted":     true,
		"sourceRepo":   rec.SourceRepo,
		"targetRepo":   rec.TargetRepo,
		"component":    rec.Component,
		"version":      rec.Version,
		"sourceDigest": rec.SourceDigest,
		"bytes":        bytesCopied,
	})
}

// recordPromotion writes the governance surfaces for a successful promotion:
// the durable audit log, an artifact.promoted webhook, and the
// forge_promotions_total metric. All are best-effort and off the copy path.
func (s *Server) recordPromotion(r *http.Request, actor string, rec ProvenanceRecord, bytesCopied int64) {
	label := rec.Component + "@" + rec.Version
	if s.AuditLog != nil {
		s.AuditLog.Append(obs.AuditEntry{
			Timestamp: time.Now().UTC(),
			Actor:     actor,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    http.StatusOK,
			Detail:    "promote: " + rec.SourceRepo + " → " + rec.TargetRepo + " " + label + " (" + humanBytes(bytesCopied) + ")",
		})
	}
	if s.Metrics != nil && s.Metrics.Promotions != nil {
		s.Metrics.Promotions.WithLabelValues(rec.TargetRepo).Inc()
	}
	if s.Webhooks != nil {
		ev := webhook.Event{
			Type:      webhook.EventArtifactPromoted,
			Repo:      rec.TargetRepo,
			Format:    "",
			Path:      label,
			Actor:     actor,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"sourceRepo":   rec.SourceRepo,
				"component":    rec.Component,
				"version":      rec.Version,
				"sourceDigest": rec.SourceDigest,
				"bytes":        bytesCopied,
			},
		}
		if tgt, ok := s.Repos.Get(rec.TargetRepo); ok {
			ev.Format = tgt.Format
		}
		go s.Webhooks.Dispatch(context.Background(), ev)
	}
}

// --- source enumeration: existence + size ------------------------------------

// mavenGAPath maps a "groupId:artifactId" component to its blob sub-path.
func mavenGAPath(component string) (string, bool) {
	group, artifact, ok := strings.Cut(component, ":")
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(group, ".", "/") + "/" + artifact, true
}

// componentExists reports whether r holds any blob/record for component+version.
// findVersion locates component+version in r through the format's own
// ListVersions, so promotion no longer keeps a second copy of every format's
// storage layout. Maven spells components two ways — "groupId:artifactId" from
// the API, "groupId/artifactId" from the blob layout — so both are matched.
func (s *Server) findVersion(r repo.Repository, component, version string) (format.Version, bool) {
	h, ok := s.Handlers.For(r.Format)
	if !ok {
		return format.Version{}, false
	}
	versions, err := h.ListVersions(&format.Context{Repo: r, Blob: s.Blob, Meta: s.Meta})
	if err != nil {
		return format.Version{}, false
	}
	for _, v := range versions {
		if v.Version == version && sameComponent(v.Component, component) {
			return v, true
		}
	}
	return format.Version{}, false
}

// sameComponent compares two component spellings, treating maven's
// "com.acme:app" and "com/acme/app" as the same thing.
func sameComponent(a, b string) bool {
	if a == b {
		return true
	}
	norm := func(x string) string {
		g, art, ok := strings.Cut(x, ":")
		if !ok {
			return x
		}
		return strings.ReplaceAll(g, ".", "/") + "/" + art
	}
	return norm(a) == norm(b)
}

func (s *Server) componentExists(r repo.Repository, component, version string) bool {
	_, ok := s.findVersion(r, component, version)
	return ok
}

// componentBytes sums the on-disk size of every blob backing component+version
// in r (used for the target quota pre-check). Best-effort: unknown sizes are 0.
func (s *Server) componentBytes(r repo.Repository, component, version string) int64 {
	v, ok := s.findVersion(r, component, version)
	if !ok {
		return 0
	}
	var total int64
	for _, k := range v.BlobKeys {
		if info, ok, _ := s.Blob.Stat(k); ok {
			total += info.Size
		}
	}
	return total
}

// --- per-format copy ---------------------------------------------------------

// readBlob reads a source blob fully and returns its bytes + sha256.
func (s *Server) readBlob(key string) ([]byte, string, error) {
	rc, err := s.Blob.Get(key)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// promoteMaven copies every asset under the version directory to the identical
// path on the target. Checksum sidecars are copied as content; maven-metadata
// is skipped (the target regenerates it). The primary artifact's sha256 is the
// provenance digest.
func (s *Server) promoteMaven(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error) {
	gaPath, ok := mavenGAPath(component)
	if !ok {
		return "", 0, promoteErr(http.StatusBadRequest, "invalid maven component %q (want groupId:artifactId)", component)
	}
	prefix := src.Name + "/" + gaPath + "/" + version + "/"
	keys, err := s.Blob.List(prefix)
	if err != nil {
		return "", 0, err
	}
	artifact := gaPath[strings.LastIndex(gaPath, "/")+1:]
	var mainDigest, pomDigest string
	var copied int64
	for _, key := range keys {
		sub := strings.TrimPrefix(key, src.Name+"/")
		base := sub[strings.LastIndex(sub, "/")+1:]
		if strings.HasPrefix(base, "maven-metadata.xml") {
			continue // generated by the target on demand
		}
		data, sum, err := s.readBlob(key)
		if err != nil {
			return "", 0, err
		}
		switch {
		case isMavenMainArtifact(base, artifact, version):
			mainDigest = sum
		case base == artifact+"-"+version+".pom":
			pomDigest = sum
		}
		rec := s.internalServe(ctx, http.MethodPut, tgt.Name, sub, "", bytes.NewReader(data), nil, publicBase)
		if !rec.ok() {
			return "", 0, rec.err()
		}
		copied += int64(len(data))
	}
	if copied == 0 {
		return "", 0, promoteErr(http.StatusNotFound, "%s@%s has no assets in %s", component, version, src.Name)
	}
	// Prefer the primary binary (jar/war/…); fall back to the POM for pom-only
	// (parent/BOM) modules.
	digest := mainDigest
	if digest == "" {
		digest = pomDigest
	}
	return digest, copied, nil
}

// isMavenMainArtifact reports whether base is the primary binary artifact
// (jar/war/aar/ear) whose checksum becomes the provenance digest.
func isMavenMainArtifact(base, artifact, version string) bool {
	for _, ext := range []string{".jar", ".war", ".aar", ".ear"} {
		if base == artifact+"-"+version+ext {
			return true
		}
	}
	return false
}

func (s *Server) promoteCRAN(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error) {
	sub := "src/contrib/" + component + "_" + version + ".tar.gz"
	data, digest, err := s.readBlob(src.Name + "/" + sub)
	if err != nil {
		return "", 0, promoteErr(http.StatusNotFound, "%s@%s not found in %s: %v", component, version, src.Name, err)
	}
	rec := s.internalServe(ctx, http.MethodPut, tgt.Name, sub, "", bytes.NewReader(data), nil, publicBase)
	if !rec.ok() {
		return "", 0, rec.err()
	}
	return digest, int64(len(data)), nil
}

// promotePyPI replays each of a release's artifacts as a twine upload against
// the target, so the target builds its own records and simple index rather than
// having them copied in. A release is several files — wheels plus an sdist — and
// promoting half of it would leave pip resolving to something it cannot install.
func (s *Server) promotePyPI(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error) {
	project := pypi.Normalize(component)
	keys, err := s.Meta.List(src.Name + ":pypi")
	if err != nil {
		return "", 0, err
	}
	prefix := project + "/" + version + "/"

	var lastDigest string
	var total int64
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		filename := strings.TrimPrefix(k, prefix)
		data, digest, err := s.readBlob(src.Name + "/packages/" + project + "/" + filename)
		if err != nil {
			return "", 0, promoteErr(http.StatusNotFound, "%s@%s (%s) not found in %s: %v",
				component, version, filename, src.Name, err)
		}
		body, contentType, err := twineForm(project, version, filename, data)
		if err != nil {
			return "", 0, err
		}
		hdr := http.Header{"Content-Type": []string{contentType}}
		rec := s.internalServe(ctx, http.MethodPost, tgt.Name, "", "", body, hdr, publicBase)
		if !rec.ok() {
			return "", 0, rec.err()
		}
		lastDigest = digest
		total += int64(len(data))
	}
	if total == 0 {
		return "", 0, promoteErr(http.StatusNotFound, "%s@%s not found in %s", component, version, src.Name)
	}
	return lastDigest, total, nil
}

// twineForm builds the multipart body twine sends, which is what the pypi
// handler's upload path accepts.
func twineForm(project, version, filename string, data []byte) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for field, value := range map[string]string{
		":action": "file_upload", "name": project, "version": version,
	} {
		if err := w.WriteField(field, value); err != nil {
			return nil, "", err
		}
	}
	fw, err := w.CreateFormFile("content", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

// helmFilename resolves the stored tgz filename for a chart version (falling
// back to the conventional name).
func (s *Server) helmFilename(repoName, chart, version string) string {
	var rec struct {
		Filename string `json:"filename"`
	}
	if ok, _ := s.Meta.GetJSON(repoName+":helm", chart+"-"+version, &rec); ok && rec.Filename != "" {
		return rec.Filename
	}
	return chart + "-" + version + ".tgz"
}

func (s *Server) promoteHelm(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error) {
	filename := s.helmFilename(src.Name, component, version)
	data, digest, err := s.readBlob(src.Name + "/" + filename)
	if err != nil {
		return "", 0, promoteErr(http.StatusNotFound, "%s@%s not found in %s: %v", component, version, src.Name, err)
	}
	rec := s.internalServe(ctx, http.MethodPost, tgt.Name, "api/charts", "", bytes.NewReader(data), nil, publicBase)
	if !rec.ok() {
		return "", 0, rec.err()
	}
	return digest, int64(len(data)), nil
}

// npmTarballKey finds the stored tarball blob key for a package version by
// scanning the package's -/ directory (handles scoped and unscoped filenames).
func (s *Server) npmTarballKey(repoName, pkg, version string) (string, bool) {
	keys, _ := s.Blob.List(repoName + "/" + pkg + "/-/")
	suffix := "-" + version + ".tgz"
	for _, k := range keys {
		if strings.HasSuffix(k, suffix) {
			return k, true
		}
	}
	return "", false
}

func (s *Server) promoteNPM(ctx context.Context, src, tgt repo.Repository, component, version, publicBase string) (string, int64, error) {
	// The stored per-version object is exactly what a publish carries.
	var vobj json.RawMessage
	if ok, _ := s.Meta.GetJSON(src.Name+":npm:v", component+":"+version, &vobj); !ok {
		return "", 0, promoteErr(http.StatusNotFound, "%s@%s not found in %s", component, version, src.Name)
	}
	tarKey, ok := s.npmTarballKey(src.Name, component, version)
	if !ok {
		return "", 0, promoteErr(http.StatusNotFound, "tarball for %s@%s not found in %s", component, version, src.Name)
	}
	data, digest, err := s.readBlob(tarKey)
	if err != nil {
		return "", 0, err
	}
	fname := tarKey[strings.LastIndex(tarKey, "/")+1:]

	// dist-tags that point at exactly this version ride along (union across
	// promotions reconstructs the tag set without ordering issues).
	tags := map[string]string{}
	var packument struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if ok, _ := s.Meta.GetJSON(src.Name+":npm", component, &packument); ok {
		for tag, tv := range packument.DistTags {
			if tv == version {
				tags[tag] = version
			}
		}
	}
	doc := map[string]any{
		"_id":      component,
		"name":     component,
		"versions": map[string]json.RawMessage{version: vobj},
		"_attachments": map[string]any{
			fname: map[string]string{"data": base64.StdEncoding.EncodeToString(data)},
		},
	}
	if len(tags) > 0 {
		doc["dist-tags"] = tags
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", 0, err
	}
	rec := s.internalServe(ctx, http.MethodPut, tgt.Name, component, "", bytes.NewReader(body), nil, publicBase)
	if !rec.ok() {
		return "", 0, rec.err()
	}
	return digest, int64(len(data)), nil
}
