package helm

import (
	"path"
	"strings"

	"forge/internal/format"
)

// ClaimPath implements format.Handler. The claimable component is the chart
// name. Chart downloads ("{name}-{version}.tgz") and the per-chart API
// ("api/charts/{name}[/{version}]") resolve to it; index.yaml and the
// all-charts listing are not claim targets.
func (h *Handler) ClaimPath(sub string) (string, bool) {
	sub = strings.Trim(sub, "/")
	if rest, ok := strings.CutPrefix(sub, "api/charts/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		if name == "" {
			return "", false
		}
		return name, true
	}
	if strings.HasSuffix(sub, ".tgz") {
		if name := chartNameFromFilename(path.Base(sub)); name != "" {
			return name, true
		}
	}
	return "", false
}

// chartNameFromFilename splits "{name}-{version}.tgz" at the last dash that
// is followed by a digit — versions start with a digit while chart names
// routinely contain dashes ("my-chart-1.2.3.tgz" → "my-chart"). Same
// heuristic ChartMuseum-style servers use; ambiguous only for chart names
// whose final dash-separated token itself starts with a digit.
func chartNameFromFilename(f string) string {
	f = strings.TrimSuffix(f, ".tgz")
	for i := len(f) - 2; i > 0; i-- {
		if f[i] == '-' && f[i+1] >= '0' && f[i+1] <= '9' {
			return f[:i]
		}
	}
	return ""
}

// OwnsComponent implements format.Handler: a hosted Helm repo owns a chart
// when it holds a record for any version of it. Record keys are
// "{name}-{version}", so a prefix hit is confirmed against the record's Name
// (the prefix "my-" would otherwise match "my-chart-1.0").
func (h *Handler) OwnsComponent(c *format.Context, component string) bool {
	keys, _ := c.Meta.List(h.ns(c))
	prefix := component + "-"
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		var rec chartRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); ok && rec.Name == component {
			return true
		}
	}
	return false
}
