package ldap

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"forge/internal/auth"
)

func TestConfigJSON_RoundTrip(t *testing.T) {
	in := Config{
		URLs:          []string{"ldaps://dc1:636", "ldap://dc2:389"},
		StartTLS:      true,
		BindDN:        "cn=svc,dc=x",
		BindPassword:  "s3cret",
		UserBaseDN:    "ou=people,dc=x",
		UserFilter:    "(sAMAccountName=%s)",
		GroupMappings: []auth.GroupRule{{Group: "forge-admins", Role: auth.RoleAdmin}},
		TokenTTL:      2 * time.Hour,
		Timeout:       3 * time.Second,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	// Durations render as human strings under camelCase keys.
	if !strings.Contains(string(b), `"tokenTTL":"2h0m0s"`) {
		t.Errorf("tokenTTL not a duration string: %s", b)
	}
	if !strings.Contains(string(b), `"userBaseDN":"ou=people,dc=x"`) {
		t.Errorf("expected camelCase keys: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.TokenTTL != 2*time.Hour || out.Timeout != 3*time.Second {
		t.Errorf("durations lost: ttl=%v timeout=%v", out.TokenTTL, out.Timeout)
	}
	if out.UserFilter != in.UserFilter || len(out.GroupMappings) != 1 {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}

func TestConfigValidate_DefaultsAndErrors(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"no url", Config{UserBaseDN: "dc=x"}, true},
		{"no base dn", Config{URLs: []string{"ldap://h"}}, true},
		{"filter missing %s", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", UserFilter: "(uid=bob)"}, true},
		{"bind dn without password", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", BindDN: "cn=svc"}, true},
		{"search mode needs group base", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", GroupMode: "search", GroupFilter: "(member=%s)"}, true},
		{"search mode filter missing %s", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", GroupMode: "search", GroupBaseDN: "ou=g", GroupFilter: "(member=x)"}, true},
		{"bad group mode", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", GroupMode: "nope"}, true},
		{"minimal ok", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x"}, false},
		{"search ok", Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x", GroupMode: "search", GroupBaseDN: "ou=g", GroupFilter: "(member=%s)"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidate_FillsDefaults(t *testing.T) {
	cfg := Config{URLs: []string{"ldap://h"}, UserBaseDN: "dc=x"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.UserFilter != defaultUserFilter {
		t.Errorf("UserFilter = %q, want default", cfg.UserFilter)
	}
	if cfg.EmailAttr != defaultEmailAttr {
		t.Errorf("EmailAttr = %q, want %q", cfg.EmailAttr, defaultEmailAttr)
	}
	if cfg.GroupMode != defaultGroupMode {
		t.Errorf("GroupMode = %q, want %q", cfg.GroupMode, defaultGroupMode)
	}
	if cfg.GroupAttr != defaultGroupAttr {
		t.Errorf("GroupAttr = %q, want %q", cfg.GroupAttr, defaultGroupAttr)
	}
	if cfg.TokenTTL != defaultTokenTTL {
		t.Errorf("TokenTTL = %v, want %v", cfg.TokenTTL, defaultTokenTTL)
	}
	if cfg.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, defaultTimeout)
	}
}

func TestShortGroupNames(t *testing.T) {
	got := shortGroupNames([]string{
		"cn=forge-admins,ou=groups,dc=example,dc=com",
		"CN=forge-devs,OU=Groups,DC=example,DC=com",
		"not-a-dn",
	})
	want := []string{"forge-admins", "forge-devs", "not-a-dn"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitURLs(t *testing.T) {
	got := splitURLs(" ldap://a:389 , ldaps://b:636 ,, ")
	want := []string{"ldap://a:389", "ldaps://b:636"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAuthenticate_RejectsEmptyPassword(t *testing.T) {
	c, err := New(Config{URLs: []string{"ldap://127.0.0.1:1"}, UserBaseDN: "dc=x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Authenticate(t.Context(), "bob", ""); err == nil {
		t.Fatal("expected error for empty password (anonymous-bind bypass), got nil")
	}
}

func TestFromEnv_DisabledWhenUnset(t *testing.T) {
	t.Setenv("LDAP_URL", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config when LDAP_URL unset, got %+v", cfg)
	}
}

func TestFromEnv_ParsesConfig(t *testing.T) {
	t.Setenv("LDAP_URL", "ldap://a:389, ldaps://b:636")
	t.Setenv("LDAP_START_TLS", "true")
	t.Setenv("LDAP_BIND_DN", "cn=svc,dc=x")
	t.Setenv("LDAP_BIND_PASSWORD", "secret")
	t.Setenv("LDAP_USER_BASE_DN", "ou=people,dc=x")
	t.Setenv("LDAP_USER_FILTER", "(sAMAccountName=%s)")
	t.Setenv("LDAP_GROUP_MAPPINGS", "forge-admins:admin,devs:write")
	t.Setenv("LDAP_TOKEN_TTL", "2h")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.URLs) != 2 || cfg.URLs[0] != "ldap://a:389" || cfg.URLs[1] != "ldaps://b:636" {
		t.Errorf("URLs = %v", cfg.URLs)
	}
	if !cfg.StartTLS {
		t.Error("StartTLS not set")
	}
	if cfg.TokenTTL != 2*time.Hour {
		t.Errorf("TokenTTL = %v", cfg.TokenTTL)
	}
	if len(cfg.GroupMappings) != 2 {
		t.Fatalf("GroupMappings = %v", cfg.GroupMappings)
	}
	if cfg.GroupMappings[0].Group != "forge-admins" || cfg.GroupMappings[0].Role != auth.RoleAdmin {
		t.Errorf("mapping[0] = %+v", cfg.GroupMappings[0])
	}
}
