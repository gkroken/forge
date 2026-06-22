package vuln_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/vuln"
)

// osvStub is a configurable fake OSV API. It serves /v1/querybatch from batch
// (positional results) and /v1/vulns/{id} from vulns, counting hydrate calls.
type osvStub struct {
	batch       [][]string        // per-query list of vuln IDs
	vulns       map[string]string // id → full JSON record
	hydrateHits int32             // GET /v1/vulns/* count
	batchFailN  int32             // first N /querybatch calls return 503
	batchCalls  int32             //
}

func (s *osvStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/querybatch", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&s.batchCalls, 1) <= atomic.LoadInt32(&s.batchFailN) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		results := make([]map[string]any, len(s.batch))
		for i, ids := range s.batch {
			vulns := make([]map[string]string, len(ids))
			for j, id := range ids {
				vulns[j] = map[string]string{"id": id, "modified": "2024-01-01T00:00:00Z"}
			}
			results[i] = map[string]any{"vulns": vulns}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/v1/vulns/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hydrateHits, 1)
		id := r.URL.Path[len("/v1/vulns/"):]
		body, ok := s.vulns[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	})
	return mux
}

func newClient(t *testing.T, s *osvStub) *vuln.Client {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return vuln.NewClient(srv.Client(), vuln.WithBaseURL(srv.URL), vuln.WithTimeout(2*time.Second))
}

const lodashVuln = `{
  "id": "GHSA-35jh-r3h4-6jhm",
  "summary": "Command Injection in lodash",
  "aliases": ["CVE-2021-23337"],
  "severity": [{"type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
  "database_specific": {"severity": "HIGH"},
  "affected": [{"ranges": [{"events": [{"introduced": "0"}, {"fixed": "4.17.21"}]}]}],
  "references": [{"type": "ADVISORY", "url": "https://github.com/advisories/GHSA-35jh-r3h4-6jhm"}]
}`

func TestQueryBatchAndHydrate(t *testing.T) {
	s := &osvStub{
		batch: [][]string{{"GHSA-35jh-r3h4-6jhm"}},
		vulns: map[string]string{"GHSA-35jh-r3h4-6jhm": lodashVuln},
	}
	c := newClient(t, s)

	got, err := c.Query(context.Background(), []vuln.Coordinate{
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.20"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("expected one advisory, got %+v", got)
	}
	a := got[0][0]
	if a.ID != "GHSA-35jh-r3h4-6jhm" {
		t.Errorf("ID = %q", a.ID)
	}
	if a.Severity != vuln.SeverityHigh { // from curated label
		t.Errorf("Severity = %v, want high", a.Severity)
	}
	if a.CVSS == "" {
		t.Error("raw CVSS vector should be preserved")
	}
	if len(a.FixedIn) != 1 || a.FixedIn[0] != "4.17.21" {
		t.Errorf("FixedIn = %v", a.FixedIn)
	}
	if a.URL != "https://github.com/advisories/GHSA-35jh-r3h4-6jhm" {
		t.Errorf("URL = %q", a.URL)
	}
}

func TestQueryDedupAndCache(t *testing.T) {
	s := &osvStub{
		// Two different coordinates, both affected by the SAME advisory id.
		batch: [][]string{{"GHSA-xxxx"}, {"GHSA-xxxx"}},
		vulns: map[string]string{"GHSA-xxxx": `{"id":"GHSA-xxxx","database_specific":{"severity":"LOW"}}`},
	}
	c := newClient(t, s)

	_, err := c.Query(context.Background(), []vuln.Coordinate{
		{Ecosystem: "npm", Name: "a", Version: "1"},
		{Ecosystem: "npm", Name: "b", Version: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&s.hydrateHits); got != 1 {
		t.Errorf("hydrate called %d times, want 1 (dedup within call)", got)
	}

	// A second Query for the same id must hit the persistent cache, not the API.
	if _, err := c.Query(context.Background(), []vuln.Coordinate{{Ecosystem: "npm", Name: "a", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&s.hydrateHits); got != 1 {
		t.Errorf("hydrate called %d times after cached re-query, want 1", got)
	}
}

func TestQueryCleanResult(t *testing.T) {
	s := &osvStub{batch: [][]string{{}}, vulns: map[string]string{}}
	c := newClient(t, s)
	got, err := c.Query(context.Background(), []vuln.Coordinate{{Ecosystem: "npm", Name: "safe", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("expected no advisories, got %+v", got)
	}
	if atomic.LoadInt32(&s.hydrateHits) != 0 {
		t.Error("clean result should not hydrate")
	}
}

func TestQueryRetriesOn5xx(t *testing.T) {
	s := &osvStub{
		batch:      [][]string{{}},
		vulns:      map[string]string{},
		batchFailN: 2, // fail twice, succeed on the third (maxRetries=2)
	}
	c := newClient(t, s)
	if _, err := c.Query(context.Background(), []vuln.Coordinate{{Ecosystem: "npm", Name: "x", Version: "1"}}); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&s.batchCalls); got != 3 {
		t.Errorf("batch called %d times, want 3 (2 fail + 1 ok)", got)
	}
}

func TestQueryEgressFailureReturnsError(t *testing.T) {
	// Point at a closed server: the client must return an error, never panic.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := vuln.NewClient(&http.Client{Timeout: time.Second}, vuln.WithBaseURL(url), vuln.WithTimeout(200*time.Millisecond))

	_, err := c.Query(context.Background(), []vuln.Coordinate{{Ecosystem: "npm", Name: "x", Version: "1"}})
	if err == nil {
		t.Fatal("expected an error on egress failure")
	}
}

func TestDeriveSeverityFromCVSSWhenNoLabel(t *testing.T) {
	// No database_specific.severity → severity computed from the CVSS vector.
	s := &osvStub{
		batch: [][]string{{"OSV-1"}},
		vulns: map[string]string{"OSV-1": `{"id":"OSV-1","severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}]}`},
	}
	c := newClient(t, s)
	got, err := c.Query(context.Background(), []vuln.Coordinate{{Ecosystem: "Maven", Name: "g:a", Version: "1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0][0].Severity != vuln.SeverityCritical { // 9.8 → critical
		t.Errorf("computed severity = %v, want critical", got[0][0].Severity)
	}
	// Fallback URL when no ADVISORY reference is present.
	if got[0][0].URL != "https://osv.dev/vulnerability/OSV-1" {
		t.Errorf("URL = %q", got[0][0].URL)
	}
}
