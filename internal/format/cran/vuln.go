package cran

import (
	"path"
	"strings"
)

// OSVEcosystem implements format.Handler. OSV publishes R advisories — the
// RSEC series — under the "CRAN" ecosystem, so CRAN packages are scannable.
// Returning "" here, which is what this format did, is not a decision anyone
// records: it just means vulnerability scanning quietly skips every CRAN
// repository, with no error and nothing in the UI.
//
// One limit, verified against the live API rather than assumed: RSEC advisories
// carry no CVSS vector and no curated severity label, so findings land as
// severity "unknown". Scanning therefore surfaces them in browse and detail,
// but vuln.Policy never acts on unknown by design (an unscored advisory must
// not block everything), so a policy cannot enforce on CRAN today. Enforcement
// would need a policy option for "act on any advisory, scored or not".
func (h *Handler) OSVEcosystem() string { return "CRAN" }

// OSVCoordinates implements format.Handler. The component is the R package
// name, which is exactly how OSV keys CRAN advisories, so the mapping is
// identity.
func (h *Handler) OSVCoordinates(component string) (ecosystem, name string, ok bool) {
	if component == "" {
		return "", "", false
	}
	return "CRAN", component, true
}

// VulnGateTarget implements format.Handler: downloading a package is what the
// gate acts on. Source tarballs and platform binaries are named
// "{Package}_{Version}.{ext}" and package names cannot contain "_", so the
// split is exact. PACKAGES indexes carry no version and are not gated — a
// blocked index would break the whole repository rather than one package.
func (h *Handler) VulnGateTarget(sub string) (component, version string, ok bool) {
	base := path.Base(strings.Trim(sub, "/"))
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if !strings.HasSuffix(base, ext) {
			continue
		}
		pkg, ver, found := strings.Cut(strings.TrimSuffix(base, ext), "_")
		if !found || pkg == "" || ver == "" {
			return "", "", false
		}
		return pkg, ver, true
	}
	return "", "", false
}
