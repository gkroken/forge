package vuln

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// defaultOSVBaseURL is the public OSV.dev API. Free, keyless, ecosystem-aware.
const defaultOSVBaseURL = "https://api.osv.dev"

// Coordinate is one package@version to query, already mapped to OSV's ecosystem
// vocabulary (Name may differ from the forge component — e.g. Maven uses
// "groupId:artifactId"). See the OSVCoordinates handler seam.
type Coordinate struct {
	Ecosystem string
	Name      string
	Version   string
}

// Client queries OSV.dev. The flow follows OSV's documented two-step contract:
// POST /v1/querybatch returns only {id, modified} per vuln, so full advisory
// detail is hydrated via GET /v1/vulns/{id}. Hydrated advisories are cached by
// id+modified (advisories are shared across packages and updated retroactively,
// so modified is the invalidation key), avoiding both the query-per-package and
// re-hydrate-the-same-CVE anti-patterns. Pure stdlib; HTTP/2 is negotiated
// automatically over TLS (OSV caps HTTP/1.1 responses at 32 MiB).
type Client struct {
	http       *http.Client
	baseURL    string
	timeout    time.Duration
	maxRetries int

	mu    sync.Mutex
	cache map[string]Advisory // key: id + "@" + modified
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithBaseURL overrides the OSV API base URL (used by tests).
func WithBaseURL(u string) ClientOption {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithTimeout sets the per-request deadline (default 15s).
func WithTimeout(d time.Duration) ClientOption { return func(c *Client) { c.timeout = d } }

// NewClient returns an OSV client over hc (nil → a default http.Client).
func NewClient(hc *http.Client, opts ...ClientOption) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	c := &Client{
		http:       hc,
		baseURL:    defaultOSVBaseURL,
		timeout:    15 * time.Second,
		maxRetries: 2,
		cache:      map[string]Advisory{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Query returns the advisories for each coordinate, positionally aligned with
// the input (a coordinate with no known advisories yields a nil slice). It
// batches the lookup, then hydrates each unique vuln ID once. On a hard egress
// failure it returns an error and no partial results — callers (the scan job)
// treat this as retryable and never let it break a publish.
func (c *Client) Query(ctx context.Context, coords []Coordinate) ([][]Advisory, error) {
	if len(coords) == 0 {
		return nil, nil
	}

	req := osvBatchReq{Queries: make([]osvQuery, len(coords))}
	for i, co := range coords {
		req.Queries[i] = osvQuery{
			Package: osvPackage{Name: co.Name, Ecosystem: co.Ecosystem},
			Version: co.Version,
		}
	}
	var resp osvBatchResp
	if err := c.do(ctx, http.MethodPost, "/v1/querybatch", req, &resp); err != nil {
		return nil, fmt.Errorf("osv querybatch: %w", err)
	}

	out := make([][]Advisory, len(coords))
	hydrated := map[string]Advisory{} // de-dup within this call (atop the persistent cache)
	for i := 0; i < len(coords) && i < len(resp.Results); i++ {
		for _, vr := range resp.Results[i].Vulns {
			adv, ok := hydrated[vr.ID]
			if !ok {
				var err error
				adv, err = c.hydrate(ctx, vr.ID, vr.Modified)
				if err != nil {
					return nil, fmt.Errorf("osv hydrate %s: %w", vr.ID, err)
				}
				hydrated[vr.ID] = adv
			}
			out[i] = append(out[i], adv)
		}
	}
	return out, nil
}

// hydrate fetches full advisory detail for one vuln ID, serving from / filling
// the id+modified cache.
func (c *Client) hydrate(ctx context.Context, id, modified string) (Advisory, error) {
	key := id + "@" + modified
	c.mu.Lock()
	if a, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return a, nil
	}
	c.mu.Unlock()

	var v osvVuln
	if err := c.do(ctx, http.MethodGet, "/v1/vulns/"+url.PathEscape(id), nil, &v); err != nil {
		return Advisory{}, err
	}

	sev, vector := deriveSeverity(v)
	summary := v.Summary
	if summary == "" {
		summary = firstLine(v.Details)
	}
	adv := Advisory{
		ID:       v.ID,
		Aliases:  v.Aliases,
		Summary:  summary,
		Severity: sev,
		CVSS:     vector,
		FixedIn:  fixedVersions(v),
		URL:      advisoryURL(v),
	}
	c.mu.Lock()
	c.cache[key] = adv
	c.mu.Unlock()
	return adv, nil
}

// do issues an HTTP request with a per-request timeout and exponential back-off
// retry on network errors and 5xx (mirrors internal/proxy). 4xx is returned
// immediately (client errors don't fix themselves). body, when non-nil, is
// marshalled as JSON; the 200 response is decoded into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond):
			}
		}

		reqCtx := ctx
		var cancel context.CancelFunc
		if c.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, c.timeout)
		}

		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL+path, rdr)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			lastErr = fmt.Errorf("osv: status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("osv: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// --- OSV wire types (subset of the schema we consume) ---

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

type osvQuery struct {
	Package osvPackage `json:"package"`
	Version string     `json:"version,omitempty"`
}

type osvBatchReq struct {
	Queries []osvQuery `json:"queries"`
}

type osvVulnRef struct {
	ID       string `json:"id"`
	Modified string `json:"modified"`
}

type osvBatchResp struct {
	Results []struct {
		Vulns []osvVulnRef `json:"vulns"`
	} `json:"results"`
}

type osvSeverity struct {
	Type  string `json:"type"`  // e.g. "CVSS_V3", "CVSS_V4"
	Score string `json:"score"` // CVSS vector string (not a number)
}

type osvVuln struct {
	ID               string          `json:"id"`
	Summary          string          `json:"summary"`
	Details          string          `json:"details"`
	Aliases          []string        `json:"aliases"`
	Severity         []osvSeverity   `json:"severity"`
	DatabaseSpecific json.RawMessage `json:"database_specific"`
	Affected         []struct {
		Ranges []struct {
			Events []struct {
				Fixed string `json:"fixed"`
			} `json:"events"`
		} `json:"ranges"`
		DatabaseSpecific json.RawMessage `json:"database_specific"`
	} `json:"affected"`
	References []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"references"`
}

// deriveSeverity resolves a Severity bucket and the raw CVSS vector for an OSV
// record. Precedence: a curated database_specific.severity label (GHSA supplies
// this) wins; otherwise the CVSS v3.x base score is computed from the vector.
// The raw vector is always returned (preferring v3, then any) so the UI can show
// it regardless.
func deriveSeverity(v osvVuln) (Severity, string) {
	var vector string
	for _, s := range v.Severity {
		if strings.HasPrefix(s.Type, "CVSS_V3") {
			vector = s.Score
			break
		}
	}
	if vector == "" {
		for _, s := range v.Severity {
			if s.Score != "" {
				vector = s.Score
				break
			}
		}
	}

	if lbl := dbSpecificSeverity(v.DatabaseSpecific); lbl != SeverityUnknown {
		return lbl, vector
	}
	for _, af := range v.Affected {
		if lbl := dbSpecificSeverity(af.DatabaseSpecific); lbl != SeverityUnknown {
			return lbl, vector
		}
	}
	if score, ok := cvssBaseScore(vector); ok {
		return SeverityFromCVSS(score), vector
	}
	return SeverityUnknown, vector
}

func dbSpecificSeverity(raw json.RawMessage) Severity {
	if len(raw) == 0 {
		return SeverityUnknown
	}
	var ds struct {
		Severity string `json:"severity"`
	}
	_ = json.Unmarshal(raw, &ds)
	return ParseSeverity(ds.Severity)
}

func fixedVersions(v osvVuln) []string {
	var out []string
	seen := map[string]bool{}
	for _, af := range v.Affected {
		for _, r := range af.Ranges {
			for _, e := range r.Events {
				if e.Fixed != "" && !seen[e.Fixed] {
					seen[e.Fixed] = true
					out = append(out, e.Fixed)
				}
			}
		}
	}
	return out
}

func advisoryURL(v osvVuln) string {
	for _, r := range v.References {
		if strings.EqualFold(r.Type, "ADVISORY") && r.URL != "" {
			return r.URL
		}
	}
	return "https://osv.dev/vulnerability/" + v.ID
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
