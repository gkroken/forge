package format_test

import (
	"testing"
	"time"

	"forge/internal/format"
	"forge/internal/proxy"
	"forge/internal/repo"
)

// MetadataMaxAge and ContentMaxAge age differently on purpose: an artifact at a
// fixed coordinate never changes, while the index listing it grows on every
// upstream publish. The admin API and config accepted MetadataMaxAge for a long
// time while nothing read it — ConfigForRepo only consults ContentMaxAge — so
// the setting silently did nothing for every format.
func TestProxyMetadataConfig(t *testing.T) {
	content := 24 * time.Hour
	metadata := 5 * time.Minute

	t.Run("metadata age overrides content age", func(t *testing.T) {
		c := &format.Context{Repo: repo.Repository{
			Name: "p", Kind: repo.Proxy, ContentMaxAge: &content, MetadataMaxAge: &metadata,
		}}
		if got := c.ProxyConfig().TTL; got != content {
			t.Errorf("ProxyConfig TTL = %v, want the content age %v", got, content)
		}
		if got := c.ProxyMetadataConfig().TTL; got != metadata {
			t.Errorf("ProxyMetadataConfig TTL = %v, want the metadata age %v — "+
				"the setting is ignored and indexes go stale for the content TTL", got, metadata)
		}
	})

	t.Run("falls back to the content age when unset", func(t *testing.T) {
		c := &format.Context{Repo: repo.Repository{
			Name: "p", Kind: repo.Proxy, ContentMaxAge: &content,
		}}
		if got := c.ProxyMetadataConfig().TTL; got != content {
			t.Errorf("with no metadata age, TTL = %v, want the content age %v", got, content)
		}
	})

	t.Run("zero metadata age is ignored, not treated as instant expiry", func(t *testing.T) {
		zero := time.Duration(0)
		c := &format.Context{Repo: repo.Repository{
			Name: "p", Kind: repo.Proxy, ContentMaxAge: &content, MetadataMaxAge: &zero,
		}}
		if got := c.ProxyMetadataConfig().TTL; got != content {
			t.Errorf("zero metadata age gave TTL %v; it must fall back to %v rather than "+
				"re-fetching every index read", got, content)
		}
	})

	t.Run("carries the rest of the proxy config", func(t *testing.T) {
		c := &format.Context{Repo: repo.Repository{
			Name: "p", Kind: repo.Proxy, MetadataMaxAge: &metadata, ProxyAuth: "Basic xyz",
		}}
		cfg := c.ProxyMetadataConfig()
		if cfg.Auth != "Basic xyz" {
			t.Errorf("metadata config dropped upstream auth: %+v", cfg)
		}
		if cfg.TTL != metadata {
			t.Errorf("TTL = %v, want %v", cfg.TTL, metadata)
		}
		_ = proxy.Config(cfg)
	})
}
