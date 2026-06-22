package oidc_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"forge/internal/auth"
	"forge/internal/oidc"
)

// ── FromEnv ───────────────────────────────────────────────────────────────────

func TestFromEnv_NilWhenIssuerUnset(t *testing.T) {
	resetOIDCEnv(t)
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when OIDC_ISSUER not set")
	}
}

func TestFromEnv_RequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			"missing CLIENT_ID",
			map[string]string{"OIDC_ISSUER": "https://idp.example.com"},
			"client ID",
		},
		{
			"missing CLIENT_SECRET",
			map[string]string{
				"OIDC_ISSUER":    "https://idp.example.com",
				"OIDC_CLIENT_ID": "cid",
			},
			"client secret",
		},
		{
			"missing REDIRECT_URL",
			map[string]string{
				"OIDC_ISSUER":        "https://idp.example.com",
				"OIDC_CLIENT_ID":     "cid",
				"OIDC_CLIENT_SECRET": "sec",
			},
			"redirect URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetOIDCEnv(t)
			for k, v := range tc.env {
				os.Setenv(k, v)
			}
			_, err := oidc.FromEnv()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !containsStr(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestFromEnv_BadGrantsJSON(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_DEFAULT_GRANTS", "not-json")
	_, err := oidc.FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid JSON grants")
	}
	if !containsStr(err.Error(), "OIDC_DEFAULT_GRANTS") {
		t.Fatalf("expected OIDC_DEFAULT_GRANTS in error, got: %v", err)
	}
}

func TestFromEnv_BadTokenTTL(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_TOKEN_TTL", "not-a-duration")
	_, err := oidc.FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid token TTL")
	}
	if !containsStr(err.Error(), "OIDC_TOKEN_TTL") {
		t.Fatalf("expected OIDC_TOKEN_TTL in error, got: %v", err)
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if len(cfg.DefaultGrants) != 1 || cfg.DefaultGrants[0].Role != auth.RoleRead || cfg.DefaultGrants[0].Repo != "*" {
		t.Fatalf("default grants: got %+v", cfg.DefaultGrants)
	}
	if cfg.TokenTTL != 8*time.Hour {
		t.Fatalf("default TTL: got %v", cfg.TokenTTL)
	}
}

func TestFromEnv_CustomGrants(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_DEFAULT_GRANTS", `[{"repo":"npm-hosted","role":2}]`)
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DefaultGrants) != 1 ||
		cfg.DefaultGrants[0].Repo != "npm-hosted" ||
		cfg.DefaultGrants[0].Role != auth.RoleWrite {
		t.Fatalf("custom grants: got %+v", cfg.DefaultGrants)
	}
}

func TestFromEnv_CustomTTL(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_TOKEN_TTL", "24h")
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenTTL != 24*time.Hour {
		t.Fatalf("custom TTL: got %v", cfg.TokenTTL)
	}
}

// ── Provider / Exchange (fake OIDC server) ────────────────────────────────────

