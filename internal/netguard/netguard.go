// Package netguard stops outbound HTTP (webhook deliveries) from reaching
// private networks.
//
// Tenants choose their webhook URLs. Without a guard, a tenant could point one
// at http://postgres.shared.svc.cluster.local, a cloud metadata endpoint
// (169.254.169.254) or anything else only the server can reach, and our worker
// would dutifully POST to it (SSRF).
//
// The check runs in the dialer's Control hook, i.e. on the IP address actually
// being connected to, after DNS resolution. Checking the URL when it is
// registered is not enough: its DNS can change afterwards (DNS rebinding).
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a destination is not a public address.
var ErrBlocked = errors.New("netguard: destination is not a public address")

// blocked lists ranges no webhook may reach.
var blocked = mustPrefixes(
	"0.0.0.0/8",       // "this" network
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // carrier-grade NAT, also Tailscale
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, cloud metadata
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, broadcast
	"::/128",          // unspecified
	"::1/128",         // loopback
	"64:ff9b::/96",    // NAT64 (can map to private IPv4)
	"100::/64",        // discard
	"2001:db8::/32",   // documentation
	"fc00::/7",        // unique local
	"fe80::/10",       // link-local
	"ff00::/8",        // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// IsPublic reports whether an address may be dialed.
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1
	if !addr.IsValid() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// control is the dialer hook: it sees the resolved IP:port.
func control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlocked, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !IsPublic(addr) {
		return fmt.Errorf("%w: %s", ErrBlocked, host)
	}
	return nil
}

// Client returns an HTTP client for calling tenant-supplied URLs. With
// allowPrivate (local development only) the guard is off.
//
// It never follows redirects (a public URL could redirect inward, and a
// webhook has no reason to redirect) and ignores HTTP_PROXY, which would
// otherwise dial the proxy instead of the checked address.
func Client(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = control
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          50,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// CheckURL validates a URL when it is registered, for early feedback: it must
// be absolute http(s), and a literal IP or obvious internal name must be
// public. Hostnames are resolved; if any address is private the URL is
// refused. The dial-time check in Client remains the real protection.
func CheckURL(ctx context.Context, raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials")
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".cluster.local") || !strings.Contains(host, ".") {
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("%s is not a public host", host)
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !IsPublic(addr) {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		// Unresolvable now may resolve later; the dial-time guard still holds.
		return nil
	}
	for _, a := range addrs {
		if !IsPublic(a) {
			return fmt.Errorf("%s resolves to a non-public address", host)
		}
	}
	return nil
}
