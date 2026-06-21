package cleanup

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

const policyNS = "cleanup-policies"

var validPolicyName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// NamedPolicy is a named, reusable retention policy stored in meta.Store.
type NamedPolicy struct {
	Name                string        `json:"name"`
	Description         string        `json:"description,omitempty"`
	KeepVersions        int           `json:"keepVersions,omitempty"`
	KeepReleasesOnly    bool          `json:"keepReleasesOnly,omitempty"`
	DeleteSnapshotsDays int           `json:"deleteSnapshotsDays,omitempty"`
	DeleteOlderThanDays int           `json:"deleteOlderThanDays,omitempty"`
	LastDownloadedDays  int           `json:"lastDownloadedDays,omitempty"` // delete artifacts not downloaded in N days
	RunOnPublish        bool          `json:"runOnPublish,omitempty"`       // also run this policy when an artifact is published
	Interval            time.Duration `json:"-"`
}

func (p NamedPolicy) MarshalJSON() ([]byte, error) {
	type Alias NamedPolicy
	return json.Marshal(&struct {
		Alias
		Interval string `json:"interval,omitempty"`
	}{
		Alias:    Alias(p),
		Interval: durationString(p.Interval),
	})
}

func (p *NamedPolicy) UnmarshalJSON(data []byte) error {
	type Alias NamedPolicy
	aux := &struct {
		*Alias
		Interval string `json:"interval,omitempty"`
	}{Alias: (*Alias)(p)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.Interval != "" {
		d, err := time.ParseDuration(aux.Interval)
		if err != nil {
			return fmt.Errorf("cleanup-policies: invalid interval %q: %w", aux.Interval, err)
		}
		p.Interval = d
	}
	return nil
}

// ToCleanupPolicy converts a NamedPolicy to the repo.CleanupPolicy type used by
// Run and DryRun.
func (p NamedPolicy) ToCleanupPolicy() *repo.CleanupPolicy {
	return &repo.CleanupPolicy{
		KeepVersions:        p.KeepVersions,
		KeepReleasesOnly:    p.KeepReleasesOnly,
		DeleteSnapshotsDays: p.DeleteSnapshotsDays,
		DeleteOlderThanDays: p.DeleteOlderThanDays,
		LastDownloadedDays:  p.LastDownloadedDays,
		Interval:            p.Interval,
	}
}

// PolicyManager persists named cleanup policies to meta.Store.
type PolicyManager struct {
	meta meta.Store
}

func NewPolicyManager(m meta.Store) *PolicyManager {
	return &PolicyManager{meta: m}
}

func (pm *PolicyManager) Get(name string) (NamedPolicy, bool, error) {
	var p NamedPolicy
	ok, err := pm.meta.GetJSON(policyNS, name, &p)
	return p, ok, err
}

func (pm *PolicyManager) List() ([]NamedPolicy, error) {
	keys, err := pm.meta.List(policyNS)
	if err != nil {
		return nil, err
	}
	var out []NamedPolicy
	for _, k := range keys {
		var p NamedPolicy
		if ok, _ := pm.meta.GetJSON(policyNS, k, &p); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// Put creates or replaces a named policy. The name must match [a-z0-9][a-z0-9-]{0,62}.
func (pm *PolicyManager) Put(p NamedPolicy) error {
	if !validPolicyName.MatchString(p.Name) {
		return fmt.Errorf("cleanup-policies: name %q must match [a-z0-9][a-z0-9-]{0,62}", p.Name)
	}
	return pm.meta.PutJSON(policyNS, p.Name, p)
}

func (pm *PolicyManager) Delete(name string) error {
	return pm.meta.Delete(policyNS, name)
}

// Reclaimable returns the total bytes that would be freed if all enabled
// policies were applied now, computed by running a dry-run across every
// hosted repo that has a cleanup policy assigned.
func Reclaimable(pm *PolicyManager, repos *repo.Manager, b blob.Store, m meta.Store) int64 {
	var total int64
	for _, r := range repos.All() {
		if r.CleanupPolicyName == "" || r.Kind == repo.Group {
			continue
		}
		np, ok, err := pm.Get(r.CleanupPolicyName)
		if err != nil || !ok {
			continue
		}
		result, err := DryRunForRepo(r, np.ToCleanupPolicy(), b, m)
		if err != nil {
			continue
		}
		for _, c := range result.Candidates {
			total += c.SizeBytes
		}
	}
	return total
}

func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
