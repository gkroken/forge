// Package ldap provides LDAP/Active-Directory authentication for forge.
// It wraps github.com/go-ldap/ldap/v3 behind a small surface the server package
// depends on. All directory interaction (dial, StartTLS, search-then-bind, group
// lookup) is contained here.
//
// The design mirrors internal/oidc: a Config assembled from flags/env, a Client
// that turns a (username, password) pair into a UserInfo{DN, Email, Groups}, and
// read-only accessors for the admin panel. Group→role resolution and session
// minting live in the server package (auth.GroupRoleMapper + establishSSOSession),
// shared with OIDC — this package only proves the identity and returns groups.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	goldap "github.com/go-ldap/ldap/v3"

	"forge/internal/auth"
)

// Config holds LDAP client configuration. Assemble it from environment variables
// (FromEnv) or command-line flags (see cmd/forge).
type Config struct {
	URLs               []string `json:"urls,omitempty"`               // ldap://host:389 / ldaps://host:636 — tried in order (failover)
	StartTLS           bool     `json:"startTLS,omitempty"`           // upgrade ldap:// connections to TLS before any bind
	CACertFile         string   `json:"caCertFile,omitempty"`         // optional custom CA PEM; empty = system roots
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty"` // dev/test only — disables TLS verification

	BindDN       string `json:"bindDN,omitempty"`       // service account for the search step (empty = anonymous search)
	BindPassword string `json:"bindPassword,omitempty"` // service-account password (secret)

	UserBaseDN string `json:"userBaseDN,omitempty"` // ou=people,dc=example,dc=com
	UserFilter string `json:"userFilter,omitempty"` // %s is replaced by the escaped login name, e.g. (uid=%s) — AD: (sAMAccountName=%s)
	EmailAttr  string `json:"emailAttr,omitempty"`  // attribute holding the user's email, default "mail"

	GroupMode   string `json:"groupMode,omitempty"`   // "memberof" (read attr off user entry — AD default) | "search"
	GroupBaseDN string `json:"groupBaseDN,omitempty"` // base DN for GroupMode=="search"
	GroupFilter string `json:"groupFilter,omitempty"` // %s is replaced by the escaped user DN, e.g. (&(objectClass=groupOfNames)(member=%s))
	GroupAttr   string `json:"groupAttr,omitempty"`   // attribute holding the group name, default "cn"

	GroupMappings []auth.GroupRule `json:"groupMappings,omitempty"` // IdP group → base role
	DefaultGrants []auth.Grant     `json:"defaultGrants,omitempty"` // fallback when no group matches, default read on *
	TokenTTL      time.Duration    `json:"-"`                       // lifetime of a minted session, default 8h; JSON as "tokenTTL" string
	Timeout       time.Duration    `json:"-"`                       // per-dial / per-operation timeout, default 5s; JSON as "timeout" string
}

// roleRuleJSON / grantJSON render roles as human strings ("read"/"write"/"admin")
// in config-as-code, rather than the numeric auth.Role used in persisted tokens.
type roleRuleJSON struct {
	Group string `json:"group"`
	Role  string `json:"role"`
}
type grantJSON struct {
	Repo string `json:"repo"`
	Role string `json:"role"`
}

// MarshalJSON renders durations as human strings ("8h", "5s") and roles as their
// names, matching the config-as-code convention (see cleanup.NamedPolicy). The
// group-mapping/grant fields are shadowed by string-role variants.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	a := alias(c)
	a.GroupMappings, a.DefaultGrants = nil, nil // rendered via the envelope below
	rules := make([]roleRuleJSON, len(c.GroupMappings))
	for i, r := range c.GroupMappings {
		rules[i] = roleRuleJSON{Group: r.Group, Role: r.Role.String()}
	}
	grants := make([]grantJSON, len(c.DefaultGrants))
	for i, g := range c.DefaultGrants {
		grants[i] = grantJSON{Repo: g.Repo, Role: g.Tier().String()}
	}
	return json.Marshal(&struct {
		alias
		TokenTTL      string         `json:"tokenTTL,omitempty"`
		Timeout       string         `json:"timeout,omitempty"`
		GroupMappings []roleRuleJSON `json:"groupMappings,omitempty"`
		DefaultGrants []grantJSON    `json:"defaultGrants,omitempty"`
	}{alias: a, TokenTTL: durString(c.TokenTTL), Timeout: durString(c.Timeout),
		GroupMappings: rules, DefaultGrants: grants})
}

