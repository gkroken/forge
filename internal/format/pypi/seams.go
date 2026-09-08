package pypi

import (
	"sort"
	"strings"

	"forge/internal/format"
	"forge/internal/integrity"
)

// The seams every format answers. PyPI declines only Reindex (its simple pages
// are generated from records on every request, so there is no materialized index
// to rebuild) and ReferencedImages (a wheel does not name container images).

// BrowseAsTree implements format.Handler: projects are identified by name, so a
// flat list is the honest view.
func (h *Handler) BrowseAsTree() bool { return false }

// BrowseRepo implements format.Handler.
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	recs, err := h.records(c)
	if err != nil {
		return nil, err
	}
	versions := map[string]map[string]bool{}
	for _, rec := range recs {
		if versions[rec.Project] == nil {
			versions[rec.Project] = map[string]bool{}
		}
		versions[rec.Project][rec.Version] = true
	}
	out := make([]format.BrowseEntry, 0, len(versions))
	for project, vs := range versions {
		list := make([]string, 0, len(vs))
		for v := range vs {
			list = append(list, v)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(list)))
		out = append(out, format.BrowseEntry{Name: project, Versions: list})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Inspect implements format.Handler.
func (h *Handler) Inspect(c *format.Context, baseURL, component string) (format.ComponentDetail, bool) {
	project := Normalize(component)
	recs, err := h.records(c)
	if err != nil {
		return format.ComponentDetail{}, false
	}
	detail := format.ComponentDetail{
		Name:           project,
		InstallSnippet: "pip install --index-url " + baseURL + "/repository/" + c.Repo.Name + "/simple/ " + project,
	}
	for _, rec := range recs {
		if rec.Project != project {
			continue
		}
		detail.Versions = append(detail.Versions, format.VersionInfo{
			Version:     rec.Version,
			SizeBytes:   rec.Size,
			PublishedAt: rec.UploadedAt,
			SHA256:      rec.SHA256,
			DownloadURL: baseURL + "/repository/" + c.Repo.Name + "/packages/" + project + "/" + rec.Filename,
			ContentType: contentTypeFor(rec.Filename),
		})
	}
	if len(detail.Versions) == 0 {
		return format.ComponentDetail{}, false
	}
	sort.Slice(detail.Versions, func(i, j int) bool {
		return detail.Versions[i].Version > detail.Versions[j].Version
	})
	return detail, true
}

// OSVEcosystem implements format.Handler: PyPI is OSV's own ecosystem name.
func (h *Handler) OSVEcosystem() string { return "PyPI" }

// OSVCoordinates implements format.Handler. OSV keys PyPI advisories by the
// normalized project name, which is what forge stores.
func (h *Handler) OSVCoordinates(component string) (string, string, bool) {
	if component == "" {
		return "", "", false
	}
	return "PyPI", Normalize(component), true
}

// VulnGateTarget implements format.Handler: a download of an artifact is what
// the gate acts on, so map "packages/{project}/{filename}" back to the release.
func (h *Handler) VulnGateTarget(sub string) (string, string, bool) {
	project, filename, ok := artifactPath(sub)
	if !ok {
		return "", "", false
	}
	version, ok := versionFromFilename(project, filename)
	if !ok {
		return "", "", false
	}
	return project, version, true
}

// ClaimPath implements format.Handler: the claimable namespace is the project.
func (h *Handler) ClaimPath(sub string) (string, bool) {
	if project, _, ok := artifactPath(sub); ok {
		return project, true
	}
	// A simple-index request for a project is also a claim on that name — this
	// is where dependency confusion actually bites, since pip resolves there
	// before it ever downloads.
	if rest, ok := strings.CutPrefix(sub, "simple/"); ok {
		if project := Normalize(strings.Trim(rest, "/")); project != "" {
			return project, true
		}
	}
	return "", false
}

// OwnsComponent implements format.Handler.
func (h *Handler) OwnsComponent(c *format.Context, component string) bool {
	project := Normalize(component)
	for _, rec := range h.mustRecords(c) {
		if rec.Project == project {
			return true
		}
	}
	return false
}