func TestNew_Discovery(t *testing.T) {
	fake := newFakeOIDC(t)
	_, err := oidc.New(context.Background(), fake.config("test-client"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNew_UnreachableIssuer(t *testing.T) {
	cfg := oidc.Config{
		Issuer:       "http://127.0.0.1:1", // nothing listening
		ClientID:     "cid",
		ClientSecret: "sec",
		RedirectURL:  "http://forge/cb",
	}
	_, err := oidc.New(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unreachable issuer")
	}
}

func TestExchange_HappyPath(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.subject = "user-123"
	fake.email = "user@example.com"
	fake.groups = []string{"developers", "forge-admins"}
	fake.nonce = "abc-nonce"

	p, err := oidc.New(context.Background(), fake.config("test-client"))
	if err != nil {
		t.Fatal(err)
	}

	info, err := p.Exchange(context.Background(), "any-code", "abc-nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if info.Subject != "user-123" {
		t.Errorf("subject: got %q, want %q", info.Subject, "user-123")
	}
	if info.Email != "user@example.com" {
		t.Errorf("email: got %q, want %q", info.Email, "user@example.com")
	}
	if len(info.Groups) != 2 || info.Groups[0] != "developers" || info.Groups[1] != "forge-admins" {
		t.Errorf("groups: got %v, want [developers forge-admins]", info.Groups)
	}
}

func TestExchange_GroupsClaimAbsent(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.nonce = "n1"
	// groups left nil → claim omitted from the token

	p, _ := oidc.New(context.Background(), fake.config("test-client"))
	info, err := p.Exchange(context.Background(), "code", "n1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(info.Groups) != 0 {
		t.Errorf("groups should be empty, got %v", info.Groups)
	}
}

func TestParseGroupMappings(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		rules, err := oidc.ParseGroupMappings("forge-admins:admin, devs:write ,staff:read")
		if err != nil {
			t.Fatal(err)
		}
		want := []auth.GroupRule{
			{Group: "forge-admins", Role: auth.RoleAdmin},
			{Group: "devs", Role: auth.RoleWrite},
			{Group: "staff", Role: auth.RoleRead},
		}
		if len(rules) != len(want) {
			t.Fatalf("got %d rules, want %d: %+v", len(rules), len(want), rules)
		}
		for i := range want {
			if rules[i] != want[i] {
				t.Errorf("rule %d: got %+v, want %+v", i, rules[i], want[i])
			}
		}
	})
	t.Run("empty", func(t *testing.T) {
		rules, err := oidc.ParseGroupMappings("   ")
		if err != nil || rules != nil {
			t.Fatalf("empty: got (%v, %v)", rules, err)
		}
	})
	t.Run("missing colon", func(t *testing.T) {
		if _, err := oidc.ParseGroupMappings("justagroup"); err == nil {
			t.Fatal("expected error for missing colon")
		}
	})
	t.Run("unknown role", func(t *testing.T) {
		if _, err := oidc.ParseGroupMappings("grp:superuser"); err == nil {
			t.Fatal("expected error for unknown role")
		}
	})
}

func TestFromEnv_GroupMappingsAndClaim(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_GROUPS_CLAIM", "roles")
	os.Setenv("OIDC_GROUP_MAPPINGS", "forge-admins:admin")
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GroupsClaim != "roles" {
		t.Errorf("groups claim: got %q", cfg.GroupsClaim)
	}
	if len(cfg.GroupMappings) != 1 || cfg.GroupMappings[0].Role != auth.RoleAdmin {
		t.Errorf("group mappings: got %+v", cfg.GroupMappings)
	}
}

func TestFromEnv_DefaultGroupsClaim(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	cfg, err := oidc.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GroupsClaim != "groups" {
		t.Errorf("default groups claim: got %q, want \"groups\"", cfg.GroupsClaim)
	}
}

func TestFromEnv_BadGroupMappings(t *testing.T) {
	resetOIDCEnv(t)
	setRequiredOIDCEnv(t)
	os.Setenv("OIDC_GROUP_MAPPINGS", "broken")
	if _, err := oidc.FromEnv(); err == nil || !containsStr(err.Error(), "OIDC_GROUP_MAPPINGS") {
		t.Fatalf("expected OIDC_GROUP_MAPPINGS error, got: %v", err)
	}
}

func TestExchange_NoEmail(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.subject = "opaque-sub"
	fake.email = ""
	fake.nonce = "n1"

	p, _ := oidc.New(context.Background(), fake.config("test-client"))
	info, err := p.Exchange(context.Background(), "code", "n1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if info.Subject != "opaque-sub" {
		t.Errorf("subject: got %q", info.Subject)
	}
	if info.Email != "" {
		t.Errorf("email should be empty, got %q", info.Email)
	}
}

func TestExchange_NonceMismatch(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.nonce = "token-embeds-this"

	p, _ := oidc.New(context.Background(), fake.config("test-client"))
	_, err := p.Exchange(context.Background(), "code", "caller-expects-this")
	if err == nil {
		t.Fatal("expected nonce mismatch error")
	}
}

func TestExchange_MissingIDToken(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.omitIDToken = true

	p, _ := oidc.New(context.Background(), fake.config("test-client"))
	_, err := p.Exchange(context.Background(), "code", "nonce")
	if err == nil {
		t.Fatal("expected error when id_token missing from response")
	}
}

func TestExchange_InvalidJWT(t *testing.T) {
	fake := newFakeOIDC(t)
	fake.badToken = true

	p, _ := oidc.New(context.Background(), fake.config("test-client"))
	_, err := p.Exchange(context.Background(), "code", "nonce")
	if err == nil {
		t.Fatal("expected error for malformed JWT")
	}
}

// ── fake OIDC server ──────────────────────────────────────────────────────────

type fakeOIDC struct {
	srv         *httptest.Server
	key         *rsa.PrivateKey
	subject     string
	email       string
	groups      []string // emitted under the "groups" claim when non-nil
	nonce       string   // embedded in every id_token
	omitIDToken bool
	badToken    bool
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{key: key, subject: "default-sub", nonce: "default-nonce"}

	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", f.serveDiscovery)
	mux.HandleFunc("/jwks", f.serveJWKS)
	mux.HandleFunc("/token", f.serveToken)
	return f
}

func (f *fakeOIDC) config(clientID string) oidc.Config {
	return oidc.Config{
		Issuer:        f.srv.URL,
		ClientID:      clientID,
		ClientSecret:  "test-secret",
		RedirectURL:   "http://forge.example.com/auth/oidc/callback",
		DefaultGrants: []auth.Grant{{Repo: "*", Role: auth.RoleRead}},
		TokenTTL:      8 * time.Hour,
	}
}

func (f *fakeOIDC) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                f.srv.URL,
		"authorization_endpoint":                f.srv.URL + "/auth",
		"token_endpoint":                        f.srv.URL + "/token",
		"jwks_uri":                              f.srv.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *fakeOIDC) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"keys": []any{rsaToJWK(&f.key.PublicKey)},
	})
}

