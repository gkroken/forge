package maven

import (
	"path"
	"strings"

	"forge/internal/format"
)

// ClaimPath implements format.Handler. The claimable component is the
// artifact directory ("{groupId path}/{artifactId}"), so a claim like
// "com/acme/**" covers every artifact under the com.acme groupId and
// "com/acme/app" claims exactly one artifact. Both artifact files
// ("com/acme/app/1.0/app-1.0.jar") and maven-metadata.xml requests (artifact-
// or SNAPSHOT-level, with or without a checksum suffix) resolve to it; paths
// that don't follow Maven 2 layout are not claim targets.
func (h *Handler) ClaimPath(sub string) (string, bool) {
	sub = strings.Trim(sub, "/")
	if cs := checksumExt(sub); cs != "" {
		sub = strings.TrimSuffix(sub, "."+cs)
	}
	parts := strings.Split(sub, "/")
	if path.Base(sub) == "maven-metadata.xml" {
		parts = parts[:len(parts)-1]
		// SNAPSHOT metadata lives inside the version directory.
		if len(parts) > 0 && startsWithDigit(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		if len(parts) < 2 {
			return "", false
		}
		return strings.Join(parts, "/"), true
	}
	// Artifact file: {groupId...}/{artifactId}/{version}/{file}. The version
	// directory is the first digit-leading segment (same heuristic as
	// compKeyFromSub), and at least groupId+artifactId must precede it.
	if len(parts) < 4 {
		return "", false
	}
	verIdx := -1
	for i, p := range parts {
		if startsWithDigit(p) {
			verIdx = i
			break
		}
	}
	if verIdx < 2 {
		return "", false
	}
	return strings.Join(parts[:verIdx], "/"), true
}

// OwnsComponent implements format.Handler: a hosted Maven repo owns an
// artifact when any blob exists under its artifact directory. The trailing
// slash keeps sibling artifacts with a common prefix ("app" vs "app-extra")
// from shadowing each other.
func (h *Handler) OwnsComponent(c *format.Context, component string) bool {
	keys, err := c.Blob.List(c.Repo.Name + "/" + component + "/")
	return err == nil && len(keys) > 0
}

func startsWithDigit(s string) bool {
	return len(s) > 0 && s[0] >= '0' && s[0] <= '9'
}
