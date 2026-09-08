package cleanup

import (
	"encoding/json"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// OCI retention.
//
// The retention unit is a tag: an image:tag pair is the thing a person publishes
// and reasons about, so it maps onto the same component+version the other
// formats use. Untagged manifests are not retention's business — they are either
// referenced by an index that is still tagged, or they are debris for integrity
// verify to report.
//
// Reclaiming space then needs a sweep, because deleting a tag frees nothing on
// its own: the bytes live in the manifest and its config and layer blobs. The
// sweep is deliberately narrow — it considers only the digests reachable from
// the manifests whose last tag was just removed, and drops those that no
// surviving manifest still references.
//
// That narrowness is the safety property. A full mark-and-sweep over every blob
// in the repository would also collect the layers of an in-flight `docker push`,
// which uploads blobs before the manifest that references them; those blobs are
// unreachable from any manifest and would look exactly like garbage. Restricting
// the sweep to what a deletion orphaned means an upload in progress is never a
// candidate, so retention needs no registry-wide read-only window.

type ociTag struct {
	Image   string
	Tag     string
	Digest  string
	Pushed  time.Time
	BlobKey string // manifest blob key
}

// ociState is one repository's tag and manifest graph, read once per run.
type ociState struct {
	tags      []ociTag
	tagKey    map[string]string // image+":"+tag -> meta key "tags/{image}/{tag}"
	manifests map[string]bool   // every manifest digest known to the repo
}

func loadOCIState(repoName string, m meta.Store, pub map[string]time.Time) (ociState, error) {
	st := ociState{tagKey: map[string]string{}, manifests: map[string]bool{}}
	ns := repoName + ":oci"
	keys, err := m.List(ns)
	if err != nil {
		return st, err
	}
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "manifests/"):
			st.manifests[strings.TrimPrefix(k, "manifests/")] = true
		case strings.HasPrefix(k, "tags/"):
			rest := strings.TrimPrefix(k, "tags/")
			image, tag, ok := lastCut(rest, "/")
			if !ok {
				continue
			}
			var dgst string
			if ok, _ := m.GetJSON(ns, k, &dgst); !ok || dgst == "" {
				continue
			}
			// The handler stamps tag-times on push; the publish ledger is the
			// fallback, and covers tags written before either existed.
			var pushed time.Time
			var ts string
			if ok, _ := m.GetJSON(ns, "tag-times/"+image+"/"+tag, &ts); ok {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					pushed = t.UTC()
				}
			}
			pushed = publishedAt(pushed, pub, image, tag)
			st.tags = append(st.tags, ociTag{
				Image: image, Tag: tag, Digest: dgst, Pushed: pushed,
				BlobKey: repoName + "/manifests/" + dgst,
			})
			st.tagKey[image+":"+tag] = k
		}
	}
	return st, nil
}

// lastCut splits on the FINAL separator, so an image name containing slashes
// ("acme/api") keeps its path and only the tag is taken off the end.
func lastCut(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}

// manifestRefs returns the digests a manifest points at: its config blob, its
// layers, and for an index, its child manifests.
func manifestRefs(b blob.Store, key string) []string {
	rc, err := b.Get(key)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if json.NewDecoder(rc).Decode(&m) != nil {
		return nil
	}
	var out []string
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	for _, sm := range m.Manifests {
		out = append(out, sm.Digest)
	}
	return out
}

