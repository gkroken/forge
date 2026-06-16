// Package repo defines the repository model shared by every format.
//
// A Repository is the unit Nexus calls a "repo": it has a name, a Format
// (maven/npm/helm/cran/oci), and a Kind:
//
//	hosted - you publish into it; it is the source of truth
//	proxy  - read-through cache of an upstream registry
//	group  - a merged read-only view over several members
// Package repo defines the repository model shared by every format.
//
// A Repository is the unit Nexus calls a "repo": it has a name, a Format
// (maven/npm/helm/cran/oci), and a Kind:
//
//	hosted - you publish into it; it is the source of truth
//	proxy  - read-through cache of an upstream registry
//	group  - a merged read-only view over several members
package repo

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Kind string

const (
	Hosted Kind = "hosted"
	Proxy  Kind = "proxy"
	Group  Kind = "group"
)

// CleanupPolicy defines automated artifact retention rules for a hosted
// repository. All fields are optional; zero/false means "no limit".
type CleanupPolicy struct {
	// KeepVersions retains only the N most recent versions of each
	// artifact/package/chart. Versions are compared as strings; for Maven
	// the path-level version directory name is used.
	KeepVersions int `json:"keepVersions,omitempty"`

	// KeepReleasesOnly deletes all pre-release versions: Maven SNAPSHOTs
	// (version contains "-SNAPSHOT") and npm pre-releases (version contains
	// a pre-release separator like "-alpha", "-beta", "-rc").
	KeepReleasesOnly bool `json:"keepReleasesOnly,omitempty"`

	// DeleteSnapshotsDays deletes SNAPSHOT versions (Maven) or pre-release
	// versions (npm) that were uploaded more than N days ago. Requires
	// upload timestamps — only applies to artifacts published after this
	// field was introduced.
	DeleteSnapshotsDays int `json:"deleteSnapshotsDays,omitempty"`

	// DeleteOlderThanDays deletes any artifact uploaded more than N days ago.
	// Requires upload timestamps — only applies to artifacts published after
	// this field was introduced.
	DeleteOlderThanDays int `json:"deleteOlderThanDays,omitempty"`

	// Interval is how often the cleanup policy runs automatically. When set
	// (e.g. "24h", "168h"), the background scheduler fires cleanup.Run on
	// this cadence. Zero means manual-only (POST /api/v1/repos/{name}/cleanup).
	Interval time.Duration `json:"-"`
}

// MarshalJSON serialises Interval as a human-readable string (e.g. "24h").
func (p CleanupPolicy) MarshalJSON() ([]byte, error) {
	type Alias CleanupPolicy
	return json.Marshal(&struct {
		Alias
		Interval string `json:"interval,omitempty"`
	}{
		Alias:    Alias(p),
		Interval: durationString(p.Interval),
	})
}

// UnmarshalJSON parses Interval back from a string.
func (p *CleanupPolicy) UnmarshalJSON(data []byte) error {
	type Alias CleanupPolicy
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
			return fmt.Errorf("invalid cleanup interval %q: %w", aux.Interval, err)
		}
		p.Interval = d
	}
	return nil
}

type Repository struct {
	Name          string         `json:"name"`
	Format        string         `json:"format"`
	Kind          Kind           `json:"kind"`
	Upstream      string         `json:"upstream,omitempty"`
	Members       []string       `json:"members,omitempty"`
	AnonymousRead bool           `json:"anonymousRead"`
	ProxyTTL      time.Duration  `json:"proxyTTL,omitempty"`
	ProxyAuth     string         `json:"proxyAuth,omitempty"`
	CleanupPolicy *CleanupPolicy `json:"cleanupPolicy,omitempty"`
}

// metaStore is the minimal interface Manager needs for persistence.
// Satisfied by meta.Store; declared here to avoid an import cycle.
type metaStore interface {
	GetJSON(ns, key string, v any) (bool, error)
	PutJSON(ns, key string, v any) error
	List(ns string) ([]string, error)
	Delete(ns, key string) error
}

const repoNS = "admin:repos"

// Manager is the in-memory + persistent registry of configured repositories.
// It is safe for concurrent use.
type Manager struct {
	mu     sync.RWMutex
	byName map[string]Repository
	store  metaStore // nil = in-memory only (eval / tests)
}

func NewManager() *Manager {
	return &Manager{byName: map[string]Repository{}}
}

// WithStore attaches a meta.Store for persistence and loads any previously
// saved repositories.  Call before seeding defaults so existing persisted
// repos are not duplicated.
func (m *Manager) WithStore(s metaStore) error {
	m.store = s
	keys, err := s.List(repoNS)
	if err != nil {
		return fmt.Errorf("repo.Manager: load: %w", err)
	}
	for _, k := range keys {
		var r Repository
		if ok, _ := s.GetJSON(repoNS, k, &r); ok {
			m.byName[r.Name] = r
		}
	}
	return nil
}

// Len returns the number of configured repositories.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byName)
}

func (m *Manager) Add(r Repository) error {
	if r.Name == "" || r.Format == "" {
		return fmt.Errorf("repository needs name and format")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byName[r.Name]; exists {
		return fmt.Errorf("repository %q already exists", r.Name)
	}
	m.byName[r.Name] = r
	if m.store != nil {
		return m.store.PutJSON(repoNS, r.Name, r)
	}
	return nil
}

// Update replaces an existing repository's configuration.
func (m *Manager) Update(r Repository) error {
	if r.Name == "" {
		return fmt.Errorf("name required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byName[r.Name]; !exists {
		return fmt.Errorf("repository %q not found", r.Name)
	}
	m.byName[r.Name] = r
	if m.store != nil {
		return m.store.PutJSON(repoNS, r.Name, r)
	}
	return nil
}

// Delete removes a repository by name.
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byName[name]; !exists {
		return fmt.Errorf("repository %q not found", name)
	}
	delete(m.byName, name)
	if m.store != nil {
		return m.store.Delete(repoNS, name)
	}
	return nil
}

func (m *Manager) Get(name string) (Repository, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.byName[name]
	return r, ok
}

func (m *Manager) All() []Repository {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Repository, 0, len(m.byName))
	for _, r := range m.byName {
		out = append(out, r)
	}
	return out
}

// MarshalJSON implements json.Marshaler so time.Duration fields are serialised
// as human-readable strings (e.g. "24h") rather than nanosecond integers.
func (r Repository) MarshalJSON() ([]byte, error) {
	type Alias Repository
	return json.Marshal(&struct {
		Alias
		ProxyTTL string `json:"proxyTTL,omitempty"`
	}{
		Alias:    Alias(r),
		ProxyTTL: durationString(r.ProxyTTL),
	})
}

// UnmarshalJSON implements json.Unmarshaler to parse ProxyTTL back from string.
func (r *Repository) UnmarshalJSON(data []byte) error {
	type Alias Repository
	aux := &struct {
		*Alias
		ProxyTTL string `json:"proxyTTL,omitempty"`
	}{Alias: (*Alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.ProxyTTL != "" {
		d, err := time.ParseDuration(aux.ProxyTTL)
		if err != nil {
			return fmt.Errorf("invalid proxyTTL %q: %w", aux.ProxyTTL, err)
		}
		r.ProxyTTL = d
	}
	return nil
}

func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
