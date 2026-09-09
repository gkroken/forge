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
	"log/slog"
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

	// LastDownloadedDays deletes artifacts whose most recent download (falling
	// back to upload time when never downloaded) is more than N days ago.
	// Artifacts with neither a download nor an upload timestamp are skipped.
	LastDownloadedDays int `json:"lastDownloadedDays,omitempty"`

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
	Name              string        `json:"name"`
	Format            string        `json:"format"`
	Kind              Kind          `json:"kind"`
	Upstream          string        `json:"upstream,omitempty"`
	Members           []string      `json:"members,omitempty"`
	AnonymousRead     bool          `json:"anonymousRead"`
	ProxyTTL          time.Duration `json:"-"` // legacy; populated from JSON; use ContentMaxAge for new code
	ProxyAuth         string        `json:"proxyAuth,omitempty"`
	CleanupPolicyName string        `json:"cleanupPolicyName,omitempty"`
	// SecurityPolicyName references a named vuln.NamedPolicy. Empty = inherit the
	// global default vulnerability policy. Admin-set only.
	SecurityPolicyName string `json:"securityPolicyName,omitempty"`

	// Claims lists namespace patterns (internal/selector grammar, e.g.
	// "@acme/**", "com/acme/**") that this HOSTED repository owns for
	// dependency-confusion protection. A claimed name is never served from a
	// proxy: proxy members of any group containing this repo skip it, and
	// direct requests to same-format proxy repos are refused (403). Claims
	// protect names before their first publish; names actually present in a
	// hosted group member are protected automatically without a claim.
	// Only meaningful on hosted repos; validated at write time.
	Claims []string `json:"claims,omitempty"`

	// DepConfusionGuard toggles dependency-confusion enforcement on GROUP and
	// PROXY repositories. nil means enabled (secure default); set to false to
	// restore unguarded proxy fall-through / packument merging.
	DepConfusionGuard *bool `json:"depConfusionGuard,omitempty"`

	// Enabled=false makes the server return 503 for all requests to this repo.
	// Existing repos without this field serialised default to true (see UnmarshalJSON).
	Enabled        bool           `json:"enabled"`
	BlobStore      string         `json:"blobStore,omitempty"`     // named store; "" = default
	ContentMaxAge  *time.Duration `json:"-"`                       // serialised as "contentMaxAge" string
	MetadataMaxAge *time.Duration `json:"-"`                       // serialised as "metadataMaxAge" string
	NegativeCache  *bool          `json:"negativeCache,omitempty"` // nil = global default (true)
	AutoBlock      *bool          `json:"autoBlock,omitempty"`     // nil = global default (true)
	TimeoutSecs    *int           `json:"timeoutSecs,omitempty"`   // nil = 30s
	Retries        *int           `json:"retries,omitempty"`       // nil = DefaultMaxRetries (2)
	QuotaGB        *float64       `json:"quotaGB,omitempty"`       // nil = unlimited

	// Immutable makes a HOSTED repository write-once: an existing
	// component+version can never be overwritten (the blob store rejects a Put
	// to an occupied key with blob.ErrImmutable → HTTP 409), soft-delete is
	// refused (which would let a delete-then-republish change released bytes),
	// and automated cleanup does not run (retaining N versions contradicts
	// write-once). Publishing a NEW version is still allowed. This is the
	// natural target for a promoted release: once promoted, it cannot be
	// mutated. Only meaningful on hosted repos; ignored on proxy/group.
	Immutable bool `json:"immutable,omitempty"`
}

// IsImmutable reports whether write-once enforcement is in force for this
// repository (hosted repos only).
func (r Repository) IsImmutable() bool { return r.Immutable && r.Kind == Hosted }

// DepGuardEnabled reports whether dependency-confusion protection is in force
// for this (group or proxy) repository. Unset means enabled.
func (r Repository) DepGuardEnabled() bool {
	return r.DepConfusionGuard == nil || *r.DepConfusionGuard
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
	m.closePublicGroupsOverPrivateMembers()
	return nil
}

// PolicyViolation reports why r must not exist alongside the repositories
// already configured, or "" when it is fine.
//
// These are invariants about the set of repositories, so they live here rather
// than in an HTTP handler. The admin API used to own them, and every other way
// of creating a repository — the browser admin form, a Config-as-Code apply, a
// Nexus migration, and forge's own startup seed — went straight to Add and
// inherited none of them. With -auth the seeded groups were anonymousRead=true
// over private hosted members, so an anonymous client read private artifacts
// through the group while the same request to the member itself answered 401.
func (m *Manager) PolicyViolation(r Repository) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.policyViolation(r)
}

