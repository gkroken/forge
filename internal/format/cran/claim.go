package cran

import (
	"path"
	"strings"

	"forge/internal/format"
)

var _ format.Claimable = (*Handler)(nil)

// ClaimPath implements format.Claimable. The claimable component is the R
// package name. Source tarballs ("src/contrib/{pkg}_{ver}.tar.gz", including
// Archive paths) and binary packages ("bin/.../{pkg}_{ver}.zip|.tgz") resolve
// to it — CRAN package names cannot contain "_", so the name is everything
// before the first underscore. PACKAGES indexes are not claim targets.
func (h *Handler) ClaimPath(sub string) (string, bool) {
	base := path.Base(strings.Trim(sub, "/"))
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if !strings.HasSuffix(base, ext) {
			continue
		}
		pkg, _, ok := strings.Cut(strings.TrimSuffix(base, ext), "_")
		if !ok || pkg == "" {
			return "", false
		}
		return pkg, true
	}
	return "", false
}

// OwnsComponent implements format.Claimable: a hosted CRAN repo owns a
// package when it holds a source record for any version. Record keys are
// "{Package}_{Version}" and package names cannot contain "_", so the prefix
// match is exact. Binary-only hosted packages are not auto-detected — protect
// those with an explicit claim.
func (h *Handler) OwnsComponent(c *format.Context, component string) bool {
	keys, _ := c.Meta.List(h.ns(c))
	prefix := component + "_"
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
