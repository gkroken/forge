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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"forge/internal/auth"
	"forge/internal/cleanup"
	"forge/internal/ldap"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

// File is the top-level shape of forge.config.json.
// All sections are optional; a partial file is valid and additive.
type File struct {
	Repositories     []repo.Repository     `json:"repositories,omitempty"`
	CleanupPolicies  []cleanup.NamedPolicy `json:"cleanupPolicies,omitempty"`
	SecurityPolicies []vuln.NamedPolicy    `json:"securityPolicies,omitempty"`
	// SecurityDefault overrides the global vulnerability-gate default.
	SecurityDefault *vuln.Policy           `json:"securityDefault,omitempty"`
	Roles           []auth.CustomRole      `json:"roles,omitempty"`
	Webhooks        []webhook.Subscription `json:"webhooks,omitempty"`
	// LDAP declares the directory-authentication settings honored on boot in
	// -config mode (an alternative to the -ldap-* flags; the block wins when both
	// are set). The bind password should be supplied via ${ENV_VAR}. It is a
	// runtime wiring input, not a managed object set, so Apply validates it but
	// does not reconcile it, and Export omits it (like OIDC).
	LDAP *ldap.Config `json:"ldap,omitempty"`
	// Prune deletes objects previously managed by this file but now absent.
	// Objects created via REST/UI are never pruned regardless of this flag.
	Prune bool `json:"prune,omitempty"`
	// Adopt permits taking ownership of a pre-existing object that this file has
	// never managed AND whose fields differ from the desired state — overwriting
	// whatever the UI/API put there. Adopting an object that already matches is
	// always allowed and needs no flag.
	//
	// Modelled on `kubectl apply --force-conflicts`: ownership transfer is never
	// the default, because a silent overwrite is how a config commit quietly
	// reverts somebody's console change. Every forced adoption is audited.
	Adopt bool `json:"adopt,omitempty"`
}

// Appliers holds the managers that Apply writes through.
type Appliers struct {
	Repos    *repo.Manager
	Cleanup  *cleanup.PolicyManager
	Vuln     *vuln.PolicyManager
	Roles    auth.RoleStore // nil when auth is disabled
	Webhooks *webhook.Store // nil when webhooks are not configured
	Meta     meta.Store     // for managed-set bookkeeping
	Audit    obs.AuditSink  // optional; records forced adoptions
}

// Result summarises what Apply or Plan found.
type Result struct {
	Repositories       KindResult
	CleanupPolicies    KindResult
	SecurityPolicies   KindResult
	Roles              KindResult
	Webhooks           KindResult
	SecurityDefaultSet bool // true when the config specified SecurityDefault
	LDAPConfigured     bool // true when the config specified an ldap block
	// Conflicts lists objects that exist but have never been managed by this
	// file and whose fields differ from it. Apply refuses unless File.Adopt is
	// set; with Adopt they are reported here and force-adopted.
	Conflicts []Conflict
}

// Conflict describes one object whose adoption would overwrite unmanaged state.
type Conflict struct {
	Kind   string   `json:"kind"` // "repository", "role", "cleanupPolicy", ...
	Name   string   `json:"name"`
	Fields []string `json:"fields"` // differing field names, sorted
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s %q differs in: %s", c.Kind, c.Name, strings.Join(c.Fields, ", "))
}

// KindResult holds per-object-kind operation counts.
type KindResult struct {
	Created int
	Updated int
	Noop    int
	Deleted int
	// Adopted counts objects that existed but were not previously managed by
	// this file and have now been taken under management.
	Adopted int
}

// Changes returns the number of write operations
// (Created+Updated+Deleted+Adopted).
func (r KindResult) Changes() int { return r.Created + r.Updated + r.Deleted + r.Adopted }

