package cleanup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
)

// TrashNS is the meta namespace holding soft-delete tombstones (one per trashed
// component+version). TrashPrefix is the reserved blob key space trashed bytes
// are moved into. It sits OUTSIDE every repository's key space
// ("{repo}/{path}"), so trashed bytes count against neither the storage quota
// (walkBlobSizes lists per repo prefix) nor integrity's orphan walk (each format
// checker lists its own repo prefix) — deleting frees quota immediately, disk is
// reclaimed only on purge.
const (
	TrashNS     = "admin:trash"
	TrashPrefix = "_trash/"
)

// Tombstone records a soft-deleted component+version so it can be restored or
// purged. Blobs are MOVED to TrashPrefix; the meta records a hard delete would
// have removed are captured verbatim for a faithful restore.
type Tombstone struct {
	ID        string            `json:"id"`
	Repo      string            `json:"repo"`
	Format    string            `json:"format"`
	Component string            `json:"component"`
	Version   string            `json:"version"`
	DeletedAt time.Time         `json:"deletedAt"`
	DeletedBy string            `json:"deletedBy"`
	Bytes     int64             `json:"bytes"`
	Blobs     []TrashedBlob     `json:"blobs"`
	Metas     []TrashedMeta     `json:"metas,omitempty"`
	PkgDoc    *TrashedPackument `json:"pkgDoc,omitempty"`
}

// TrashedBlob pairs an original blob key with the trash key its bytes were
// moved to.
type TrashedBlob struct {
	Orig  string `json:"orig"`
	Trash string `json:"trash"`
}

// TrashedMeta is a full meta record (npm version record, helm/cran record)
// captured verbatim so restore can put it back.
type TrashedMeta struct {
	NS  string          `json:"ns"`
	Key string          `json:"key"`
	Doc json.RawMessage `json:"doc"`
}

// TrashedPackument captures the single version entry removed from an npm
// packument (a shared per-package document, not a per-version record).
type TrashedPackument struct {
	NS      string          `json:"ns"`
	Key     string          `json:"key"` // package name
	Version string          `json:"version"`
	Entry   json.RawMessage `json:"entry"` // the removed versions[version] value
}

func newTrashID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// moveBlob copies from→to and deletes the source, returning the bytes moved.
// blob.Store has no native rename, so this is Get→Put→Delete.
func moveBlob(b blob.Store, from, to string) (int64, error) {
	rc, err := b.Get(from)
	if err != nil {
		return 0, err
	}
	info, err := b.Put(to, rc)
	rc.Close() //nolint:errcheck
	if err != nil {
		return 0, err
	}
	_ = b.Delete(from)
	return info.Size, nil
}

// TrashVersion soft-deletes exactly one component+version from a hosted repo:
// its blobs are moved to TrashPrefix and the meta records a hard delete would
// have removed are captured into a persisted Tombstone. Returns the tombstone so
// callers can surface an ID/undo affordance. Mirrors DeleteVersion's per-format
// blob layout and component identifiers.
func TrashVersion(repoName, format, component, version, deletedBy string, b blob.Store, m meta.Store) (*Tombstone, error) {
	if component == "" || version == "" {
		return nil, fmt.Errorf("cleanup: component and version are required")
	}
	ts := &Tombstone{
		ID:        newTrashID(),
		Repo:      repoName,
		Format:    format,
		Component: component,
		Version:   version,
		DeletedAt: time.Now().UTC(),
		DeletedBy: deletedBy,
	}
	var err error
	switch format {
	case "maven":
		err = trashMaven(ts, repoName, component, version, b)
	case "cran":
		err = trashCRAN(ts, repoName, component, version, b, m)
	case "helm":
		err = trashHelm(ts, repoName, component, version, b, m)
	case "npm":
		err = trashNPM(ts, repoName, component, version, b, m)
	case "pypi":
		err = trashPyPI(ts, repoName, component, version, b, m)
	default:
		return nil, fmt.Errorf("cleanup: unsupported format %q", format)
	}
	if err != nil {
		return nil, err
	}
	if len(ts.Blobs) == 0 {
		return nil, fmt.Errorf("cleanup: %s %s not found", component, version)
	}
	if err := m.PutJSON(TrashNS, ts.ID, ts); err != nil {
		return nil, err
	}
	return ts, nil
}

