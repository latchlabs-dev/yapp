package hooks

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateSubscriptionURL checks that a hook endpoint is safe to call.
//
// Hook URLs are supplied by administrators and stored, so they are an SSRF
// vector aimed squarely at the proxy's own network position: a URL pointing at
// 169.254.169.254 or an internal admin port would have the data plane fetch it
// with the data plane's credentials and reachability. The check therefore
// resolves the hostname rather than only rejecting literal private IPs, since
// an attacker controls their own DNS and can point a public name at a private
// address.
//
// allowPrivate exists for local development and the live-test harness; it must
// stay false in production.
func ValidateSubscriptionURL(ctx context.Context, raw string, allowPrivate bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("parse hook url: %w", err)
	}
	if parsed.Scheme != "https" {
		if !(allowPrivate && parsed.Scheme == "http") {
			return fmt.Errorf("hook url must use https, got %q", parsed.Scheme)
		}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("hook url must include a host")
	}
	if allowPrivate {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("hook url may not target %s", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("hook url may not target the private address %s", ip)
		}
		return nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve hook host %s: %w", host, err)
	}
	// Every resolved address must be public. One private answer among several
	// is enough for a DNS-rebinding attacker to win a race.
	for _, addr := range addrs {
		if isBlockedIP(addr.IP) {
			return fmt.Errorf("hook host %s resolves to the private address %s", host, addr.IP)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}