func runOCI(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (Result, error) {
	pub := PublishIndex(m, repoName)
	st, err := loadOCIState(repoName, m, pub)
	if err != nil {
		return Result{}, err
	}
	ns := repoName + ":oci"

	byImage := map[string][]ociTag{}
	for _, t := range st.tags {
		byImage[t.Image] = append(byImage[t.Image], t)
	}

	var res Result
	deletedTags := map[string]bool{} // image+":"+tag
	for image, tags := range byImage {
		toDelete := applyPolicies(p, tags,
			func(t ociTag) string { return t.Tag },
			func(t ociTag) time.Time { return t.Pushed },
			func(t ociTag) time.Time { return lastDownloadTime(m, t.BlobKey) },
		)
		for _, t := range toDelete {
			if k, ok := st.tagKey[t.Image+":"+t.Tag]; ok {
				m.Delete(ns, k) //nolint:errcheck
			}
			m.Delete(ns, "tag-times/"+t.Image+"/"+t.Tag) //nolint:errcheck
			ForgetPublish(m, repoName, t.Image, t.Tag)
			deletedTags[t.Image+":"+t.Tag] = true
			res.Deleted++
		}
		_ = image
	}
	if len(deletedTags) == 0 {
		return res, nil
	}

	// Which manifests lost their last tag?
	stillTagged := map[string]bool{}
	orphanedManifests := map[string]bool{}
	for _, t := range st.tags {
		if deletedTags[t.Image+":"+t.Tag] {
			orphanedManifests[t.Digest] = true
		} else {
			stillTagged[t.Digest] = true
		}
	}
	for d := range stillTagged {
		delete(orphanedManifests, d)
	}
	if len(orphanedManifests) == 0 {
		return res, nil
	}

	// Everything still reachable from a surviving tag must survive the sweep.
	keep := map[string]bool{}
	for d := range stillTagged {
		markReachable(b, repoName, d, keep)
	}

	// Sweep only what the removed manifests referenced. Blobs that no manifest
	// ever referenced — an in-flight push — are deliberately out of scope.
	for d := range orphanedManifests {
		mk := repoName + "/manifests/" + d
		for _, ref := range manifestRefs(b, mk) {
			if keep[ref] || orphanedManifests[ref] {
				continue // still in use, or handled as a manifest in its own right
			}
			res.FreedBytes += deleteBlobIfPresent(b, repoName+"/blobs/"+ref)
		}
		if keep[d] {
			continue
		}
		res.FreedBytes += deleteBlobIfPresent(b, mk)
		m.Delete(ns, "manifests/"+d) //nolint:errcheck
	}
	return res, nil
}

// markReachable adds a manifest and everything it references to keep.
func markReachable(b blob.Store, repoName, digest string, keep map[string]bool) {
	if keep[digest] {
		return
	}
	keep[digest] = true
	for _, ref := range manifestRefs(b, repoName+"/manifests/"+digest) {
		if keep[ref] {
			continue
		}
		keep[ref] = true
		// An index's children are manifests too; follow them.
		markReachable(b, repoName, ref, keep)
	}
}

func deleteBlobIfPresent(b blob.Store, key string) int64 {
	info, exists, _ := b.Stat(key)
	if !exists {
		return 0
	}
	b.Delete(key) //nolint:errcheck
	return info.Size
}

func dryRunOCI(repoName string, p *repo.CleanupPolicy, b blob.Store, m meta.Store) (DryRunResult, error) {
	pub := PublishIndex(m, repoName)
	st, err := loadOCIState(repoName, m, pub)
	if err != nil {
		return DryRunResult{}, err
	}
	byImage := map[string][]ociTag{}
	for _, t := range st.tags {
		byImage[t.Image] = append(byImage[t.Image], t)
	}

	result := DryRunResult{Candidates: []Candidate{}}
	for _, tags := range byImage {
		cands, skipped := applyPoliciesTagged(p, tags,
			func(t ociTag) string { return t.Tag },
			func(t ociTag) time.Time { return t.Pushed },
			func(t ociTag) time.Time { return lastDownloadTime(m, t.BlobKey) },
		)
		for _, t := range skipped {
			result.Unevaluable = append(result.Unevaluable, Unevaluable{
				Component: t.Image, Version: t.Tag, Rule: "delete_older_than_days",
			})
		}
		for _, t := range cands {
			result.Candidates = append(result.Candidates, Candidate{
				Component: t.rec.Image,
				Version:   t.rec.Tag,
				SizeBytes: statSize(b, t.rec.BlobKey),
				AgeDays:   blobAgeDays(t.at),
				Reason:    t.reason,
			})
		}
	}
	return result, nil
}
