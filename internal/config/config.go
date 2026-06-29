// Package config provides declarative configuration for forge: load a JSON
// file, validate it, diff it against current state, and reconcile (apply).
//
// The intended GitOps flow:
//  1. Author forge.config.json (or export current state with Export).
//  2. Commit the file and mount it as a ConfigMap.
//  3. forge -config /etc/forge/config.json reads it on every boot and converges.
//
// Secrets (webhook.Secret, repo.ProxyAuth) are injected via ${ENV_VAR}
// placeholders so they never need to be committed.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/cleanup"
	"forge/internal/meta"
	"forge/internal/repo"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

// File is the top-level shape of forge.config.json.
// All sections are optional; a partial file is valid and additive.
type File struct {
	Repositories     []repo.Repository      `json:"repositories,omitempty"`
	CleanupPolicies  []cleanup.NamedPolicy  `json:"cleanupPolicies,omitempty"`
	SecurityPolicies []vuln.NamedPolicy     `json:"securityPolicies,omitempty"`
	// SecurityDefault overrides the global vulnerability-gate default.
	SecurityDefault  *vuln.Policy           `json:"securityDefault,omitempty"`
	Roles            []auth.CustomRole      `json:"roles,omitempty"`
	Webhooks         []webhook.Subscription `json:"webhooks,omitempty"`
	// Prune deletes objects previously managed by this file but now absent.
	// Objects created via REST/UI are never pruned regardless of this flag.
	Prune bool `json:"prune,omitempty"`
}

// Appliers holds the managers that Apply writes through.
type Appliers struct {
	Repos    *repo.Manager
	Cleanup  *cleanup.PolicyManager
	Vuln     *vuln.PolicyManager
	Roles    auth.RoleStore // nil when auth is disabled
	Webhooks *webhook.Store
	Meta     meta.Store // for managed-set bookkeeping
}

// Result summarises what Apply or Plan found.
type Result struct {
	Repositories     KindResult
	CleanupPolicies  KindResult
	SecurityPolicies KindResult
	Roles            KindResult
	Webhooks         KindResult
	SecurityDefaultSet bool // true when the config specified SecurityDefault
}

// KindResult holds per-object-kind operation counts.
type KindResult struct {
	Created int
	Updated int
	Noop    int
	Deleted int
}

// Changes returns the number of write operations (Created+Updated+Deleted).
func (r KindResult) Changes() int { return r.Created + r.Updated + r.Deleted }

