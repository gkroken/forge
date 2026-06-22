package vuln

import (
	"testing"

	"forge/internal/meta"
)

func adv(id string, sev Severity, aliases ...string) Advisory {
	return Advisory{ID: id, Severity: sev, Aliases: aliases}
}

func findingWith(advs ...Advisory) Finding {
	return Finding{Component: "lodash", Version: "1.0.0", Source: SourceOSV, Advisories: advs}
}

func TestPolicyDecision_Off(t *testing.T) {
	for _, p := range []Policy{{Mode: ModeOff}, {}} {
		got, _ := p.Decision(findingWith(adv("CVE-1", SeverityCritical)), true)
		if got != ActionServe {
			t.Fatalf("Off/empty mode should serve, got %v", got)
		}
	}
}

func TestPolicyDecision_Threshold(t *testing.T) {
	f := findingWith(adv("CVE-1", SeverityModerate))
	tests := []struct {
		mode      Mode
		threshold Severity
		want      Action
	}{
		{ModeBlock, SeverityHigh, ActionServe},     // moderate below high
		{ModeBlock, SeverityModerate, ActionBlock}, // at threshold
		{ModeBlock, SeverityLow, ActionBlock},      // above threshold
		{ModeWarn, SeverityModerate, ActionWarn},
		{ModeWarn, SeverityCritical, ActionServe},
	}
	for _, tc := range tests {
		p := Policy{Mode: tc.mode, Threshold: tc.threshold, FailOpen: true}
		if got, _ := p.Decision(f, true); got != tc.want {
			t.Errorf("mode=%s thr=%s: got %v want %v", tc.mode, tc.threshold, got, tc.want)
		}
	}
}

func TestPolicyDecision_WorstSeverityReported(t *testing.T) {
	f := findingWith(adv("CVE-1", SeverityLow), adv("CVE-2", SeverityCritical), adv("CVE-3", SeverityModerate))
	p := Policy{Mode: ModeBlock, Threshold: SeverityHigh}
	act, sev := p.Decision(f, true)
	if act != ActionBlock || sev != SeverityCritical {
		t.Fatalf("got (%v,%v) want (block,critical)", act, sev)
	}
}

func TestPolicyDecision_CleanAndUnknownAlwaysServe(t *testing.T) {
	p := Policy{Mode: ModeBlock, Threshold: SeverityUnknown} // most aggressive threshold
	// scanned-clean finding (no advisories)
	if act, _ := p.Decision(findingWith(), true); act != ActionServe {
		t.Fatalf("clean finding must serve even at unknown threshold, got %v", act)
	}
	// only unscored advisories
	if act, _ := p.Decision(findingWith(adv("CVE-x", SeverityUnknown)), true); act != ActionServe {
		t.Fatalf("unscored-only finding must serve, got %v", act)
	}
}

func TestPolicyDecision_Suppression(t *testing.T) {
	f := findingWith(adv("GHSA-aaa", SeverityCritical, "CVE-2021-1"), adv("CVE-2021-2", SeverityModerate))
	p := Policy{Mode: ModeBlock, Threshold: SeverityHigh}

	// suppress by primary ID
	p.Suppressions = []Suppression{{ID: "GHSA-aaa", Reason: "false positive"}}
	if act, sev := p.Decision(f, true); act != ActionServe || sev != SeverityModerate {
		t.Fatalf("suppress by id: got (%v,%v) want (serve,moderate)", act, sev)
	}
	// suppress by alias (case-insensitive)
	p.Suppressions = []Suppression{{ID: "cve-2021-1"}}
	if act, _ := p.Decision(f, true); act != ActionServe {
		t.Fatalf("suppress by alias should drop critical, got %v", act)
	}
}

func TestPolicyDecision_FailOpenClosed(t *testing.T) {
	unscanned := Finding{}
	open := Policy{Mode: ModeBlock, Threshold: SeverityHigh, FailOpen: true}
	if act, _ := open.Decision(unscanned, false); act != ActionServe {
		t.Fatalf("fail-open unscanned should serve, got %v", act)
	}
	closed := Policy{Mode: ModeBlock, Threshold: SeverityHigh, FailOpen: false}
	if act, _ := closed.Decision(unscanned, false); act != ActionBlock {
		t.Fatalf("fail-closed unscanned should block, got %v", act)
	}
	warnClosed := Policy{Mode: ModeWarn, FailOpen: false}
	if act, _ := warnClosed.Decision(unscanned, false); act != ActionWarn {
		t.Fatalf("fail-closed warn should warn, got %v", act)
	}
}

func TestPolicyManager_RoundTripAndResolve(t *testing.T) {
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pm := NewPolicyManager(m)

	// default with nothing stored → Off
	if d, err := pm.Default(); err != nil || d.Mode != ModeOff {
		t.Fatalf("empty default: got (%v,%v)", d.Mode, err)
	}

	strict := NamedPolicy{Name: "strict", Description: "block highs",
		Policy: Policy{Mode: ModeBlock, Threshold: SeverityHigh, FailOpen: true}}
	if err := pm.Put(strict); err != nil {
		t.Fatal(err)
	}
	got, ok, err := pm.Get("strict")
	if err != nil || !ok || got.Mode != ModeBlock || got.Threshold != SeverityHigh {
		t.Fatalf("get strict: (%+v,%v,%v)", got, ok, err)
	}
	if list, _ := pm.List(); len(list) != 1 {
		t.Fatalf("List len = %d want 1", len(list))
	}

	// Resolve: named ref wins
	if p, _ := pm.Resolve("strict"); p.Mode != ModeBlock {
		t.Fatalf("resolve named: got %v", p.Mode)
	}
	// Resolve: unknown name → falls through to default (Off)
	if p, _ := pm.Resolve("ghost"); p.Mode != ModeOff {
		t.Fatalf("resolve unknown → default off, got %v", p.Mode)
	}
	// set global default; empty ref resolves to it
	if err := pm.SetDefault(Policy{Mode: ModeWarn, Threshold: SeverityModerate}); err != nil {
		t.Fatal(err)
	}
	if p, _ := pm.Resolve(""); p.Mode != ModeWarn {
		t.Fatalf("resolve empty → default warn, got %v", p.Mode)
	}

	if err := pm.Put(NamedPolicy{Name: "Bad Name"}); err == nil {
		t.Fatal("expected invalid-name error")
	}
}
