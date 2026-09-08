package maven

import (
	"fmt"
	"io"
	"strings"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/repo"
)

// Compile-time assertion that the handler implements the integrity seam.

// VerifyIntegrity implements format.Handler.
//
// Maven's blobs are the source of truth (indexes are generated from them), so
// a blob without metadata is normal — the invariants run the other way:
//
//   - checksum sidecar blob (.md5/.sha1/.sha256) whose base artifact is gone → orphan
//   - full mode: sidecar content ≠ recomputed digest of the base artifact    → mismatch
//   - snapArtifact record whose published SNAPSHOT file is gone              → missing
//   - component record (browse timestamps) with no blobs left underneath     → orphan
//
// Artifacts without client-uploaded sidecars carry no stored expectation, so
// silent corruption there is undetectable by design (nothing recorded at
// write time to compare against).
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	if c.Repo.Kind == repo.Proxy {
		return integrity.VerifyProxyCache(c.Repo.Name, c.Blob, c.Meta)
	}
	var res integrity.Result
	if c.Repo.Kind != repo.Hosted {
		return res, nil
	}

	prefix := c.Repo.Name + "/"
	keys, err := c.Blob.List(prefix)
	if err != nil {
		return res, err
	}
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		present[k] = true
	}
	res.BlobsChecked = len(keys)

	// Sidecar invariants. Hash each base artifact at most once even when it
	// has several sidecars.
	hashed := map[string]integrity.Hashes{}
	for _, k := range keys {
		cs := checksumExt(k)
		if cs == "" {
			continue
		}
		base := strings.TrimSuffix(k, "."+cs)
		comp, ver := compVerFromKey(strings.TrimPrefix(base, prefix))
		if !present[base] {
			res.Add(integrity.KindOrphan, k, comp, ver,
				"checksum sidecar has no base artifact: "+strings.TrimPrefix(base, prefix)+" is gone")
			continue
		}
		if mode != integrity.ModeFull {
			continue
		}
		hs, ok := hashed[base]
		if !ok {
			hs, err = integrity.HashBlob(c.Blob, base)
			if err != nil {
				res.Add(integrity.KindMismatch, base, comp, ver, "artifact unreadable: "+err.Error())
				continue
			}
			hashed[base] = hs
			res.BytesRead += hs.Size
		}
		declared, err := readSidecar(c, k)
		if err != nil {
			continue
		}
		var got string
		switch cs {
		case "md5":
			got = hs.MD5
		case "sha1":
			got = hs.SHA1
		case "sha256":
			got = hs.SHA256
		}
		if declared != "" && !strings.EqualFold(declared, got) {
			res.Add(integrity.KindMismatch, base, comp, ver,
				fmt.Sprintf("stored .%s sidecar says %s but artifact hashes to %s — bytes changed since publish", cs, declared, got))
		}
	}

	// snapArtifact records: each published SNAPSHOT file must still exist.
	snapKeys, _ := c.Meta.List(h.snapVersNS(c))
	for _, sk := range snapKeys {
		var rec snapArtifact
		if ok, _ := c.Meta.GetJSON(h.snapVersNS(c), sk, &rec); !ok {
			continue
		}
		res.MetaChecked++
		snapshotPath := sk
		if i := strings.Index(sk, ":"); i >= 0 {
			snapshotPath = sk[:i]
		}
		file := rec.ArtifactID + "-" + rec.Value
		if rec.Classifier != "" {
			file += "-" + rec.Classifier
		}
		file += "." + rec.Extension
		want := prefix + snapshotPath + "/" + file
		if !present[want] {
			res.Add(integrity.KindMissing, want,
				rec.GroupID+":"+rec.ArtifactID, rec.Version,
				"SNAPSHOT record "+sk+" points at a blob that is gone")
		}
	}

	// Component records: flag entries whose artifact tree has no blobs left.
	compKeys, _ := c.Meta.List(h.compNS(c))
	for _, comp := range compKeys {
		res.MetaChecked++
		g, a, ok := strings.Cut(comp, ":")
		if !ok {
			continue
		}
		dir := prefix + strings.ReplaceAll(g, ".", "/") + "/" + a + "/"
		found := false
		for _, k := range keys {
			if strings.HasPrefix(k, dir) {
				found = true
				break
			}
		}
		if !found {
			res.Add(integrity.KindOrphan, h.compNS(c)+" · "+comp, comp, "",
				"component record has no artifacts left in storage")
		}
	}
	return res, nil
}

// compVerFromKey derives ("groupId:artifactId", version) from a repo-relative
// blob sub-path, best-effort (empty strings when the layout doesn't parse).
func compVerFromKey(sub string) (comp, ver string) {
	parts := strings.Split(sub, "/")
	if len(parts) < 4 {
		return "", ""
	}
	for i, p := range parts {
		if i >= 1 && len(p) > 0 && p[0] >= '0' && p[0] <= '9' {
			return strings.Join(parts[:i-1], ".") + ":" + parts[i-1], p
		}
	}
	return "", ""
}

// readSidecar reads a checksum sidecar blob and returns the declared hash
// (first whitespace-separated field — Maven tools sometimes append the
// filename).
func readSidecar(c *format.Context, key string) (string, error) {
	rc, err := c.Blob.Get(key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", nil
	}
	return strings.ToLower(fields[0]), nil
}
