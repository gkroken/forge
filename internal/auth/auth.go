// Package auth provides token-based authentication and per-repository
// role-based access control for forge.
//
// Every request passes through an Enforcer that makes a policy decision
// before the format handler is invoked. In eval mode (no Store configured)
// the AllowAll policy is used — still a decision on every route.
//
// Token format: "forge_" + 64 hex chars (32 random bytes).
// Tokens are stored by their SHA-256 hash; the raw secret is shown only once.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"forge/internal/meta"
	"forge/internal/selector"
)

// Role is an identity tier: it describes what kind of principal a user or
// IdP group maps to (Reader / Publisher / Administrator). Grants no longer
// carry a Role — they carry explicit Actions — but the tier survives as the
// vocabulary of the user store, OIDC/LDAP group mapping, and custom roles.
// GrantForRole expands a tier into its action bundle.
type Role int

const (
	RoleNone  Role = 0
	RoleRead  Role = 1
	RoleWrite Role = 2
	RoleAdmin Role = 3
)

func (r Role) String() string {
	switch r {
	case RoleRead:
		return "read"
	case RoleWrite:
		return "write"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParseRole converts "read" | "write" | "admin" to a Role.
func ParseRole(s string) (Role, error) {
	switch s {
	case "read":
		return RoleRead, nil
	case "write":
		return RoleWrite, nil
	case "admin":
		return RoleAdmin, nil
	default:
		return RoleNone, fmt.Errorf("unknown role %q", s)
	}
}

// Action is a permission verb that a Grant can carry.
type Action string

const (
	ActionRead   Action = "read"   // GET/HEAD: download, resolve, browse
	ActionWrite  Action = "write"  // PUT/POST: publish content
	ActionDelete Action = "delete" // DELETE: remove content
	ActionAdmin  Action = "admin"  // manage the repository: settings, cleanup, cache, policies
)

// AllActions lists every valid Action in display order.
var AllActions = []Action{ActionRead, ActionWrite, ActionDelete, ActionAdmin}

// ParseAction converts a string to an Action.
func ParseAction(s string) (Action, error) {
	a := Action(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(AllActions, a) {
		return a, nil
	}
	return "", fmt.Errorf("unknown action %q (want read|write|delete|admin)", s)
}

// Grant gives a set of Actions on a repository. Repo == "*" matches any
// repository. An empty Selectors list covers the whole repository; otherwise
// the content actions (read/write/delete) apply only to request paths that
// match at least one selector pattern (grammar in internal/selector).
// ActionAdmin cannot be selector-scoped: repository administration is not a
// path-level operation (ValidateGrants rejects the combination).
type Grant struct {
	Repo      string   `json:"repo"`
	Actions   []Action `json:"actions"`
	Selectors []string `json:"selectors,omitempty"`
}

// UnmarshalJSON accepts both the current shape and the legacy pre-actions
// shape {"repo":"x","role":2} (or "role":"write" — the documented API form),
// expanding the role tier into its action bundle. Tokens persisted before
// the schema change keep working; they are rewritten in the new shape on
// their next use (Verify updates LastUsed).
func (g *Grant) UnmarshalJSON(b []byte) error {
	var aux struct {
		Repo      string          `json:"repo"`
		Actions   []Action        `json:"actions"`
		Selectors []string        `json:"selectors"`
		Role      json.RawMessage `json:"role"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	g.Repo, g.Actions, g.Selectors = aux.Repo, aux.Actions, aux.Selectors
	if len(g.Actions) == 0 && len(aux.Role) > 0 {
		var n int
		var s string
		switch {
		case json.Unmarshal(aux.Role, &n) == nil:
			g.Actions = actionsForRole(Role(n))
		case json.Unmarshal(aux.Role, &s) == nil:
			r, err := ParseRole(s)
			if err != nil {
				return fmt.Errorf("grant on %q: %w", aux.Repo, err)
			}
			g.Actions = actionsForRole(r)
		default:
			return fmt.Errorf("grant on %q: role must be a number or role name", aux.Repo)
		}
	}
	return nil
}

// allows reports whether this grant permits action a on path within repoName.
func (g Grant) allows(repoName, path string, a Action) bool {
	if g.Repo != repoName && g.Repo != "*" {
		return false
	}
	if !slices.Contains(g.Actions, a) {
		return false
	}
	if a == ActionAdmin || len(g.Selectors) == 0 {
		return true
	}
	return selector.MatchAny(g.Selectors, path)
}

// Tier returns the highest identity tier this grant implies: admin ⊃ write ⊃
// read. It is the lossy inverse of GrantForRole, used where a grant must be
// summarised as a role name (LDAP config rendering, UI badges).
func (g Grant) Tier() Role {
	switch {
	case slices.Contains(g.Actions, ActionAdmin):
		return RoleAdmin
	case slices.Contains(g.Actions, ActionWrite):
		return RoleWrite
	case slices.Contains(g.Actions, ActionRead):
		return RoleRead
	}
	return RoleNone
}

// actionsForRole expands an identity tier into its action bundle. The write
// tier includes delete because the pre-actions RoleWrite covered the DELETE
// method; behaviour of existing write tokens and Publisher users must not
// silently narrow.
func actionsForRole(r Role) []Action {
	switch {
	case r >= RoleAdmin:
		return []Action{ActionRead, ActionWrite, ActionDelete, ActionAdmin}
	case r >= RoleWrite:
		return []Action{ActionRead, ActionWrite, ActionDelete}
	case r >= RoleRead:
		return []Action{ActionRead}
	}
	return nil
}

// GrantForRole expands an identity-tier Role into a whole-repo Grant.
func GrantForRole(repoName string, r Role) Grant {
	return Grant{Repo: repoName, Actions: actionsForRole(r)}
}

// ValidateGrants checks that a grant list is well-formed for a new token.
func ValidateGrants(grants []Grant) error {
	if len(grants) == 0 {
		return fmt.Errorf("at least one grant is required")
	}
	for i, g := range grants {
		if g.Repo == "" {
			return fmt.Errorf("grant %d: repository is required", i+1)
		}
		if len(g.Actions) == 0 {
			return fmt.Errorf("grant %d (%s): at least one action is required", i+1, g.Repo)
		}
		for _, a := range g.Actions {
			if !slices.Contains(AllActions, a) {
				return fmt.Errorf("grant %d (%s): unknown action %q", i+1, g.Repo, a)
			}
		}
		if len(g.Selectors) > 0 && slices.Contains(g.Actions, ActionAdmin) {
			return fmt.Errorf("grant %d (%s): admin cannot be selector-scoped", i+1, g.Repo)
		}
		for _, sel := range g.Selectors {
			if err := selector.Validate(sel); err != nil {
				return fmt.Errorf("grant %d (%s): %w", i+1, g.Repo, err)
			}
		}
	}
	return nil
}

// Token is a long-lived API credential. The raw secret is never stored;
// only its SHA-256 hash is persisted.
type Token struct {
	ID          string     `json:"id"`
	Description string     `json:"description"`
	Grants      []Grant    `json:"grants"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
}

// Allows reports whether this token permits action a on path within repo.
// A grant with Repo=="*" acts as a wildcard. path is the repo-relative
// request path; it is only consulted for selector-scoped grants (pass ""
// for repo-level checks such as admin).
func (t *Token) Allows(repo, path string, a Action) bool {
	for _, g := range t.Grants {
		if g.allows(repo, path, a) {
			return true
		}
	}
	return false
}

// GlobalAdmin reports whether this token carries admin on every repository
// (an admin grant on the "*" wildcard). System-level surfaces — user, token,
// webhook, and global-policy management — require this, not just repo admin.
func (t *Token) GlobalAdmin() bool {
	for _, g := range t.Grants {
		if g.Repo == "*" && slices.Contains(g.Actions, ActionAdmin) {
			return true
		}
	}
	return false
}

// Store manages token lifecycle.
type Store interface {
	// Create mints a new token. Returns the Token metadata and the raw
	// display secret (shown once; caller must convey it to the user).
	// The optional owner argument (first element only) sets Token.Owner.
	Create(desc string, grants []Grant, expiresAt *time.Time, owner ...string) (Token, string, error)
	// Verify checks a raw display secret and returns the corresponding
	// Token, or nil if unknown, expired, or malformed.
	Verify(secret string) (*Token, error)
	// Revoke permanently invalidates the token with the given ID.
	Revoke(id string) error
	// List returns all token metadata (no secrets).
	List() ([]Token, error)
	// Count returns the number of live tokens.
	Count() (int, error)
}

// NewMetaStore returns a Store backed by m.
func NewMetaStore(m meta.Store) Store { return &metaStore{meta: m} }

// --- internal helpers --------------------------------------------------------

const tokenPrefix = "forge_"

const (
	nsTokenByHash = "auth:tokens"    // hash → storedToken
	nsTokenByID   = "auth:token-idx" // id   → hash
)

type storedToken struct {
	Token
	SecretHash string `json:"secret_hash"`
}

// generate returns 32 cryptographically random bytes and the display string.
func generate() (raw [32]byte, display string) {
	if _, err := rand.Read(raw[:]); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	display = tokenPrefix + hex.EncodeToString(raw[:])
	return
}

// generateID returns a 16-char random hex string used as a token ID.
func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// hashRaw returns the hex-encoded SHA-256 of raw bytes.
func hashRaw(raw [32]byte) string {
	h := sha256.Sum256(raw[:])
	return hex.EncodeToString(h[:])
}

// hashDisplay parses a display secret and returns its hex-encoded SHA-256.
// Returns "" if the secret is malformed.
func hashDisplay(secret string) string {
	hexPart := strings.TrimPrefix(secret, tokenPrefix)
	raw, err := hex.DecodeString(hexPart)
	if err != nil || len(raw) != 32 {
		return ""
	}
	var b [32]byte
	copy(b[:], raw)
	return hashRaw(b)
}
