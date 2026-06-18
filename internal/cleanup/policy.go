package cleanup

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

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
	LastDownloadedDays  int           `json:"lastDownloadedDays,omitempty"` // no-op until download tracking lands
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
// Run and DryRun. LastDownloadedDays is omitted until download tracking lands.
func (p NamedPolicy) ToCleanupPolicy() *repo.CleanupPolicy {
	return &repo.CleanupPolicy{
		KeepVersions:        p.KeepVersions,
		KeepReleasesOnly:    p.KeepReleasesOnly,
		DeleteSnapshotsDays: p.DeleteSnapshotsDays,
		DeleteOlderThanDays: p.DeleteOlderThanDays,
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

func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