// moveToTrash moves origKey's bytes into this tombstone's trash space and
// records the pairing + bytes. Missing blobs are silently skipped.
func (ts *Tombstone) moveToTrash(b blob.Store, origKey string) error {
	if _, ok, _ := b.Stat(origKey); !ok {
		return nil
	}
	trashKey := TrashPrefix + ts.ID + "/" + origKey
	n, err := moveBlob(b, origKey, trashKey)
	if err != nil {
		return err
	}
	ts.Blobs = append(ts.Blobs, TrashedBlob{Orig: origKey, Trash: trashKey})
	ts.Bytes += n
	return nil
}

// captureMeta reads a full meta record verbatim, appends it to the tombstone,
// and deletes it from the store.
func (ts *Tombstone) captureMeta(m meta.Store, ns, key string) error {
	var doc json.RawMessage
	ok, err := m.GetJSON(ns, key, &doc)
	if err != nil || !ok {
		return err
	}
	ts.Metas = append(ts.Metas, TrashedMeta{NS: ns, Key: key, Doc: doc})
	return m.Delete(ns, key)
}

func trashMaven(ts *Tombstone, repoName, ga, version string, b blob.Store) error {
	group, artifact, ok := strings.Cut(ga, ":")
	if !ok {
		return fmt.Errorf("cleanup: invalid maven component %q (want groupId:artifactId)", ga)
	}
	gaPath := strings.ReplaceAll(group, ".", "/") + "/" + artifact
	prefix := repoName + "/" + gaPath + "/" + version + "/"
	keys, err := b.List(prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := ts.moveToTrash(b, k); err != nil {
			return err
		}
	}
	return nil
}

func trashCRAN(ts *Tombstone, repoName, pkg, version string, b blob.Store, m meta.Store) error {
	if err := ts.moveToTrash(b, repoName+"/src/contrib/"+pkg+"_"+version+".tar.gz"); err != nil {
		return err
	}
	return ts.captureMeta(m, repoName+"+cran", pkg+"_"+version)
}

func trashHelm(ts *Tombstone, repoName, chart, version string, b blob.Store, m meta.Store) error {
	ns := repoName + ":helm"
	var rec helmRecord
	hasRec, _ := m.GetJSON(ns, chart+"-"+version, &rec)
	filename := rec.Filename
	if filename == "" {
		filename = chart + "-" + version + ".tgz"
	}
	if err := ts.moveToTrash(b, repoName+"/"+filename); err != nil {
		return err
	}
	if hasRec {
		return ts.captureMeta(m, ns, chart+"-"+version)
	}
	return nil
}

