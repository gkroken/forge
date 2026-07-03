package ldap

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"forge/internal/auth"
)

// FromEnv reads LDAP configuration from environment variables.
// Returns nil if LDAP_URL is not set (LDAP disabled). Comma-separate LDAP_URL to
// list failover servers. Mirrors oidc.FromEnv.
func FromEnv() (*Config, error) {
	raw := os.Getenv("LDAP_URL")
	if raw == "" {
		return nil, nil
	}

	mappings, err := auth.ParseGroupMappings(os.Getenv("LDAP_GROUP_MAPPINGS"))
	if err != nil {
		return nil, fmt.Errorf("LDAP_GROUP_MAPPINGS: %w", err)
	}

	grants := []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
	if g := os.Getenv("LDAP_DEFAULT_GRANTS"); g != "" {
		if err := json.Unmarshal([]byte(g), &grants); err != nil {
			return nil, fmt.Errorf("LDAP_DEFAULT_GRANTS: %w", err)
		}
	}

	ttl := defaultTokenTTL
	if v := os.Getenv("LDAP_TOKEN_TTL"); v != "" {
		ttl, err = time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("LDAP_TOKEN_TTL: %w", err)
		}
	}

	cfg := &Config{
		URLs:               splitURLs(raw),
		StartTLS:           os.Getenv("LDAP_START_TLS") == "true",
		CACertFile:         os.Getenv("LDAP_CA_CERT"),
		InsecureSkipVerify: os.Getenv("LDAP_INSECURE_SKIP_VERIFY") == "true",
		BindDN:             os.Getenv("LDAP_BIND_DN"),
		BindPassword:       os.Getenv("LDAP_BIND_PASSWORD"),
		UserBaseDN:         os.Getenv("LDAP_USER_BASE_DN"),
		UserFilter:         os.Getenv("LDAP_USER_FILTER"),
		EmailAttr:          os.Getenv("LDAP_EMAIL_ATTR"),
		GroupMode:          os.Getenv("LDAP_GROUP_MODE"),
		GroupBaseDN:        os.Getenv("LDAP_GROUP_BASE_DN"),
		GroupFilter:        os.Getenv("LDAP_GROUP_FILTER"),
		GroupAttr:          os.Getenv("LDAP_GROUP_ATTR"),
		GroupMappings:      mappings,
		DefaultGrants:      grants,
		TokenTTL:           ttl,
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w (when LDAP_URL is set)", err)
	}
	return cfg, nil
}

// splitURLs splits a comma-separated URL list, trimming blanks.
func splitURLs(s string) []string {
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}
