package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/obs"
	"forge/internal/queue"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

// TestGovernanceRecorders exercises the audit + metric + webhook branches of the
// governance recorders on a fully-wired server. These fire off the request path
// (best-effort), so all three sinks must be present to cover every branch.
func TestGovernanceRecorders(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "npm-hosted", "npm")
	al := obs.NewAuditLog(20)
	promReg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(promReg)
	eng := webhook.New(s.Meta, queue.NewMem(8), nil).WithSSRFGuard(webhook.NewSSRFGuard(true))
	s.WithAuditLog(al).WithMetrics(metrics, promReg).WithWebhooks(eng)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/repos/npm-hosted/x", nil)
	rp, _ := s.Repos.Get("npm-hosted")

	s.recordPromotion(r, "tester", ProvenanceRecord{
		SourceRepo: "npm-stage", TargetRepo: "npm-hosted", Component: "leftpad", Version: "1.2.3",
		SourceDigest: "deadbeef",
	}, 4096)
	s.recordQuotaBlock(r, rp, 200, 100)
	s.recordVulnGate(r, rp, "leftpad", "1.2.3", vuln.SeverityHigh, vuln.ActionBlock)
	s.recordVulnGate(r, rp, "leftpad", "1.2.3", vuln.SeverityLow, vuln.ActionWarn)

	// onProxyCacheFill: valid key dispatches; empty/keyless keys short-circuit.
	s.onProxyCacheFill("npm-hosted/leftpad/-/leftpad-1.2.3.tgz")
	s.onProxyCacheFill("")

	// The synchronous audit surface recorded promotion + quota + 2 vuln decisions.
	rows := al.Recent(10)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %d, want 4 (promote, quota, block, warn)", len(rows))
	}
	// The blocked vuln decision is the 403; the warn is a 200.
	var have403, have507 bool
	for _, e := range rows {
		switch e.Status {
		case http.StatusForbidden:
			have403 = true
		case http.StatusInsufficientStorage:
			have507 = true
		}
	}
	if !have403 || !have507 {
		t.Errorf("expected a 403 (vuln block) and a 507 (quota) audit row; rows=%+v", rows)
	}
}

// TestOnProxyCacheFill_NoWebhooks is the no-op branch when webhooks are off.
func TestOnProxyCacheFill_NoWebhooks(t *testing.T) {
	s := newMigrationServer(t)
	// No webhooks wired → returns immediately without panicking.
	s.onProxyCacheFill("npm-hosted/x")
}
