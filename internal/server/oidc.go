package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"forge/internal/auth"
	forgeoidc "forge/internal/oidc"
)

// oidcProvider is the interface the OIDC handlers depend on.
// *forgeoidc.Provider satisfies it; tests use a fake implementation.
type oidcProvider interface {
	AuthURL(state, nonce string) string
	Exchange(ctx context.Context, code, nonce string) (forgeoidc.UserInfo, error)
	DefaultGrants() []auth.Grant
	TokenTTL() time.Duration
}

const (
	oidcStateCookie = "forge_oidc_state" // short-lived, SameSite=Lax
	oidcStateMaxAge = 10 * 60            // 10 minutes in seconds
)

// handleOIDCLogin starts the OIDC authorization code flow.
// It generates a random state (CSRF) and nonce (replay protection), stores
// them in a signed cookie, then redirects the browser to the IdP.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil || s.Auth == nil {
		http.Error(w, "OIDC not configured", http.StatusNotFound)
		return
	}

	state := randomHex(16)
	nonce := randomHex(16)

	cookieVal := signOIDCState(s.oidcKey, state, nonce)
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteLax already present
		Name:     oidcStateCookie,
		Value:    cookieVal,
		Path:     "/auth/oidc/",
		MaxAge:   oidcStateMaxAge,
		HttpOnly: true,
		Secure:   isSecureContext(r),
		SameSite: http.SameSiteLaxMode, // Lax required: IdP redirect back is cross-site
	})

	http.Redirect(w, r, s.OIDC.AuthURL(state, nonce), http.StatusFound)
}

// handleOIDCCallback handles the IdP redirect back to forge.
// It validates the state cookie, exchanges the code for an ID token,
// mints a forge token, sets the session cookie, and redirects to the UI.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil || s.Auth == nil {
		http.Error(w, "OIDC not configured", http.StatusNotFound)
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		slog.Warn("oidc: IdP returned error", "error", errParam, "description", desc)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	// Validate state against the signed cookie.
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil || state == "" {
		http.Redirect(w, r, "/ui/login?error=invalid", http.StatusSeeOther)
		return
	}
	nonce, ok := verifyOIDCState(s.oidcKey, cookie.Value, state)
	if !ok {
		http.Redirect(w, r, "/ui/login?error=invalid", http.StatusSeeOther)
		return
	}

	// Clear the state cookie immediately — it's single-use.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteLax already present
		Name:     oidcStateCookie,
		Value:    "",
		Path:     "/auth/oidc/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureContext(r),
		SameSite: http.SameSiteLaxMode,
	})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/ui/login?error=invalid", http.StatusSeeOther)
		return
	}

	info, err := s.OIDC.Exchange(r.Context(), code, nonce)
	if err != nil {
		slog.Warn("oidc: exchange failed", "err", err)
		http.Redirect(w, r, "/ui/login?error=oidc", http.StatusSeeOther)
		return
	}

	// Mint a forge token for this OIDC identity.
	label := info.Email
	if label == "" {
		label = info.Subject
	}
	desc := "oidc:" + label
	expiry := time.Now().Add(s.OIDC.TokenTTL())
	tok, secret, err := s.Auth.Create(desc, s.OIDC.DefaultGrants(), &expiry)
	if err != nil {
		slog.Error("oidc: token create failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("audit", "audit", true, "event", "oidc.login",
		"token_id", tok.ID, "subject", info.Subject, "email", info.Email)

	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteStrict already present
		Name:     auth.UISessionCookie,
		Value:    secret,
		Path:     "/",
		MaxAge:   int(s.OIDC.TokenTTL().Seconds()),
		HttpOnly: true,
		Secure:   isSecureContext(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// ── state cookie signing ──────────────────────────────────────────────────────

// signOIDCState produces a cookie value encoding state, nonce, and an
// HMAC-SHA256 signature over both.  Format: state.nonce.sig (all base64url).
func signOIDCState(key []byte, state, nonce string) string {
	sig := oidcHMAC(key, state, nonce)
	return fmt.Sprintf("%s.%s.%s",
		base64.RawURLEncoding.EncodeToString([]byte(state)),
		base64.RawURLEncoding.EncodeToString([]byte(nonce)),
		base64.RawURLEncoding.EncodeToString(sig),
	)
}

// verifyOIDCState parses and verifies a cookie value produced by signOIDCState.
// It returns the nonce and true if the cookie is valid and state matches.
func verifyOIDCState(key []byte, cookieVal, expectedState string) (nonce string, ok bool) {
	parts := strings.SplitN(cookieVal, ".", 3)
	if len(parts) != 3 {
		return "", false
	}
	stateBytes, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	nonceBytes, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sigBytes, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return "", false
	}
	if string(stateBytes) != expectedState {
		return "", false
	}
	expected := oidcHMAC(key, string(stateBytes), string(nonceBytes))
	if !hmac.Equal(sigBytes, expected) {
		return "", false
	}
	return string(nonceBytes), true
}

func oidcHMAC(key []byte, state, nonce string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(state))
	mac.Write([]byte("|"))
	mac.Write([]byte(nonce))
	return mac.Sum(nil)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oidc: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