// policyViolation assumes the caller holds m.mu.
func (m *Manager) policyViolation(r Repository) string {
	if r.Kind == Group {
		for _, memberName := range r.Members {
			if memberName == r.Name {
				return fmt.Sprintf("group %q lists itself as a member", r.Name)
			}
			// A group cannot contain a group: MemberCtx refuses a group member,
			// so a nested one contributed nothing and the group answered 404
			// for packages its inner group served, with no error and no log.
			if member, ok := m.byName[memberName]; ok && member.Kind == Group {
				return fmt.Sprintf(
					"group %q lists group %q as a member: groups cannot be nested, and a "+
						"nested member would silently contribute nothing. List %q's members "+
						"directly instead.",
					r.Name, memberName, memberName)
			}
		}
		if r.AnonymousRead {
			for _, memberName := range r.Members {
				member, ok := m.byName[memberName]
				if !ok {
					continue // unknown member — the handler answers 404 for it
				}
				if !member.AnonymousRead {
					return fmt.Sprintf(
						"group %q has anonymousRead=true but member %q has anonymousRead=false: "+
							"anonymous clients would read private content through the group",
						r.Name, memberName)
				}
			}
		}
		return ""
	}

	// The same rule from the other side: a repository cannot go private while a
	// public group still lists it, or the group keeps serving it to anonymous
	// clients.
	if !r.AnonymousRead {
		for _, g := range m.byName {
			if g.Kind != Group || !g.AnonymousRead || g.Name == r.Name {
				continue
			}
			for _, memberName := range g.Members {
				if memberName == r.Name {
					return fmt.Sprintf(
						"cannot set anonymousRead=false on %q: public group %q would expose it to "+
							"anonymous clients; update the group first",
						r.Name, g.Name)
				}
			}
		}
	}
	return ""
}

// closePublicGroupsOverPrivateMembers downgrades any loaded group that is
// public over a private member. A stored configuration predating the check, or
// written by a path that skipped it, would otherwise keep serving private
// content to anonymous clients. Failing closed beats refusing to start: the
// operator keeps their install and gets a warning until they fix it.
func (m *Manager) closePublicGroupsOverPrivateMembers() {
	for name, g := range m.byName {
		if g.Kind != Group || !g.AnonymousRead {
			continue
		}
		for _, memberName := range g.Members {
			member, ok := m.byName[memberName]
			if ok && !member.AnonymousRead {
				g.AnonymousRead = false
				m.byName[name] = g
				slog.Warn("group had anonymous read over a private member; anonymous read disabled for this run",
					"group", name, "member", memberName)
				break
			}
		}
	}
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
	if msg := m.policyViolation(r); msg != "" {
		return fmt.Errorf("%s", msg)
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
	if msg := m.policyViolation(r); msg != "" {
		return fmt.Errorf("%s", msg)
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

// MarshalJSON serialises duration fields as human-readable strings and omits
// the zero-value ProxyTTL to avoid persisting 0ns.
func (r Repository) MarshalJSON() ([]byte, error) {
	type Alias Repository
	return json.Marshal(&struct {
		Alias
		ProxyTTL       string `json:"proxyTTL,omitempty"`
		ContentMaxAge  string `json:"contentMaxAge,omitempty"`
		MetadataMaxAge string `json:"metadataMaxAge,omitempty"`
	}{
		Alias:          Alias(r),
		ProxyTTL:       durationString(r.ProxyTTL),
		ContentMaxAge:  durationPtrString(r.ContentMaxAge),
		MetadataMaxAge: durationPtrString(r.MetadataMaxAge),
	})
}

// UnmarshalJSON parses duration strings back to time.Duration values and
// defaults Enabled=true so existing repos without the field are not taken
// offline after an upgrade.
func (r *Repository) UnmarshalJSON(data []byte) error {
	r.Enabled = true // backward compat: repos with no "enabled" field stay online
	type Alias Repository
	aux := &struct {
		*Alias
		ProxyTTL       string `json:"proxyTTL,omitempty"`
		ContentMaxAge  string `json:"contentMaxAge,omitempty"`
		MetadataMaxAge string `json:"metadataMaxAge,omitempty"`
	}{Alias: (*Alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	switch {
	case aux.ContentMaxAge != "":
		d, err := time.ParseDuration(aux.ContentMaxAge)
		if err != nil {
			return fmt.Errorf("invalid contentMaxAge %q: %w", aux.ContentMaxAge, err)
		}
		r.ContentMaxAge = &d
	case aux.ProxyTTL != "":
		d, err := time.ParseDuration(aux.ProxyTTL)
		if err != nil {
			return fmt.Errorf("invalid proxyTTL %q: %w", aux.ProxyTTL, err)
		}
		r.ProxyTTL = d
		r.ContentMaxAge = &d // copy into new field for callers using ContentMaxAge
	}
	if aux.MetadataMaxAge != "" {
		d, err := time.ParseDuration(aux.MetadataMaxAge)
		if err != nil {
			return fmt.Errorf("invalid metadataMaxAge %q: %w", aux.MetadataMaxAge, err)
		}
		r.MetadataMaxAge = &d
	}
	return nil
}

func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func durationPtrString(d *time.Duration) string {
	if d == nil || *d == 0 {
		return ""
	}
	return d.String()
}