func trashNPM(ts *Tombstone, repoName, pkg, version string, b blob.Store, m meta.Store) error {
	if err := ts.moveToTrash(b, repoName+"/"+pkg+"/-/"+pkg+"-"+version+".tgz"); err != nil {
		return err
	}
	// Per-version record.
	if err := ts.captureMeta(m, repoName+":npm:v", pkg+":"+version); err != nil {
		return err
	}
	// Remove the version from the shared packument, capturing the removed entry.
	pkgNS := repoName + ":npm"
	var packument map[string]any
	if ok, _ := m.GetJSON(pkgNS, pkg, &packument); ok {
		if vers, ok := packument["versions"].(map[string]any); ok {
			if entry, present := vers[version]; present {
				raw, _ := json.Marshal(entry)
				ts.PkgDoc = &TrashedPackument{NS: pkgNS, Key: pkg, Version: version, Entry: raw}
				delete(vers, version)
				packument["versions"] = vers
				if err := m.PutJSON(pkgNS, pkg, packument); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ListTrash returns the tombstones for a repository (all repos if repoName is
// empty), newest first.
func ListTrash(m meta.Store, repoName string) ([]Tombstone, error) {
	ids, err := m.List(TrashNS)
	if err != nil {
		return nil, err
	}
	var out []Tombstone
	for _, id := range ids {
		var ts Tombstone
		if ok, _ := m.GetJSON(TrashNS, id, &ts); !ok {
			continue
		}
		if repoName != "" && ts.Repo != repoName {
			continue
		}
		out = append(out, ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	return out, nil
}

// GetTombstone fetches one tombstone by ID.
func GetTombstone(m meta.Store, id string) (Tombstone, bool, error) {
	var ts Tombstone
	ok, err := m.GetJSON(TrashNS, id, &ts)
	return ts, ok, err
}

// RestoreVersion moves a tombstone's blobs back to their original keys, restores
// the captured meta records (and the npm packument entry), and removes the
// tombstone. Restoring over a name that was re-published since deletion
// overwrites it — last write wins, matching a normal re-publish.
func RestoreVersion(m meta.Store, b blob.Store, id string) (*Tombstone, error) {
	ts, ok, err := GetTombstone(m, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cleanup: trash entry %q not found", id)
	}
	for _, bl := range ts.Blobs {
		if _, ok, _ := b.Stat(bl.Trash); !ok {
			continue
		}
		if _, err := moveBlob(b, bl.Trash, bl.Orig); err != nil {
			return nil, err
		}
	}
	for _, md := range ts.Metas {
		if err := m.PutJSON(md.NS, md.Key, md.Doc); err != nil {
			return nil, err
		}
	}
	if ts.PkgDoc != nil {
		var packument map[string]any
		if ok, _ := m.GetJSON(ts.PkgDoc.NS, ts.PkgDoc.Key, &packument); !ok {
			packument = map[string]any{}
		}
		vers, _ := packument["versions"].(map[string]any)
		if vers == nil {
			vers = map[string]any{}
		}
		var entry any
		_ = json.Unmarshal(ts.PkgDoc.Entry, &entry)
		vers[ts.PkgDoc.Version] = entry
		packument["versions"] = vers
		if err := m.PutJSON(ts.PkgDoc.NS, ts.PkgDoc.Key, packument); err != nil {
			return nil, err
		}
	}
	if err := m.Delete(TrashNS, id); err != nil {
		return nil, err
	}
	return &ts, nil
}

// PurgeTombstone hard-deletes a tombstone's trashed blobs and the tombstone
// itself, freeing disk. Returns the bytes reclaimed.
func PurgeTombstone(m meta.Store, b blob.Store, id string) (int64, error) {
	ts, ok, err := GetTombstone(m, id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("cleanup: trash entry %q not found", id)
	}
	var freed int64
	for _, bl := range ts.Blobs {
		if info, ok, _ := b.Stat(bl.Trash); ok {
			freed += info.Size
		}
		_ = b.Delete(bl.Trash)
	}
	return freed, m.Delete(TrashNS, id)
}

// PurgeExpired hard-deletes every tombstone older than retention. Returns the
// number purged and bytes reclaimed. A non-positive retention is a no-op (trash
// is kept indefinitely).
func PurgeExpired(m meta.Store, b blob.Store, retention time.Duration) (int, int64, error) {
	if retention <= 0 {
		return 0, 0, nil
	}
	all, err := ListTrash(m, "")
	if err != nil {
		return 0, 0, err
	}
	cutoff := time.Now().UTC().Add(-retention)
	var purged int
	var freed int64
	for _, ts := range all {
		if ts.DeletedAt.After(cutoff) {
			continue
		}
		f, err := PurgeTombstone(m, b, ts.ID)
		if err != nil {
			continue
		}
		purged++
		freed += f
	}
	return purged, freed, nil
}

// trashPyPI moves every artifact of a release — a release usually has both a
// wheel and an sdist, and may have several wheels — plus the record describing
// each, so a restore brings the whole release back rather than half of it.
func trashPyPI(ts *Tombstone, repoName, project, version string, b blob.Store, m meta.Store) error {
	ns := repoName + ":pypi"
	keys, err := m.List(ns)
	if err != nil {
		return err
	}
	prefix := project + "/" + version + "/"
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		filename := strings.TrimPrefix(k, prefix)
		if err := ts.moveToTrash(b, repoName+"/packages/"+project+"/"+filename); err != nil {
			return err
		}
		if err := ts.captureMeta(m, ns, k); err != nil {
			return err
		}
	}
	return nil
}