// VerifyIntegrity implements format.Handler. PyPI's own consistency rule is
// that every record has its file and every file has a record; the simple pages
// are generated per request, so there is no index that can drift.
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	// A proxy owns no records — its truth is the shared cache convention
	// (blob bytes plus a CacheEntry beside them), checked in one place.
	if isProxy(c) {
		return integrity.VerifyProxyCache(c.Repo.Name, c.Blob, c.Meta)
	}
	var res integrity.Result
	recs, err := h.records(c)
	if err != nil {
		return res, err
	}
	onDisk := map[string]bool{}
	keys, _ := c.Blob.List(c.Repo.Name + "/packages/")
	for _, k := range keys {
		onDisk[k] = true
	}

	for _, rec := range recs {
		res.MetaChecked++
		key := h.fileKey(c, rec.Project, rec.Filename)
		_, exists, _ := c.Blob.Stat(key)
		if !exists {
			res.Add(integrity.KindMissing, key, rec.Project, rec.Version,
				"record exists but the file is gone — pip will 404 on a link the index still advertises")
			continue
		}
		delete(onDisk, key)
		// Stat does not hash — the digest has to be re-read from the bytes, and
		// only full mode pays for that. Comparing against Stat's empty SHA256
		// reported every file in the repository as corrupt.
		if mode == integrity.ModeFull && rec.SHA256 != "" {
			hs, herr := integrity.HashBlob(c.Blob, key)
			if herr != nil {
				res.Add(integrity.KindMismatch, key, rec.Project, rec.Version,
					"file unreadable: "+herr.Error())
				continue
			}
			res.BytesRead += hs.Size
			if !strings.EqualFold(hs.SHA256, rec.SHA256) {
				res.Add(integrity.KindMismatch, key, rec.Project, rec.Version,
					"file hashes to "+hs.SHA256+" but the index advertises "+rec.SHA256+
						" — pip will reject the download")
			}
		}
	}
	for key := range onDisk {
		res.Add(integrity.KindOrphan, key, "", "",
			"file has no record — it is unreachable through the simple index")
	}
	return res, nil
}

// --- retention --------------------------------------------------------------

// ListVersions implements format.Handler. A release is a project+version, and
// its artifacts are every wheel and sdist published under it.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	recs, err := h.records(c)
	if err != nil {
		return nil, err
	}
	type key struct{ project, version string }
	grouped := map[key]*format.Version{}
	for _, rec := range recs {
		k := key{rec.Project, rec.Version}
		v := grouped[k]
		if v == nil {
			v = &format.Version{Component: rec.Project, Version: rec.Version}
			grouped[k] = v
		}
		v.BlobKeys = append(v.BlobKeys, h.fileKey(c, rec.Project, rec.Filename))
		// The earliest upload in a release is when that release appeared.
		if v.PublishedAt.IsZero() || (!rec.UploadedAt.IsZero() && rec.UploadedAt.Before(v.PublishedAt)) {
			v.PublishedAt = rec.UploadedAt
		}
	}
	out := make([]format.Version, 0, len(grouped))
	for _, v := range grouped {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// DeleteVersion implements format.Handler: removes every artifact of a release
// and the records describing them.
func (h *Handler) DeleteVersion(c *format.Context, component, version string) (int64, error) {
	project := Normalize(component)
	// On a proxy this is cache eviction: the bytes go, the upstream mapping
	// goes, and the next request re-fetches.
	if isProxy(c) {
		return h.deleteCached(c, project, version)
	}
	var freed int64
	for _, rec := range h.mustRecords(c) {
		if rec.Project != project || rec.Version != version {
			continue
		}
		key := h.fileKey(c, rec.Project, rec.Filename)
		if info, exists, _ := c.Blob.Stat(key); exists {
			freed += info.Size
			c.Blob.Delete(key) //nolint:errcheck
		}
		c.Meta.Delete(h.ns(c), recordKey(rec.Project, rec.Version, rec.Filename)) //nolint:errcheck
	}
	return freed, nil
}

// --- path parsing -----------------------------------------------------------

// artifactPath splits "packages/{project}/{filename}".
func artifactPath(sub string) (project, filename string, ok bool) {
	rest, ok := strings.CutPrefix(sub, "packages/")
	if !ok {
		return "", "", false
	}
	project, filename, ok = strings.Cut(rest, "/")
	if !ok || project == "" || filename == "" {
		return "", "", false
	}
	return project, filename, true
}

// versionFromFilename recovers the version from a wheel or sdist filename:
//
//	{distribution}-{version}(-{build})?-{python}-{abi}-{platform}.whl
//	{name}-{version}.tar.gz
//
// The distribution part uses "_" where the project name has "-", so the prefix
// is matched against the normalized name rather than compared literally.
func versionFromFilename(project, filename string) (string, bool) {
	base := filename
	switch {
	case strings.HasSuffix(base, ".whl"):
		base = strings.TrimSuffix(base, ".whl")
		parts := strings.Split(base, "-")
		if len(parts) < 5 {
			return "", false
		}
		if Normalize(parts[0]) != project {
			return "", false
		}
		return parts[1], true
	case strings.HasSuffix(base, ".tar.gz"):
		base = strings.TrimSuffix(base, ".tar.gz")
	case strings.HasSuffix(base, ".zip"):
		base = strings.TrimSuffix(base, ".zip")
	default:
		return "", false
	}
	i := strings.LastIndex(base, "-")
	if i <= 0 || Normalize(base[:i]) != project {
		return "", false
	}
	return base[i+1:], true
}

// contentTypeFor labels the two artifact kinds pip understands.
func contentTypeFor(filename string) string {
	if strings.HasSuffix(filename, ".whl") {
		return "application/vnd.python.wheel"
	}
	return "application/gzip"
}