// Load reads the file at path, expands ${VAR} env-var placeholders, and
// unmarshals it. The format is chosen by extension: .yaml/.yml parse as YAML,
// anything else as JSON. Referencing an undefined env var is an error.
//
// Placeholder expansion runs on the raw text before parsing, so ${VAR} secret
// indirection behaves identically in both formats.
func Load(path string) (File, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied -config file, not client input
	if err != nil {
		return File{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return File{}, fmt.Errorf("config: %w", err)
	}
	asYAML := IsYAMLPath(path)
	// Reject unknown keys before decoding: encoding/json would discard them
	// silently, and a typo in a source-of-truth file must not be a no-op.
	if err := CheckKeys([]byte(expanded), asYAML); err != nil {
		return File{}, fmt.Errorf("config: %s: %w", path, err)
	}
	f, err := Unmarshal([]byte(expanded), asYAML)
	if err != nil {
		return File{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return f, nil
}

// IsYAMLPath reports whether path should be parsed as YAML, based on its
// extension. JSON is a subset of YAML 1.2, so a .json file parsed as YAML would
// also succeed; the extension check keeps error messages format-accurate.
func IsYAMLPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// Unmarshal decodes a config document. When asYAML is set the bytes are parsed
// as YAML (converted to JSON internally, so every existing json struct tag is
// honoured unchanged); otherwise as JSON.
func Unmarshal(data []byte, asYAML bool) (File, error) {
	var f File
	if asYAML {
		// sigs.k8s.io/yaml converts YAML->JSON and delegates to encoding/json,
		// so File and every nested type keep their existing `json:` tags.
		if err := yaml.Unmarshal(data, &f); err != nil {
			return File{}, err
		}
		return f, nil
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, err
	}
	return f, nil
}

// Marshal encodes f as YAML or JSON. JSON output is indented to match what
// -config-export has always produced.
func Marshal(f File, asYAML bool) ([]byte, error) {
	if asYAML {
		return yaml.Marshal(f)
	}
	return json.MarshalIndent(f, "", "  ")
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

// Object kinds, used by Conflict.Kind and Ownership.Owns. These are the five
// kinds a config file can manage.
const (
	KindRepository     = "repository"
	KindRole           = "role"
	KindCleanupPolicy  = "cleanupPolicy"
	KindSecurityPolicy = "securityPolicy"
	KindWebhook        = "webhook"
)

// Ownership answers "does the config file own this object?" for callers outside
// this package — chiefly the admin API, which refuses writes to config-owned
// objects. It is a point-in-time snapshot of the managed set.
type Ownership struct {
	idx   managedIndex
	empty bool
}

// LoadOwnership reads the managed set. A store with no managed set yet (forge
// has never run an Apply) yields an Ownership that owns nothing.
func LoadOwnership(m meta.Store) Ownership {
	ms := loadManaged(m)
	o := Ownership{idx: ms.index()}
	o.empty = len(ms.Repositories) == 0 && len(ms.Roles) == 0 &&
		len(ms.CleanupPolicies) == 0 && len(ms.SecurityPolicies) == 0 &&
		len(ms.Webhooks) == 0
	return o
}

// Owns reports whether the named object of the given kind is config-managed.
// An unknown kind is never owned — callers must not be able to accidentally
// lock down an object kind config does not actually manage.
func (o Ownership) Owns(kind, name string) bool {
	switch kind {
	case KindRepository:
		return o.idx.repos[name]
	case KindRole:
		return o.idx.roles[name]
	case KindCleanupPolicy:
		return o.idx.cleanup[name]
	case KindSecurityPolicy:
		return o.idx.vuln[name]
	case KindWebhook:
		return o.idx.webhooks[name]
	default:
		return false
	}
}

// Empty reports whether nothing at all is config-managed.
func (o Ownership) Empty() bool { return o.empty }

// disposition is what Apply should do with one desired object. Plan and Apply
// both derive it from classify, so they can never disagree about an object.
type disposition int

const (
	dispCreate        disposition = iota // absent from the store
	dispUpdate                           // present, config-managed, differs
	dispNoop                             // present, config-managed, identical
	dispAdopt                            // present, NOT managed, identical -> free
	dispAdoptConflict                    // present, NOT managed, differs -> needs Adopt
)

// classify decides an object's disposition from three facts: does it exist,
// has this config file managed it before, and does it match the desired state.
//
// The managed distinction is the whole point: without it a UI-created object
// silently becomes config-owned on the next boot and its settings are
// overwritten with no signal to anyone.
func classify(desired, existing any, exists, managed bool) disposition {
	switch {
	case !exists:
		return dispCreate
	case managed && jsonEqual(desired, existing):
		return dispNoop
	case managed:
		return dispUpdate
	case jsonEqual(desired, existing):
		return dispAdopt
	default:
		return dispAdoptConflict
	}
}

// managedIndex is the managed set as lookup maps, one per object kind.
type managedIndex struct {
	repos, cleanup, vuln, roles, webhooks map[string]bool
}

func (ms managedSet) index() managedIndex {
	return managedIndex{
		repos:    sliceSet(ms.Repositories),
		cleanup:  sliceSet(ms.CleanupPolicies),
		vuln:     sliceSet(ms.SecurityPolicies),
		roles:    sliceSet(ms.Roles),
		webhooks: sliceSet(ms.Webhooks),
	}
}

func sliceSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// diffFields reports the top-level JSON field names on which a and b differ,
// sorted. Used to tell an operator exactly what a forced adoption would
// overwrite rather than just naming the object.
func diffFields(a, b any) []string {
	ma, mb := toFieldMap(a), toFieldMap(b)
	seen := make(map[string]bool, len(ma)+len(mb))
	var out []string
	for k, va := range ma {
		seen[k] = true
		vb, ok := mb[k]
		if !ok || !jsonEqual(va, vb) {
			out = append(out, k)
		}
	}
	for k := range mb {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func toFieldMap(v any) map[string]json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// addConflict records an adoption conflict against the result.
func (r *Result) addConflict(kind, name string, fields []string) {
	r.Conflicts = append(r.Conflicts, Conflict{Kind: kind, Name: name, Fields: fields})
}

// auditAdoption records a forced adoption. Best-effort: a nil sink (auth
// disabled, or a unit test) is not an error.
func (a Appliers) auditAdoption(kind, name string, fields []string) {
	if a.Audit == nil {
		return
	}
	a.Audit.Append(obs.AuditEntry{
		Timestamp: time.Now().UTC(),
		Actor:     "config",
		Method:    "ADOPT",
		Path:      kind + "/" + name,
		Status:    200,
		Detail: fmt.Sprintf("config force-adopted unmanaged %s %q, overwriting: %s",
			kind, name, strings.Join(fields, ", ")),
	})
}

// Plan computes the diff between the desired File and current state.
// It reads from the managers but makes no writes.
//
// Objects that exist but were never managed by this file are classified as
// adoptions, not updates. An adoption whose fields already match is free; one
// that differs is recorded in Result.Conflicts and blocks Apply unless
// File.Adopt is set.
func Plan(f File, a Appliers) (Result, error) {
	if err := validate(f, a); err != nil {
		return Result{}, err
	}

	var res Result
	managed := loadManaged(a.Meta)
	mi := managed.index()

	// Cleanup policies.
	cleanupDesired := strSet(f.CleanupPolicies, func(p cleanup.NamedPolicy) string { return p.Name })
	for _, p := range f.CleanupPolicies {
		existing, ok, _ := a.Cleanup.Get(p.Name)
		res.tally(&res.CleanupPolicies, KindCleanupPolicy, p.Name,
			classify(p, existing, ok, mi.cleanup[p.Name]), p, existing)
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
		res.tally(&res.SecurityPolicies, KindSecurityPolicy, p.Name,
			classify(p, existing, ok, mi.vuln[p.Name]), p, existing)
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
	res.LDAPConfigured = f.LDAP != nil

	// Roles.
	if a.Roles != nil {
		rolesDesired := strSet(f.Roles, func(r auth.CustomRole) string { return r.Name })
		for _, r := range f.Roles {
			existing, ok, _ := a.Roles.Get(r.Name)
			res.tally(&res.Roles, KindRole, r.Name,
				classify(r, existing, ok, mi.roles[r.Name]), r, existing)
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
		res.tally(&res.Repositories, KindRepository, r.Name,
			classify(r, existing, ok, mi.repos[r.Name]), r, existing)
	}
	if f.Prune {
		for _, name := range managed.Repositories {
			if !reposDesired[name] {
				res.Repositories.Deleted++
			}
		}
	}

	// Webhooks (matched by Name).
	if a.Webhooks != nil {
		subs, _ := a.Webhooks.List()
		byName := make(map[string]webhook.Subscription, len(subs))
		for _, s := range subs {
			byName[s.Name] = s
		}
		webhooksDesired := strSet(f.Webhooks, func(s webhook.Subscription) string { return s.Name })
		for _, s := range f.Webhooks {
			ex, ok := byName[s.Name]
			// Compare the merged form so server-owned fields (ID, CreatedAt) and an
			// omitted secret never read as a difference.
			merged := mergeWebhook(s, ex)
			res.tally(&res.Webhooks, KindWebhook, s.Name,
				classify(merged, ex, ok, mi.webhooks[s.Name]), merged, ex)
		}
		if f.Prune {
			for _, name := range managed.Webhooks {
				if !webhooksDesired[name] {
					res.Webhooks.Deleted++
				}
			}
		}
	}

	return res, nil
}

// tally increments the right counter for a disposition and records a conflict
// when an adoption would overwrite unmanaged state.
func (r *Result) tally(k *KindResult, kind, name string, d disposition, desired, existing any) {
	switch d {
	case dispCreate:
		k.Created++
	case dispUpdate:
		k.Updated++
	case dispNoop:
		k.Noop++
	case dispAdopt:
		k.Adopted++
	case dispAdoptConflict:
		k.Adopted++
		r.addConflict(kind, name, diffFields(desired, existing))
	}
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
	if f.LDAP != nil {
		cp := *f.LDAP // Validate mutates (fills defaults); don't touch the caller's copy
		if err := cp.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("ldap: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config validation failed:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Apply reconciles the current state to match f. It is idempotent.
// Dependency order: roles → cleanup policies → security policies (+default) → repositories → webhooks.
//
// Apply refuses, before writing anything, if the file would adopt an object it
// has never managed whose fields differ from the desired state — set File.Adopt
// to allow that. Refusing up front (rather than failing partway) keeps a
// rejected apply from leaving state half-converged.
func Apply(f File, a Appliers) (Result, error) {
	// Plan is read-only and already validates; it also surfaces conflicts so we
	// can bail before the first write.
	pre, err := Plan(f, a)
	if err != nil {
		return Result{}, err
	}
	if len(pre.Conflicts) > 0 && !f.Adopt {
		var lines []string
		for _, c := range pre.Conflicts {
			lines = append(lines, c.String())
		}
		return Result{}, fmt.Errorf(
			"config: refusing to adopt %d object(s) this file has never managed and whose settings differ:\n  %s\n"+
				"set \"adopt\": true (or pass -config-adopt) to take ownership and overwrite them",
			len(pre.Conflicts), strings.Join(lines, "\n  "))
	}

	var res Result
	res.Conflicts = pre.Conflicts // forced adoptions, reported for the operator
	managed := loadManaged(a.Meta)
	snapshot := managed // copy before mutation (for prune)
	mi := managed.index()

	// 1. Roles (RoleStore has no Update — delete+recreate for changes).
	if a.Roles != nil {
		for _, r := range f.Roles {
			existing, ok, _ := a.Roles.Get(r.Name)
			d := classify(r, existing, ok, mi.roles[r.Name])
			switch d {
			case dispCreate:
				if err := a.Roles.Create(r); err != nil {
					return res, fmt.Errorf("config: create role %q: %w", r.Name, err)
				}
				res.Roles.Created++
			case dispUpdate, dispAdoptConflict:
				if err := a.Roles.Delete(r.Name); err != nil {
					return res, fmt.Errorf("config: update role %q (delete): %w", r.Name, err)
				}
				if err := a.Roles.Create(r); err != nil {
					return res, fmt.Errorf("config: update role %q (recreate): %w", r.Name, err)
				}
				if d == dispAdoptConflict {
					a.auditAdoption(KindRole, r.Name, diffFields(r, existing))
					res.Roles.Adopted++
				} else {
					res.Roles.Updated++
				}
			case dispAdopt:
				res.Roles.Adopted++ // identical already; only ownership changes
			default:
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
		d := classify(p, existing, ok, mi.cleanup[p.Name])
		switch d {
		case dispCreate, dispUpdate, dispAdoptConflict:
			if err := a.Cleanup.Put(p); err != nil {
				return res, fmt.Errorf("config: write cleanup policy %q: %w", p.Name, err)
			}
			switch d {
			case dispCreate:
				res.CleanupPolicies.Created++
			case dispUpdate:
				res.CleanupPolicies.Updated++
			default:
				a.auditAdoption(KindCleanupPolicy, p.Name, diffFields(p, existing))
				res.CleanupPolicies.Adopted++
			}
		case dispAdopt:
			res.CleanupPolicies.Adopted++
		default:
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
		d := classify(p, existing, ok, mi.vuln[p.Name])
		switch d {
		case dispCreate, dispUpdate, dispAdoptConflict:
			if err := a.Vuln.Put(p); err != nil {
				return res, fmt.Errorf("config: write security policy %q: %w", p.Name, err)
			}
			switch d {
			case dispCreate:
				res.SecurityPolicies.Created++
			case dispUpdate:
				res.SecurityPolicies.Updated++
			default:
				a.auditAdoption(KindSecurityPolicy, p.Name, diffFields(p, existing))
				res.SecurityPolicies.Adopted++
			}
		case dispAdopt:
			res.SecurityPolicies.Adopted++
		default:
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
	res.LDAPConfigured = f.LDAP != nil

	// 4. Repositories.
	for _, r := range f.Repositories {
		existing, ok := a.Repos.Get(r.Name)
		d := classify(r, existing, ok, mi.repos[r.Name])
		switch d {
		case dispCreate:
			if err := a.Repos.Add(r); err != nil {
				return res, fmt.Errorf("config: create repo %q: %w", r.Name, err)
			}
			res.Repositories.Created++
		case dispUpdate, dispAdoptConflict:
			if err := a.Repos.Update(r); err != nil {
				return res, fmt.Errorf("config: update repo %q: %w", r.Name, err)
			}
			if d == dispAdoptConflict {
				a.auditAdoption(KindRepository, r.Name, diffFields(r, existing))
				res.Repositories.Adopted++
			} else {
				res.Repositories.Updated++
			}
		case dispAdopt:
			res.Repositories.Adopted++
		default:
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
	if a.Webhooks == nil {
		if err := saveManaged(a.Meta, managed); err != nil {
			return res, fmt.Errorf("config: save managed set: %w", err)
		}
		return res, nil
	}
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
		merged := mergeWebhook(s, ex)
		d := classify(merged, ex, ok, mi.webhooks[s.Name])
		switch d {
		case dispCreate:
			if _, err := a.Webhooks.Create(s); err != nil {
				return res, fmt.Errorf("config: create webhook %q: %w", s.Name, err)
			}
			res.Webhooks.Created++
		case dispUpdate, dispAdoptConflict:
			if _, err := a.Webhooks.Update(merged); err != nil {
				return res, fmt.Errorf("config: update webhook %q: %w", s.Name, err)
			}
			if d == dispAdoptConflict {
				a.auditAdoption(KindWebhook, s.Name, diffFields(merged, ex))
				res.Webhooks.Adopted++
			} else {
				res.Webhooks.Updated++
			}
		case dispAdopt:
			res.Webhooks.Adopted++
		default:
			res.Webhooks.Noop++
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

	if a.Webhooks != nil {
		subs, err := a.Webhooks.List()
		if err != nil {
			return File{}, fmt.Errorf("config export: webhooks: %w", err)
		}
		for i := range subs {
			subs[i].Secret = "" // blanked; re-supply via ${WEBHOOK_SECRET}
		}
		f.Webhooks = subs
	}

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
