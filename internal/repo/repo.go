// Package repo defines the repository model shared by every format.
//
// A Repository is the unit Nexus calls a "repo": it has a name, a Format
// (maven/npm/helm/cran), and a Kind:
//
//	hosted - you publish into it; it is the source of truth
//	proxy  - read-through cache of an upstream registry
//	group  - a merged read-only view over several members (not yet implemented)
package repo

import "fmt"

type Kind string

const (
	Hosted Kind = "hosted"
	Proxy  Kind = "proxy"
	Group  Kind = "group"
)

type Repository struct {
	Name     string
	Format   string // "maven", "npm", "helm", "cran"
	Kind     Kind
	Upstream string   // for Proxy: base URL of the remote registry
	Members  []string // for Group: ordered member repo names
}

// Manager is the in-memory registry of configured repositories.
// In production this is backed by a DB table and an admin API.
type Manager struct {
	byName map[string]Repository
}

func NewManager() *Manager {
	return &Manager{byName: map[string]Repository{}}
}

func (m *Manager) Add(r Repository) error {
	if r.Name == "" || r.Format == "" {
		return fmt.Errorf("repository needs name and format")
	}
	if _, exists := m.byName[r.Name]; exists {
		return fmt.Errorf("repository %q already exists", r.Name)
	}
	m.byName[r.Name] = r
	return nil
}

func (m *Manager) Get(name string) (Repository, bool) {
	r, ok := m.byName[name]
	return r, ok
}

func (m *Manager) All() []Repository {
	out := make([]Repository, 0, len(m.byName))
	for _, r := range m.byName {
		out = append(out, r)
	}
	return out
}
