package webhook

import (
	"net"
	"syscall"
	"testing"
)

func TestSSRFGuard_ValidateURL(t *testing.T) {
	g := NewSSRFGuard(false)
	cases := []struct {
		url     string
		allowed bool
	}{
		{"http://127.0.0.1/hook", false},          // loopback
		{"http://localhost/hook", false},          // loopback by name
		{"http://169.254.169.254/latest", false},  // cloud metadata (link-local)
		{"http://10.0.0.5/hook", false},           // private
		{"http://192.168.1.10/hook", false},       // private
		{"http://172.16.4.4/hook", false},         // private
		{"http://[::1]/hook", false},              // IPv6 loopback
		{"http://0.0.0.0/hook", false},            // unspecified
		{"ftp://example.com/hook", false},         // bad scheme
		{"http://8.8.8.8/hook", true},             // public
		{"https://93.184.216.34/hook", true},      // public (example.com's IP)
	}
	for _, c := range cases {
		err := g.ValidateURL(c.url)
		if (err == nil) != c.allowed {
			t.Errorf("ValidateURL(%q): allowed=%v, err=%v", c.url, c.allowed, err)
		}
	}
}

func TestSSRFGuard_AllowPrivateEscapeHatch(t *testing.T) {
	g := NewSSRFGuard(true)
	for _, u := range []string{"http://127.0.0.1/h", "http://10.1.2.3/h", "http://169.254.169.254/h"} {
		if err := g.ValidateURL(u); err != nil {
			t.Errorf("with allowPrivate, %q should pass, got %v", u, err)
		}
	}
	// Bad scheme is still rejected even with the escape hatch.
	if err := g.ValidateURL("gopher://10.0.0.1"); err == nil {
		t.Error("bad scheme should be rejected even when allowPrivate")
	}
}

// TestSSRFGuard_ControlBlocksRebinding simulates the dial-time check: a hostname
// that resolved to a private IP (rebinding) is refused before connect.
func TestSSRFGuard_ControlBlocksRebinding(t *testing.T) {
	g := NewSSRFGuard(false)
	var rc syscall.RawConn
	if err := g.Control("tcp", "169.254.169.254:80", rc); err == nil {
		t.Error("Control should block the metadata address at dial time")
	}
	if err := g.Control("tcp", "10.0.0.9:443", rc); err == nil {
		t.Error("Control should block a private address at dial time")
	}
	if err := g.Control("tcp", "8.8.8.8:443", rc); err != nil {
		t.Errorf("Control should allow a public address, got %v", err)
	}
	// Escape hatch lets everything dial.
	if err := NewSSRFGuard(true).Control("tcp", "127.0.0.1:80", rc); err != nil {
		t.Errorf("allowPrivate Control should permit loopback, got %v", err)
	}
}

func TestSSRFGuard_blockedClassification(t *testing.T) {
	g := NewSSRFGuard(false)
	blocked := []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.0.1", "172.31.255.255",
		"169.254.169.254", "fe80::1", "224.0.0.1", "0.0.0.0", "fc00::1"}
	for _, s := range blocked {
		if !g.blocked(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if g.blocked(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}
