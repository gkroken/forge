package webhook

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// SSRFGuard validates webhook target URLs against an allow/deny policy and, via
// Control, enforces the same policy at dial time so a hostname that passes
// validation but later resolves (or re-resolves) to a blocked address — a DNS
// rebinding attack — is still refused before the socket connects.
//
// By default it blocks any address that is loopback, link-local (incl. the
// 169.254.169.254 cloud-metadata endpoint), private (RFC 1918 / ULA),
// unspecified, or multicast. Set allowPrivate (WEBHOOK_ALLOW_PRIVATE) for
// internal-only deployments where receivers live on a private network.
type SSRFGuard struct {
	allowPrivate bool
}

// NewSSRFGuard returns a guard. allowPrivate=true disables the private/loopback
// blocking (internal-only escape hatch).
func NewSSRFGuard(allowPrivate bool) *SSRFGuard { return &SSRFGuard{allowPrivate: allowPrivate} }

// ValidateURL rejects a target whose scheme isn't http(s) or whose host resolves
// to a blocked address. A literal IP is checked directly; a hostname is resolved
// and every returned address must be allowed (a single blocked answer fails).
func (g *SSRFGuard) ValidateURL(raw string) error {
	if g == nil {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}
	if g.allowPrivate {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if g.blocked(ip) {
			return fmt.Errorf("target address %s is not allowed (private/loopback/metadata)", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}
	for _, ip := range ips {
		if g.blocked(ip) {
			return fmt.Errorf("host %q resolves to a blocked address %s", host, ip)
		}
	}
	return nil
}

// Control is an http.Transport DialContext Control hook: it runs after DNS
// resolution with the concrete address about to be dialed, so it catches
// rebinding that slipped past ValidateURL. Wire it via HTTPClient.
func (g *SSRFGuard) Control(_, address string, _ syscall.RawConn) error {
	if g == nil || g.allowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip != nil && g.blocked(ip) {
		return fmt.Errorf("webhook: refusing to dial blocked address %s", ip)
	}
	return nil
}

// HTTPClient returns an http.Client whose transport enforces Control at dial
// time, with the given per-request timeout.
func (g *SSRFGuard) HTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: g.Control}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// blocked reports whether ip is in a denied range.
func (g *SSRFGuard) blocked(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return true
	}
	// 169.254.169.254 is link-local (already blocked), but spell it out as the
	// canonical cloud-metadata endpoint for clarity.
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	return false
}