func (f *fakeOIDC) serveToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if f.omitIDToken {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer"})
		return
	}
	idToken := "not.a.jwt"
	if !f.badToken {
		// Extract clientID from Basic auth (oauth2 sends it that way).
		clientID, _, _ := r.BasicAuth()
		if clientID == "" {
			r.ParseForm()
			clientID = r.FormValue("client_id")
		}
		idToken = makeIDToken(f.key, f.srv.URL, clientID, f.subject, f.email, f.nonce, f.groups)
	}
	json.NewEncoder(w).Encode(map[string]string{
		"access_token": "fake-at",
		"token_type":   "Bearer",
		"id_token":     idToken,
	})
}

// makeIDToken creates a signed RS256 ID token using only stdlib.
func makeIDToken(key *rsa.PrivateKey, issuer, audience, subject, email, nonce string, groups []string) string {
	header := b64url(mustMarshal(map[string]string{"alg": "RS256", "kid": "k1"}))
	claims := map[string]any{
		"iss":   issuer,
		"sub":   subject,
		"aud":   audience,
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": nonce,
	}
	if email != "" {
		claims["email"] = email
	}
	if groups != nil {
		claims["groups"] = groups
	}
	payload := b64url(mustMarshal(claims))
	msg := header + "." + payload
	h := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		panic("makeIDToken: " + err.Error())
	}
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// rsaToJWK encodes a public key as a JWK map (RFC 7517 §6.3).
func rsaToJWK(pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": "k1",
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func b64url(b []byte) string          { return base64.RawURLEncoding.EncodeToString(b) }
func mustMarshal(v any) []byte        { b, err := json.Marshal(v); fatalf(err); return b }
func fatalf(err error)                { if err != nil { panic(err) } }
func containsStr(s, sub string) bool  { return len(s) >= len(sub) && (s == sub || len(sub) == 0 || containsRune(s, sub)) }

func containsRune(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── env helpers ───────────────────────────────────────────────────────────────

var oidcEnvKeys = []string{
	"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
	"OIDC_REDIRECT_URL", "OIDC_DEFAULT_GRANTS", "OIDC_TOKEN_TTL",
	"OIDC_GROUPS_CLAIM", "OIDC_GROUP_MAPPINGS",
}

// resetOIDCEnv saves all OIDC env vars, clears them, and restores on cleanup.
func resetOIDCEnv(t *testing.T) {
	t.Helper()
	saved := make(map[string]string, len(oidcEnvKeys))
	had := make(map[string]bool, len(oidcEnvKeys))
	for _, k := range oidcEnvKeys {
		v, ok := os.LookupEnv(k)
		saved[k], had[k] = v, ok
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range oidcEnvKeys {
			if had[k] {
				os.Setenv(k, saved[k])
			} else {
				os.Unsetenv(k)
			}
		}
	})
}

// setRequiredOIDCEnv sets the three required vars to stub values.
func setRequiredOIDCEnv(t *testing.T) {
	t.Helper()
	os.Setenv("OIDC_ISSUER", "https://idp.example.com")
	os.Setenv("OIDC_CLIENT_ID", "test-client")
	os.Setenv("OIDC_CLIENT_SECRET", "test-secret")
	os.Setenv("OIDC_REDIRECT_URL", "https://forge.example.com/auth/oidc/callback")
}
