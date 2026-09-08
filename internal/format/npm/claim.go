package npm

import (
	"encoding/json"
	"net/url"
	"strings"

	"forge/internal/format"
)

// ClaimPath implements format.Handler. The claimable component is the
// package name, so a claim like "@acme/**" covers a scope and "left-pad"
// claims one package. Both packument requests ("{pkg}") and tarball downloads
// ("{pkg}/-/{file}.tgz") resolve to it; the "-/" registry endpoints and paths
// that aren't a valid flat or @scope/name shape are not claim targets.
func (h *Handler) ClaimPath(sub string) (string, bool) {
	s, err := url.PathUnescape(sub)
	if err != nil {
		s = sub // same tolerance as Serve: fall back to the raw path
	}
	s = strings.Trim(s, "/")
	if s == "" || s == "-" || strings.HasPrefix(s, "-/") {
		return "", false
	}
	if i := strings.Index(s, "/-/"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "@") {
		if strings.Count(s, "/") != 1 {
			return "", false
		}
	} else if strings.Contains(s, "/") {
		return "", false
	}
	return s, true
}

// OwnsComponent implements format.Handler: a hosted npm repo owns a package
// when it holds a packument for it.
func (h *Handler) OwnsComponent(c *format.Context, component string) bool {
	var raw json.RawMessage
	ok, _ := c.Meta.GetJSON(h.ns(c), component, &raw)
	return ok
}
