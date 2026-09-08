package cleanup

import (
	"strings"
	"time"

	"forge/internal/meta"
)

// Publish ledger.
//
// Age-based retention needs to know when a version was published. Each format
// used to answer that from its own records, and they disagreed: helm and cran
// stored a timestamp, maven stored one for snapshots only, and npm built its
// version records from meta key names alone so the field was structurally
// always zero. Because the rules skip an artifact with no timestamp rather than
// assume it is old, deleteOlderThanDays was permanently inert on npm and on
// maven releases — with nothing to show for it: the policy still read as active
// and dry runs still reported no candidates.
//
// So publish time is recorded once, in the spine, by every format handler:
// one namespace per repository, one record per component+version. Cleanup
// prefers a format's own timestamp where it has a better one and falls back to
// this ledger otherwise, which means a new format gets working retention by
// calling RecordPublish instead of by reimplementing timestamps.

const publishedNSSuffix = ":published"

// PublishedNS is the meta namespace holding a repository's publish ledger.
func PublishedNS(repoName string) string { return repoName + publishedNSSuffix }

type publishRecord struct {
	PublishedAt time.Time `json:"publishedAt"`
}

// PublishKey is the ledger key for one component+version. Component is the
// format's natural component identity — the package name for npm, the chart
// name for helm, "groupId/artifactId" for maven — and must match what that
// format's cleanup pass uses, or the fallback silently misses.
func PublishKey(component, version string) string {
	return component + ":" + version
}

// RecordPublish stamps a component+version as published now. Best-effort: a
// failure here must never fail the upload that triggered it, since the artifact
// is already stored and a missing ledger entry only costs age-based retention.
func RecordPublish(m meta.Store, repoName, component, version string) {
	recordPublishAt(m, repoName, component, version, time.Now().UTC())
}

// RecordPublishAt is RecordPublish with an explicit time, for handlers that
// know the real publish time (a proxied artifact's upstream date, say) and for
// tests.
func RecordPublishAt(m meta.Store, repoName, component, version string, t time.Time) {
	recordPublishAt(m, repoName, component, version, t.UTC())
}

func recordPublishAt(m meta.Store, repoName, component, version string, t time.Time) {
	if m == nil || repoName == "" || component == "" || version == "" {
		return
	}
	// Never overwrite an existing entry: re-publishing the same coordinates, or
	// a proxy re-caching a tarball, must not make an old artifact look new.
	if ok, err := m.GetJSON(PublishedNS(repoName), PublishKey(component, version), &publishRecord{}); ok && err == nil {
		return
	}
	_ = m.PutJSON(PublishedNS(repoName), PublishKey(component, version), publishRecord{PublishedAt: t})
}

// ForgetPublish drops a ledger entry, so a deleted version does not leave a
// record behind that would resurface if the same coordinates are published
// again.
func ForgetPublish(m meta.Store, repoName, component, version string) {
	if m == nil {
		return
	}
	_ = m.Delete(PublishedNS(repoName), PublishKey(component, version))
}

// PublishIndex loads a repository's whole ledger in one pass. Cleanup reads it
// once per run rather than per artifact — a repository can hold tens of
// thousands of versions, and this is on the retention path.
func PublishIndex(m meta.Store, repoName string) map[string]time.Time {
	out := map[string]time.Time{}
	if m == nil {
		return out
	}
	keys, err := m.List(PublishedNS(repoName))
	if err != nil {
		return out
	}
	for _, k := range keys {
		var rec publishRecord
		if ok, err := m.GetJSON(PublishedNS(repoName), k, &rec); ok && err == nil && !rec.PublishedAt.IsZero() {
			out[k] = rec.PublishedAt
		}
	}
	return out
}

// publishedAt resolves a version's publish time: the format's own timestamp
// when it has one, else the ledger. Returns the zero time when neither knows,
// which the rules treat as "cannot evaluate" rather than "old".
func publishedAt(own time.Time, pub map[string]time.Time, component, version string) time.Time {
	if !own.IsZero() {
		return own
	}
	return pub[PublishKey(component, version)]
}

// RecordPublishFromMavenPath records a publish using a maven sub-path
// ("{groupId path}/{artifactId}/{version}/{file}"). Paths too short to carry a
// version — checksum sidecars at the metadata level, for instance — are ignored.
func RecordPublishFromMavenPath(m meta.Store, repoName, subPath string) {
	if component, version, ok := mavenComponent(subPath); ok {
		RecordPublish(m, repoName, component, version)
	}
}

// mavenComponent derives the ledger component for a maven blob path
// ("{groupId path}/{artifactId}/{version}/{file}" relative to the repo), which
// is the same "groupId/artifactId" key the maven cleanup pass groups by.
// Returns ok=false for a path too short to carry version information.
func mavenComponent(relPath string) (component, version string, ok bool) {
	parts := strings.Split(strings.Trim(relPath, "/"), "/")
	if len(parts) < 3 {
		return "", "", false
	}
	return strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], true
}
