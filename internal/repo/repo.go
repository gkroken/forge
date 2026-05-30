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

type Repository struct {
	Name          string        `json:"name"`
	Format        string        `json:"format"`
	Kind          Kind          `json:"kind"`
	Upstream      string        `json:"upstream,omitempty"`
	Members       []string      `json:"members,omitempty"`
	AnonymousRead bool          `json:"anonymousRead"`
	ProxyTTL      time.Duration `json:"proxyTTL,omitempty"`
	ProxyAuth     string        `json:"proxyAuth,omitempty"`
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