// UnmarshalJSON parses the duration strings and the string-role group mappings /
// default grants. It shadows the embedded numeric-role fields so callers write
// "role": "admin", not "role": 3.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	aux := &struct {
		*alias
		TokenTTL      string         `json:"tokenTTL,omitempty"`
		Timeout       string         `json:"timeout,omitempty"`
		GroupMappings []roleRuleJSON `json:"groupMappings,omitempty"`
		DefaultGrants []grantJSON    `json:"defaultGrants,omitempty"`
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	for name, raw := range map[string]string{"tokenTTL": aux.TokenTTL, "timeout": aux.Timeout} {
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("ldap: invalid %s %q: %w", name, raw, err)
		}
		if name == "tokenTTL" {
			c.TokenTTL = d
		} else {
			c.Timeout = d
		}
	}
	c.GroupMappings = c.GroupMappings[:0]
	for _, r := range aux.GroupMappings {
		role := auth.BaseRoleFor(r.Role)
		if role == auth.RoleNone {
			return fmt.Errorf("ldap: group mapping %q: unknown role %q (want read|write|admin)", r.Group, r.Role)
		}
		c.GroupMappings = append(c.GroupMappings, auth.GroupRule{Group: r.Group, Role: role})
	}
	c.DefaultGrants = c.DefaultGrants[:0]
	for _, g := range aux.DefaultGrants {
		role := auth.BaseRoleFor(g.Role)
		if role == auth.RoleNone {
			return fmt.Errorf("ldap: default grant on %q: unknown role %q (want read|write|admin)", g.Repo, g.Role)
		}
		c.DefaultGrants = append(c.DefaultGrants, auth.GrantForRole(g.Repo, role))
	}
	return nil
}

func durString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

const (
	defaultUserFilter = "(uid=%s)"
	defaultEmailAttr  = "mail"
	defaultGroupMode  = "memberof"
	defaultGroupAttr  = "cn"
	defaultTokenTTL   = 8 * time.Hour
	defaultTimeout    = 5 * time.Second
)

