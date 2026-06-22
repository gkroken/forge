// Package oidc provides OIDC-based authentication for forge.
// It wraps coreos/go-oidc/v3 and golang.org/x/oauth2 behind a small surface
// that the server package depends on.  All IdP interaction (provider discovery,
// code exchange, ID-token verification) is contained here.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"forge/internal/auth"
)

// Config holds the OIDC client configuration. It can be assembled from
// environment variables (FromEnv) or from command-line flags (see cmd/forge).
type Config struct {
	Issuer        string           // OIDC_ISSUER  e.g. https://accounts.google.com
	ClientID      string           // OIDC_CLIENT_ID
	ClientSecret  string           // OIDC_CLIENT_SECRET
	RedirectURL   string           // OIDC_REDIRECT_URL  e.g. https://forge.example.com/auth/oidc/callback
	GroupsClaim   string           // OIDC_GROUPS_CLAIM  ID-token claim holding group membership, default "groups"
	GroupMappings []auth.GroupRule // OIDC_GROUP_MAPPINGS  "group:role,..." mapping IdP groups onto base roles
	DefaultGrants []auth.Grant     // OIDC_DEFAULT_GRANTS  JSON; fallback when no group matches, default read on *
	TokenTTL      time.Duration    // OIDC_TOKEN_TTL  default 8h
}

// defaultGroupsClaim is the ID-token claim forge reads group membership from
// when OIDC_GROUPS_CLAIM is unset. Keycloak and Entra/Okta emit "groups".
const defaultGroupsClaim = "groups"

// Validate checks that the required fields for an enabled OIDC config are set.
func (c Config) Validate() error {
	switch {
	case c.Issuer == "":
		return errors.New("OIDC issuer must be set")
	case c.ClientID == "":
		return errors.New("OIDC client ID must be set")
	case c.ClientSecret == "":
		return errors.New("OIDC client secret must be set")
	case c.RedirectURL == "":
		return errors.New("OIDC redirect URL must be set")
	}
	return nil
}

// ParseGroupMappings parses a "group:role,group:role" string into GroupRules.
// Role is one of read|write|admin (reader|publisher|administrator also accepted).
// The group name is everything before the final colon, so it may itself contain
// colons. An empty string yields no rules.
func ParseGroupMappings(s string) ([]auth.GroupRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var rules []auth.GroupRule
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.LastIndex(pair, ":")
		if i < 0 {
			return nil, fmt.Errorf("group mapping %q: expected group:role", pair)
		}
		group := strings.TrimSpace(pair[:i])
		roleName := strings.TrimSpace(pair[i+1:])
		if group == "" {
			return nil, fmt.Errorf("group mapping %q: empty group name", pair)
		}
		role := auth.BaseRoleFor(roleName)
		if role == auth.RoleNone {
			return nil, fmt.Errorf("group mapping %q: unknown role %q (want read|write|admin)", pair, roleName)
		}
		rules = append(rules, auth.GroupRule{Group: group, Role: role})
	}
	return rules, nil
}

// FromEnv reads OIDC configuration from environment variables.
// Returns nil if OIDC_ISSUER is not set (OIDC disabled).
func FromEnv() (*Config, error) {
	issuer := os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		return nil, nil
	}
	groupsClaim := os.Getenv("OIDC_GROUPS_CLAIM")
	if groupsClaim == "" {
		groupsClaim = defaultGroupsClaim
	}

	mappings, err := ParseGroupMappings(os.Getenv("OIDC_GROUP_MAPPINGS"))
	if err != nil {
		return nil, fmt.Errorf("OIDC_GROUP_MAPPINGS: %w", err)
	}

	grants := []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
	if raw := os.Getenv("OIDC_DEFAULT_GRANTS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &grants); err != nil {
			return nil, fmt.Errorf("OIDC_DEFAULT_GRANTS: %w", err)
		}
	}

	ttl := 8 * time.Hour
	if raw := os.Getenv("OIDC_TOKEN_TTL"); raw != "" {
		var err error
		ttl, err = time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("OIDC_TOKEN_TTL: %w", err)
		}
	}

	cfg := &Config{
		Issuer:        issuer,
		ClientID:      os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:   os.Getenv("OIDC_REDIRECT_URL"),
		GroupsClaim:   groupsClaim,
		GroupMappings: mappings,
		DefaultGrants: grants,
		TokenTTL:      ttl,
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w (when OIDC_ISSUER is set)", err)
	}
	return cfg, nil
}

// Provider is a ready-to-use OIDC provider. Construct one via New.
type Provider struct {
	cfg      Config
	provider *gooidc.Provider
	oauth2   oauth2.Config
}

// New discovers the OIDC provider at cfg.Issuer and returns a Provider.
// ctx is used only for the discovery HTTP call; it need not outlive this call.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = defaultGroupsClaim
	}
	p, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	return &Provider{
		cfg:      cfg,
		provider: p,
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       []string{gooidc.ScopeOpenID, "email", "profile"},
		},
	}, nil
}

// AuthURL returns the IdP authorization URL for the login redirect.
// state is a random CSRF token; nonce is bound into the ID token.
func (p *Provider) AuthURL(state, nonce string) string {
	return p.oauth2.AuthCodeURL(state, gooidc.Nonce(nonce))
}

// UserInfo holds the claims extracted from a successful OIDC exchange.
type UserInfo struct {
	Subject string   // stable user identifier from "sub" claim
	Email   string   // from "email" claim (may be empty if IdP omits it)
	Groups  []string // from the configured groups claim (may be empty)
}

// Exchange trades an authorization code for a verified ID token.
// nonce must match the value embedded in the token by the IdP.
func (p *Provider) Exchange(ctx context.Context, code, nonce string) (UserInfo, error) {
	tok, err := p.oauth2.Exchange(ctx, code)
	if err != nil {
		return UserInfo{}, fmt.Errorf("oidc: code exchange: %w", err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return UserInfo{}, errors.New("oidc: id_token missing from token response")
	}

	verifier := p.provider.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID})
	idToken, err := verifier.Verify(ctx, rawID)
	if err != nil {
		return UserInfo{}, fmt.Errorf("oidc: id_token verify: %w", err)
	}
	if idToken.Nonce != nonce {
		return UserInfo{}, errors.New("oidc: nonce mismatch")
	}

	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return UserInfo{}, fmt.Errorf("oidc: claims: %w", err)
	}

	var email string
	if raw, ok := claims["email"]; ok {
		_ = json.Unmarshal(raw, &email) // best-effort; absent/odd email is non-fatal
	}

	return UserInfo{
		Subject: idToken.Subject,
		Email:   email,
		Groups:  extractGroups(claims[p.cfg.GroupsClaim]),
	}, nil
}

// extractGroups decodes the groups claim, accepting either a JSON array of
// strings (Keycloak, Entra, Okta) or a single string. Anything else yields nil.
func extractGroups(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

// DefaultGrants returns the grants that should be assigned to a new
// forge token issued via OIDC login.
func (p *Provider) DefaultGrants() []auth.Grant { return p.cfg.DefaultGrants }

// TokenTTL returns how long a forge token issued via OIDC should live.
func (p *Provider) TokenTTL() time.Duration { return p.cfg.TokenTTL }

// The following accessors expose non-secret config for read-only display in the
// admin UI. The client secret is deliberately never exposed.

func (p *Provider) Issuer() string      { return p.cfg.Issuer }
func (p *Provider) ClientID() string    { return p.cfg.ClientID }
func (p *Provider) RedirectURL() string { return p.cfg.RedirectURL }
func (p *Provider) GroupsClaim() string { return p.cfg.GroupsClaim }
