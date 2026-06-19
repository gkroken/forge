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
	"fmt"
	"strings"
	"time"

	"forge/internal/meta"
)

// Role is the permission level granted within a repository.
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

// Action is the class of HTTP operation.
type Action int

const (
	ActionRead  Action = iota // GET, HEAD
	ActionWrite               // PUT, POST, DELETE, PATCH
)

// Grant gives a Role on a specific repository.
// Repo == "*" matches any repository.
type Grant struct {
	Repo string `json:"repo"`
	Role Role   `json:"role"`
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

// RoleFor returns the highest Role this token grants on repo.
// A grant with Repo=="*" acts as a wildcard.
func (t *Token) RoleFor(repo string) Role {
	best := RoleNone
	for _, g := range t.Grants {
		if g.Repo == repo || g.Repo == "*" {
			if g.Role > best {
				best = g.Role
			}
		}
	}
	return best
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
