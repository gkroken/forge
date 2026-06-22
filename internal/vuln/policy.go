package vuln

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"forge/internal/meta"
)

// Enforcement policy for vulnerable-artifact downloads. The model is
// source-agnostic like the rest of the spine: it reads persisted Findings and
// decides serve / warn / block, knowing nothing about OSV or any producer.
//
// Storage mirrors cleanup's named-policy precedent:
//   - named, reusable policies live in meta ns "vuln-policies" (one doc each),
//   - a single global-default policy lives in ns "vuln-policy" key "default".
//
// A repository references a named policy by Repository.SecurityPolicyName;
// Resolve falls back named → global default → Off.
const (
	policyNS        = "vuln-policies"
	globalPolicyNS  = "vuln-policy"
	globalPolicyKey = "default"
)

var validPolicyName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Mode is the enforcement mode. Off disables the gate; Warn serves but signals
// (header + audit + metric + webhook); Block returns 403.
type Mode string

const (
	ModeOff   Mode = "off"
	ModeWarn  Mode = "warn"
	ModeBlock Mode = "block"
)

// Action is the outcome of evaluating one download against a Policy.
type Action int

const (
	ActionServe Action = iota // serve normally
	ActionWarn                // serve, but emit the warning signal
	ActionBlock               // refuse with 403
)

func (a Action) String() string {
	switch a {
	case ActionWarn:
		return "warn"
	case ActionBlock:
		return "block"
	default:
		return "serve"
	}
}

// Suppression silences a specific advisory (matched against Advisory.ID and its
// Aliases, case-insensitively) so it never counts toward the worst severity.
// Reason/By/At are recorded for the audit trail — a suppression is a governance
// decision, not a silent toggle.
type Suppression struct {
	ID     string    `json:"id"` // CVE/GHSA/OSV id
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

// Policy is the enforcement configuration evaluated at download time.
type Policy struct {
	Mode      Mode     `json:"mode"`
	Threshold Severity `json:"threshold"` // minimum severity to act on (e.g. high)
	// FailOpen serves components that have no Finding yet (never scanned). True
	// is the safe default — fail-closed would 403 every unscanned artifact.
	FailOpen     bool          `json:"failOpen"`
	Suppressions []Suppression `json:"suppressions,omitempty"`
}

// Decision evaluates a download of one component@version. found reports whether
// a Finding exists (i.e. the component has been scanned). The returned Severity
// is the worst non-suppressed severity considered (SeverityUnknown when serving
// for a reason other than severity).
//
// Rules:
//   - Mode Off (or empty) → always serve.
//   - No Finding (unscanned) → serve if FailOpen, else act (block/warn).
//   - A scanned component acts only when its worst non-suppressed severity is
//     known (> unknown) AND at or above Threshold. A clean finding, or one whose
//     only advisories are suppressed or unscored, always serves — the gate never
//     blocks on "unknown".
func (p Policy) Decision(f Finding, found bool) (Action, Severity) {
	if p.Mode == ModeOff || p.Mode == "" {
		return ActionServe, SeverityUnknown
	}
	if !found {
		if p.FailOpen {
			return ActionServe, SeverityUnknown
		}
		return p.act(), SeverityUnknown
	}
	worst := p.worstUnsuppressed(f)
	if worst == SeverityUnknown || worst < p.Threshold {
		return ActionServe, worst
	}
	return p.act(), worst
}

func (p Policy) act() Action {
	if p.Mode == ModeBlock {
		return ActionBlock
	}
	return ActionWarn
}

// worstUnsuppressed returns the highest severity among advisories not silenced
// by a suppression.
func (p Policy) worstUnsuppressed(f Finding) Severity {
	worst := SeverityUnknown
	for _, a := range f.Advisories {
		if p.suppressed(a) {
			continue
		}
		if a.Severity > worst {
			worst = a.Severity
		}
	}
	return worst
}

func (p Policy) suppressed(a Advisory) bool {
	for _, s := range p.Suppressions {
		if s.ID == "" {
			continue
		}
		if strings.EqualFold(s.ID, a.ID) {
			return true
		}
		for _, alias := range a.Aliases {
			if strings.EqualFold(s.ID, alias) {
				return true
			}
		}
	}
	return false
}

// NamedPolicy is a named, reusable Policy (the "strict"/"lenient" precedent from
// cleanup) persisted in meta.Store.
type NamedPolicy struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Policy
}

// PolicyManager persists named security policies and the global default to
// meta.Store. It mirrors cleanup.PolicyManager.
type PolicyManager struct {
	meta meta.Store
}

func NewPolicyManager(m meta.Store) *PolicyManager { return &PolicyManager{meta: m} }

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
	out := make([]NamedPolicy, 0, len(keys))
	for _, k := range keys {
		var p NamedPolicy
		if ok, _ := pm.meta.GetJSON(policyNS, k, &p); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// Put creates or replaces a named policy. The name must match
// [a-z0-9][a-z0-9-]{0,62}.
func (pm *PolicyManager) Put(p NamedPolicy) error {
	if !validPolicyName.MatchString(p.Name) {
		return fmt.Errorf("vuln-policies: name %q must match [a-z0-9][a-z0-9-]{0,62}", p.Name)
	}
	return pm.meta.PutJSON(policyNS, p.Name, p)
}

func (pm *PolicyManager) Delete(name string) error {
	return pm.meta.Delete(policyNS, name)
}

// Default returns the global-default policy, or an Off policy when none is set.
func (pm *PolicyManager) Default() (Policy, error) {
	var p Policy
	ok, err := pm.meta.GetJSON(globalPolicyNS, globalPolicyKey, &p)
	if err != nil {
		return Policy{}, err
	}
	if !ok {
		return Policy{Mode: ModeOff}, nil
	}
	return p, nil
}

// SetDefault stores the global-default policy.
func (pm *PolicyManager) SetDefault(p Policy) error {
	return pm.meta.PutJSON(globalPolicyNS, globalPolicyKey, p)
}

// Resolve returns the effective policy for a repository: its named policy when
// policyName is set and exists, otherwise the global default (Off when unset).
func (pm *PolicyManager) Resolve(policyName string) (Policy, error) {
	if policyName != "" {
		np, ok, err := pm.Get(policyName)
		if err != nil {
			return Policy{}, err
		}
		if ok {
			return np.Policy, nil
		}
	}
	return pm.Default()
}