// Validate checks that the required fields for an enabled LDAP config are set and
// coherent, and normalises defaults.
func (c *Config) Validate() error {
	if len(c.URLs) == 0 {
		return errors.New("LDAP URL must be set")
	}
	if c.UserBaseDN == "" {
		return errors.New("LDAP user base DN must be set")
	}
	if c.UserFilter == "" {
		c.UserFilter = defaultUserFilter
	}
	if !strings.Contains(c.UserFilter, "%s") {
		return fmt.Errorf("LDAP user filter %q must contain %%s (the login-name placeholder)", c.UserFilter)
	}
	if c.BindDN != "" && c.BindPassword == "" {
		return errors.New("LDAP bind password must be set when a bind DN is configured")
	}
	if c.EmailAttr == "" {
		c.EmailAttr = defaultEmailAttr
	}
	if c.GroupMode == "" {
		c.GroupMode = defaultGroupMode
	}
	switch c.GroupMode {
	case "memberof":
	case "search":
		if c.GroupBaseDN == "" {
			return errors.New("LDAP group base DN must be set for group mode \"search\"")
		}
		if !strings.Contains(c.GroupFilter, "%s") {
			return fmt.Errorf("LDAP group filter %q must contain %%s (the user-DN placeholder) for group mode \"search\"", c.GroupFilter)
		}
	default:
		return fmt.Errorf("LDAP group mode %q: want \"memberof\" or \"search\"", c.GroupMode)
	}
	if c.GroupAttr == "" {
		c.GroupAttr = defaultGroupAttr
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = defaultTokenTTL
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	return nil
}

// UserInfo holds the identity proven by a successful search-then-bind.
type UserInfo struct {
	DN       string   // the user's distinguished name (used as the session subject)
	Username string   // the login name as supplied
	Email    string   // from EmailAttr, may be empty
	Groups   []string // short group names, resolved per GroupMode
}

// Client is a ready-to-use LDAP authenticator. Construct one via New.
// It holds no persistent connection — each Authenticate dials fresh (with
// failover), which keeps the client stateless and safe under concurrency.
type Client struct {
	cfg    Config
	tlsCfg *tls.Config
}

// New validates cfg, builds its tls.Config, and returns a Client.
// It does not dial; connection happens per-login inside Authenticate.
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, // #nosec G402 -- opt-in dev/test flag, documented
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("ldap: read CA cert %s: %w", cfg.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ldap: CA cert %s: no certificates parsed", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &Client{cfg: cfg, tlsCfg: tlsCfg}, nil
}

// Authenticate performs search-then-bind:
//  1. dial the directory (failover across URLs), applying TLS / StartTLS;
//  2. bind as the service account (or anonymously) and search for the user;
//  3. re-bind as the found user DN with the supplied password (credential check);
//  4. collect the user's groups per GroupMode.
//
// An empty password is rejected outright: many servers treat a bind with an empty
// password as an unauthenticated (anonymous) success, which would be an auth bypass.
func (c *Client) Authenticate(ctx context.Context, username, password string) (UserInfo, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return UserInfo{}, errors.New("ldap: username and password required")
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	defer conn.Close()

	// Step 1: bind as the service account so we may search. Empty BindDN =
	// anonymous bind (some directories permit anonymous search).
	if c.cfg.BindDN != "" {
		if err := conn.Bind(c.cfg.BindDN, c.cfg.BindPassword); err != nil {
			return UserInfo{}, fmt.Errorf("ldap: service-account bind failed: %w", err)
		}
	}

	// Step 2: find the user entry. Escape the login name (RFC 4515) to prevent
	// LDAP filter injection.
	filter := strings.ReplaceAll(c.cfg.UserFilter, "%s", goldap.EscapeFilter(username))
	attrs := []string{c.cfg.EmailAttr}
	if c.cfg.GroupMode == "memberof" {
		attrs = append(attrs, "memberOf")
	}
	res, err := conn.Search(goldap.NewSearchRequest(
		c.cfg.UserBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		2, int(c.cfg.Timeout.Seconds()), false, filter, attrs, nil,
	))
	if err != nil {
		return UserInfo{}, fmt.Errorf("ldap: user search failed: %w", err)
	}
	if len(res.Entries) == 0 {
		return UserInfo{}, errors.New("ldap: user not found")
	}
	if len(res.Entries) > 1 {
		return UserInfo{}, errors.New("ldap: user filter matched multiple entries")
	}
	entry := res.Entries[0]

	// Step 3: re-bind as the user to verify the password.
	if err := conn.Bind(entry.DN, password); err != nil {
		return UserInfo{}, fmt.Errorf("ldap: user bind failed: %w", err)
	}

	info := UserInfo{
		DN:       entry.DN,
		Username: username,
		Email:    entry.GetAttributeValue(c.cfg.EmailAttr),
	}

	// Step 4: collect groups. In "search" mode we must re-bind as the service
	// account first, since the group tree may not be readable by the user.
	switch c.cfg.GroupMode {
	case "memberof":
		info.Groups = shortGroupNames(entry.GetAttributeValues("memberOf"))
	case "search":
		if c.cfg.BindDN != "" {
			if err := conn.Bind(c.cfg.BindDN, c.cfg.BindPassword); err != nil {
				return UserInfo{}, fmt.Errorf("ldap: re-bind for group search failed: %w", err)
			}
		}
		info.Groups, err = c.searchGroups(conn, entry.DN)
		if err != nil {
			return UserInfo{}, err
		}
	}
	return info, nil
}

// dial connects to the first reachable URL, applying TLS (ldaps) or StartTLS.
func (c *Client) dial(ctx context.Context) (*goldap.Conn, error) {
	var lastErr error
	for _, url := range c.cfg.URLs {
		conn, err := goldap.DialURL(url, goldap.DialWithTLSConfig(c.tlsCfg))
		if err != nil {
			lastErr = err
			continue
		}
		conn.SetTimeout(c.cfg.Timeout)
		if c.cfg.StartTLS && strings.HasPrefix(strings.ToLower(url), "ldap://") {
			if err := conn.StartTLS(c.tlsCfg); err != nil {
				conn.Close()
				lastErr = fmt.Errorf("ldap: StartTLS on %s: %w", url, err)
				continue
			}
		}
		return conn, nil
	}
	return nil, fmt.Errorf("ldap: all servers unreachable: %w", lastErr)
}

// searchGroups finds groups the user DN is a member of via GroupFilter.
func (c *Client) searchGroups(conn *goldap.Conn, userDN string) ([]string, error) {
	filter := strings.ReplaceAll(c.cfg.GroupFilter, "%s", goldap.EscapeFilter(userDN))
	res, err := conn.Search(goldap.NewSearchRequest(
		c.cfg.GroupBaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		0, int(c.cfg.Timeout.Seconds()), false, filter, []string{c.cfg.GroupAttr}, nil,
	))
	if err != nil {
		return nil, fmt.Errorf("ldap: group search failed: %w", err)
	}
	groups := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if v := e.GetAttributeValue(c.cfg.GroupAttr); v != "" {
			groups = append(groups, v)
		}
	}
	return groups, nil
}

// shortGroupNames turns a set of group DNs (from a memberOf attribute) into the
// leftmost RDN value of each — e.g. "cn=forge-admins,ou=groups,dc=x" → "forge-admins".
// This matches the short group names used in OIDC group→role mappings. Values that
// don't parse as a DN are passed through unchanged.
func shortGroupNames(dns []string) []string {
	out := make([]string, 0, len(dns))
	for _, dn := range dns {
		if parsed, err := goldap.ParseDN(dn); err == nil && len(parsed.RDNs) > 0 &&
			len(parsed.RDNs[0].Attributes) > 0 {
			out = append(out, parsed.RDNs[0].Attributes[0].Value)
			continue
		}
		out = append(out, dn)
	}
	return out
}

// Read-only accessors expose non-secret config for the admin UI. BindPassword is
// deliberately never exposed.

func (c *Client) URLs() []string          { return c.cfg.URLs }
func (c *Client) BindDN() string          { return c.cfg.BindDN }
func (c *Client) UserBaseDN() string      { return c.cfg.UserBaseDN }
func (c *Client) UserFilter() string      { return c.cfg.UserFilter }
func (c *Client) GroupMode() string       { return c.cfg.GroupMode }
func (c *Client) DefaultGrants() []auth.Grant { return c.cfg.DefaultGrants }
func (c *Client) TokenTTL() time.Duration { return c.cfg.TokenTTL }
