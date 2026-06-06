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
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"forge/internal/auth"
)

// Config holds the OIDC client configuration, loaded from environment variables.
type Config struct {
	Issuer        string       // OIDC_ISSUER  e.g. https://accounts.google.com
	ClientID      string       // OIDC_CLIENT_ID
	ClientSecret  string       // OIDC_CLIENT_SECRET
	RedirectURL   string       // OIDC_REDIRECT_URL  e.g. https://forge.example.com/auth/oidc/callback
	DefaultGrants []auth.Grant // OIDC_DEFAULT_GRANTS  JSON, default: read on *
	TokenTTL      time.Duration // OIDC_TOKEN_TTL  default 8h
}

// FromEnv reads OIDC configuration from environment variables.
// Returns nil if OIDC_ISSUER is not set (OIDC disabled).
func FromEnv() (*Config, error) {
	issuer := os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		return nil, nil
	}
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if clientID == "" {
		return nil, errors.New("OIDC_CLIENT_ID must be set when OIDC_ISSUER is set")
	}
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	if clientSecret == "" {
		return nil, errors.New("OIDC_CLIENT_SECRET must be set when OIDC_ISSUER is set")
	}
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")
	if redirectURL == "" {
		return nil, errors.New("OIDC_REDIRECT_URL must be set when OIDC_ISSUER is set")
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

	return &Config{
		Issuer:        issuer,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		RedirectURL:   redirectURL,
		DefaultGrants: grants,
		TokenTTL:      ttl,
	}, nil
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
	Subject string // stable user identifier from "sub" claim
	Email   string // from "email" claim (may be empty if IdP omits it)
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

	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return UserInfo{}, fmt.Errorf("oidc: claims: %w", err)
	}

	return UserInfo{Subject: idToken.Subject, Email: claims.Email}, nil
}

// DefaultGrants returns the grants that should be assigned to a new
// forge token issued via OIDC login.
func (p *Provider) DefaultGrants() []auth.Grant { return p.cfg.DefaultGrants }

// TokenTTL returns how long a forge token issued via OIDC should live.
func (p *Provider) TokenTTL() time.Duration { return p.cfg.TokenTTL }