// Load reads the file at path, expands ${VAR} env-var placeholders, and
// unmarshals it as JSON. Referencing an undefined env var is an error.
func Load(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return File{}, fmt.Errorf("config: %w", err)
	}
	var f File
	if err := json.Unmarshal([]byte(expanded), &f); err != nil {
		return File{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return f, nil
}

// expandEnv replaces ${VAR} (and $VAR) with the named env var's value.
// Returns an error if any referenced variable is not set in the environment.
func expandEnv(s string) (string, error) {
	var missing string
	result := os.Expand(s, func(key string) string {
		v, ok := os.LookupEnv(key)
		if !ok && missing == "" {
			missing = key
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("env var %q is not set (use ${VAR} for secrets)", missing)
	}
	return result, nil
}

// managed-set: tracks which names were applied via config so Prune only
// removes objects it previously created, never UI/API-created ones.
const (
	managedNS  = "admin:config-managed"
	managedKey = "set"
)

type managedSet struct {
	Repositories     []string `json:"repositories,omitempty"`
	CleanupPolicies  []string `json:"cleanupPolicies,omitempty"`
	SecurityPolicies []string `json:"securityPolicies,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	Webhooks         []string `json:"webhooks,omitempty"`
}

func loadManaged(m meta.Store) managedSet {
	var ms managedSet
	_, _ = m.GetJSON(managedNS, managedKey, &ms)
	return ms
}

func saveManaged(m meta.Store, ms managedSet) error {
	return m.PutJSON(managedNS, managedKey, ms)
}

// Plan computes the diff between the desired File and current state.
// It reads from the managers but makes no writes.
func Plan(f File, a Appliers) (Result, error) {
	if err := validate(f, a); err != nil {
		return Result{}, err
	}

	var res Result
	managed := loadManaged(a.Meta)

	// Cleanup policies.
	cleanupDesired := strSet(f.CleanupPolicies, func(p cleanup.NamedPolicy) string { return p.Name })
	for _, p := range f.CleanupPolicies {
		existing, ok, _ := a.Cleanup.Get(p.Name)
		switch {
		case !ok:
			res.CleanupPolicies.Created++
		case jsonEqual(p, existing):
			res.CleanupPolicies.Noop++
		default:
			res.CleanupPolicies.Updated++
		}
	}
	if f.Prune {
		for _, name := range managed.CleanupPolicies {
			if !cleanupDesired[name] {
				res.CleanupPolicies.Deleted++
			}
		}
	}

	// Security policies.
	vulnDesired := strSet(f.SecurityPolicies, func(p vuln.NamedPolicy) string { return p.Name })
	for _, p := range f.SecurityPolicies {
		existing, ok, _ := a.Vuln.Get(p.Name)
		switch {
		case !ok:
			res.SecurityPolicies.Created++
		case jsonEqual(p, existing):
			res.SecurityPolicies.Noop++
		default:
			res.SecurityPolicies.Updated++
		}
	}
	if f.Prune {
		for _, name := range managed.SecurityPolicies {
			if !vulnDesired[name] {
				res.SecurityPolicies.Deleted++
			}
		}
	}
	if f.SecurityDefault != nil {
		res.SecurityDefaultSet = true
	}

	// Roles.
	if a.Roles != nil {
		rolesDesired := strSet(f.Roles, func(r auth.CustomRole) string { return r.Name })
		for _, r := range f.Roles {
			existing, ok, _ := a.Roles.Get(r.Name)
			switch {
			case !ok:
				res.Roles.Created++
			case jsonEqual(r, existing):
				res.Roles.Noop++
			default:
				res.Roles.Updated++
			}
		}
		if f.Prune {
			for _, name := range managed.Roles {
				if !rolesDesired[name] {
					res.Roles.Deleted++
				}
			}
		}
	}

	// Repositories.
	reposDesired := strSet(f.Repositories, func(r repo.Repository) string { return r.Name })
	for _, r := range f.Repositories {
		existing, ok := a.Repos.Get(r.Name)
		switch {
		case !ok:
			res.Repositories.Created++
		case jsonEqual(r, existing):
			res.Repositories.Noop++
		default:
			res.Repositories.Updated++
		}
	}
	if f.Prune {
		for _, name := range managed.Repositories {
			if !reposDesired[name] {
				res.Repositories.Deleted++
			}
		}
	}

	// Webhooks (matched by Name).
	subs, _ := a.Webhooks.List()
	byName := make(map[string]webhook.Subscription, len(subs))
	for _, s := range subs {
		byName[s.Name] = s
	}
	webhooksDesired := strSet(f.Webhooks, func(s webhook.Subscription) string { return s.Name })
	for _, s := range f.Webhooks {
		ex, ok := byName[s.Name]
		switch {
		case !ok:
			res.Webhooks.Created++
		case webhookEqual(mergeWebhook(s, ex), ex):
			res.Webhooks.Noop++
		default:
			res.Webhooks.Updated++
		}
	}
	if f.Prune {
		for _, name := range managed.Webhooks {
			if !webhooksDesired[name] {
				res.Webhooks.Deleted++
			}
		}
	}

	return res, nil
}

// validate checks cross-references in f against the file itself and the
// current store state. Returns a combined error listing all problems.
func validate(f File, a Appliers) error {
	cleanupInFile := strSet(f.CleanupPolicies, func(p cleanup.NamedPolicy) string { return p.Name })
	vulnInFile := strSet(f.SecurityPolicies, func(p vuln.NamedPolicy) string { return p.Name })
	reposInFile := strSet(f.Repositories, func(r repo.Repository) string { return r.Name })

	var errs []string
	for _, r := range f.Repositories {
		if r.Format == "" {
			errs = append(errs, fmt.Sprintf("repository %q: format is required", r.Name))
		}
		for _, m := range r.Members {
			if !reposInFile[m] {
				if _, ok := a.Repos.Get(m); !ok {
					errs = append(errs, fmt.Sprintf("repository %q: group member %q not found in file or store", r.Name, m))
				}
			}
		}
		if r.CleanupPolicyName != "" && !cleanupInFile[r.CleanupPolicyName] {
			if _, ok, _ := a.Cleanup.Get(r.CleanupPolicyName); !ok {
				errs = append(errs, fmt.Sprintf("repository %q: cleanup policy %q not found in file or store", r.Name, r.CleanupPolicyName))
			}
		}
		if r.SecurityPolicyName != "" && !vulnInFile[r.SecurityPolicyName] {
			if _, ok, _ := a.Vuln.Get(r.SecurityPolicyName); !ok {
				errs = append(errs, fmt.Sprintf("repository %q: security policy %q not found in file or store", r.Name, r.SecurityPolicyName))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Apply reconciles the current state to match f. It is idempotent.
// Dependency order: roles → cleanup policies → security policies (+default) → repositories → webhooks.
func Apply(f File, a Appliers) (Result, error) {
	if err := validate(f, a); err != nil {
		return Result{}, err
	}

	var res Result
	managed := loadManaged(a.Meta)
	snapshot := managed // copy before mutation (for prune)

	// 1. Roles (RoleStore has no Update — delete+recreate for changes).
	if a.Roles != nil {
		for _, r := range f.Roles {
			existing, ok, _ := a.Roles.Get(r.Name)
			if !ok {
				if err := a.Roles.Create(r); err != nil {
					return res, fmt.Errorf("config: create role %q: %w", r.Name, err)
				}
				res.Roles.Created++
			} else if !jsonEqual(r, existing) {
				if err := a.Roles.Delete(r.Name); err != nil {
					return res, fmt.Errorf("config: update role %q (delete): %w", r.Name, err)
				}
				if err := a.Roles.Create(r); err != nil {
					return res, fmt.Errorf("config: update role %q (recreate): %w", r.Name, err)
				}
				res.Roles.Updated++
			} else {
				res.Roles.Noop++
			}
		}
		if f.Prune {
			desired := strSet(f.Roles, func(r auth.CustomRole) string { return r.Name })
			for _, name := range snapshot.Roles {
				if !desired[name] {
					if err := a.Roles.Delete(name); err != nil {
						return res, fmt.Errorf("config: prune role %q: %w", name, err)
					}
					res.Roles.Deleted++
				}
			}
		}
		managed.Roles = strSlice(f.Roles, func(r auth.CustomRole) string { return r.Name })
	}

	// 2. Cleanup policies.
	for _, p := range f.CleanupPolicies {
		existing, ok, _ := a.Cleanup.Get(p.Name)
		if !ok {
			if err := a.Cleanup.Put(p); err != nil {
				return res, fmt.Errorf("config: create cleanup policy %q: %w", p.Name, err)
			}
			res.CleanupPolicies.Created++
		} else if !jsonEqual(p, existing) {
			if err := a.Cleanup.Put(p); err != nil {
				return res, fmt.Errorf("config: update cleanup policy %q: %w", p.Name, err)
			}
			res.CleanupPolicies.Updated++
		} else {
			res.CleanupPolicies.Noop++
		}
	}
	if f.Prune {
		desired := strSet(f.CleanupPolicies, func(p cleanup.NamedPolicy) string { return p.Name })
		for _, name := range snapshot.CleanupPolicies {
			if !desired[name] {
				if err := a.Cleanup.Delete(name); err != nil {
					return res, fmt.Errorf("config: prune cleanup policy %q: %w", name, err)
				}
				res.CleanupPolicies.Deleted++
			}
		}
	}
	managed.CleanupPolicies = strSlice(f.CleanupPolicies, func(p cleanup.NamedPolicy) string { return p.Name })

	// 3. Security policies + optional global default.
	for _, p := range f.SecurityPolicies {
		existing, ok, _ := a.Vuln.Get(p.Name)
		if !ok {
			if err := a.Vuln.Put(p); err != nil {
				return res, fmt.Errorf("config: create security policy %q: %w", p.Name, err)
			}
			res.SecurityPolicies.Created++
		} else if !jsonEqual(p, existing) {
			if err := a.Vuln.Put(p); err != nil {
				return res, fmt.Errorf("config: update security policy %q: %w", p.Name, err)
			}
			res.SecurityPolicies.Updated++
		} else {
			res.SecurityPolicies.Noop++
		}
	}
	if f.Prune {
		desired := strSet(f.SecurityPolicies, func(p vuln.NamedPolicy) string { return p.Name })
		for _, name := range snapshot.SecurityPolicies {
			if !desired[name] {
				if err := a.Vuln.Delete(name); err != nil {
					return res, fmt.Errorf("config: prune security policy %q: %w", name, err)
				}
				res.SecurityPolicies.Deleted++
			}
		}
	}
	managed.SecurityPolicies = strSlice(f.SecurityPolicies, func(p vuln.NamedPolicy) string { return p.Name })
	if f.SecurityDefault != nil {
		if err := a.Vuln.SetDefault(*f.SecurityDefault); err != nil {
			return res, fmt.Errorf("config: set security default: %w", err)
		}
		res.SecurityDefaultSet = true
	}

	// 4. Repositories.
	for _, r := range f.Repositories {
		existing, ok := a.Repos.Get(r.Name)
		if !ok {
			if err := a.Repos.Add(r); err != nil {
				return res, fmt.Errorf("config: create repo %q: %w", r.Name, err)
			}
			res.Repositories.Created++
		} else if !jsonEqual(r, existing) {
			if err := a.Repos.Update(r); err != nil {
				return res, fmt.Errorf("config: update repo %q: %w", r.Name, err)
			}
			res.Repositories.Updated++
		} else {
			res.Repositories.Noop++
		}
	}
	if f.Prune {
		desired := strSet(f.Repositories, func(r repo.Repository) string { return r.Name })
		for _, name := range snapshot.Repositories {
			if !desired[name] {
				if err := a.Repos.Delete(name); err != nil {
					return res, fmt.Errorf("config: prune repo %q: %w", name, err)
				}
				res.Repositories.Deleted++
			}
		}
	}
	managed.Repositories = strSlice(f.Repositories, func(r repo.Repository) string { return r.Name })

	// 5. Webhooks (reconciled by Name; ID is server-assigned).
	existing, err := a.Webhooks.List()
	if err != nil {
		return res, fmt.Errorf("config: list webhooks: %w", err)
	}
	byName := make(map[string]webhook.Subscription, len(existing))
	for _, s := range existing {
		byName[s.Name] = s
	}
	for _, s := range f.Webhooks {
		ex, ok := byName[s.Name]
		if !ok {
			if _, err := a.Webhooks.Create(s); err != nil {
				return res, fmt.Errorf("config: create webhook %q: %w", s.Name, err)
			}
			res.Webhooks.Created++
		} else {
			merged := mergeWebhook(s, ex)
			if !webhookEqual(merged, ex) {
				if _, err := a.Webhooks.Update(merged); err != nil {
					return res, fmt.Errorf("config: update webhook %q: %w", s.Name, err)
				}
				res.Webhooks.Updated++
			} else {
				res.Webhooks.Noop++
			}
		}
	}
	if f.Prune {
		desired := strSet(f.Webhooks, func(s webhook.Subscription) string { return s.Name })
		for _, name := range snapshot.Webhooks {
			if !desired[name] {
				if sub, ok := byName[name]; ok {
					if err := a.Webhooks.Delete(sub.ID); err != nil {
						return res, fmt.Errorf("config: prune webhook %q: %w", name, err)
					}
					res.Webhooks.Deleted++
				}
			}
		}
	}
	managed.Webhooks = strSlice(f.Webhooks, func(s webhook.Subscription) string { return s.Name })

	if err := saveManaged(a.Meta, managed); err != nil {
		return res, fmt.Errorf("config: save managed set: %w", err)
	}
	return res, nil
}

// Export dumps the current state to a File. Secrets (webhook.Secret,
// repo.ProxyAuth) are blanked — re-supply them via ${ENV_VAR} on import.
func Export(a Appliers) (File, error) {
	var f File

	f.Repositories = a.Repos.All()
	for i := range f.Repositories {
		f.Repositories[i].ProxyAuth = ""
	}

	cp, err := a.Cleanup.List()
	if err != nil {
		return File{}, fmt.Errorf("config export: cleanup policies: %w", err)
	}
	f.CleanupPolicies = cp

	sp, err := a.Vuln.List()
	if err != nil {
		return File{}, fmt.Errorf("config export: security policies: %w", err)
	}
	f.SecurityPolicies = sp

	def, err := a.Vuln.Default()
	if err != nil {
		return File{}, fmt.Errorf("config export: security default: %w", err)
	}
	f.SecurityDefault = &def

	if a.Roles != nil {
		roles, err := a.Roles.List()
		if err != nil {
			return File{}, fmt.Errorf("config export: roles: %w", err)
		}
		f.Roles = roles
	}

	subs, err := a.Webhooks.List()
	if err != nil {
		return File{}, fmt.Errorf("config export: webhooks: %w", err)
	}
	for i := range subs {
		subs[i].Secret = "" // blanked; re-supply via ${WEBHOOK_SECRET}
	}
	f.Webhooks = subs

	return f, nil
}

// mergeWebhook copies server-owned fields from existing into desired and
// preserves the existing secret when desired.Secret is blank.
func mergeWebhook(desired, existing webhook.Subscription) webhook.Subscription {
	desired.ID = existing.ID
	desired.CreatedAt = existing.CreatedAt
	if desired.Secret == "" {
		desired.Secret = existing.Secret
	}
	return desired
}

// webhookEqual compares two subscriptions, ignoring server-owned fields (ID,
// CreatedAt). Both inputs should have those fields normalised via mergeWebhook.
func webhookEqual(a, b webhook.Subscription) bool {
	a.ID, b.ID = "", ""
	a.CreatedAt = time.Time{}
	b.CreatedAt = time.Time{}
	return jsonEqual(a, b)
}

// jsonEqual reports whether a and b produce identical JSON. Used instead of
// reflect.DeepEqual to handle nil-vs-empty-slice and pointer field edge cases.
func jsonEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

func strSet[T any](items []T, key func(T) string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[key(item)] = true
	}
	return m
}

func strSlice[T any](items []T, key func(T) string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = key(item)
	}
	return out
}
