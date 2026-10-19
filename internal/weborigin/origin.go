// Package weborigin validates and canonicalizes configured and browser origins.
// It deliberately does not use Host or forwarding headers as an authority.
package weborigin

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Canonical returns an HTTP(S) origin with lowercase host, normalized IP and
// port, no default port, and no trailing slash. Configured Unicode domains must
// use their ASCII/punycode spelling, matching browser Origin serialization.
func Canonical(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "?#\\") || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("an HTTP(S) origin without credentials, path, query or fragment is required")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("an HTTP(S) origin is required")
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	bracketed := strings.HasPrefix(u.Host, "[")
	if host == "" || strings.ContainsAny(host, "%/\\") || (bracketed && (ipErr != nil || !ip.Is6())) || (!bracketed && strings.Contains(host, ":")) {
		return "", fmt.Errorf("invalid origin hostname")
	}
	for _, r := range host {
		if r > 127 {
			return "", fmt.Errorf("use the ASCII/punycode origin hostname")
		}
	}
	if ipErr == nil {
		host = ip.String()
		if ip.Is6() {
			host = "[" + host + "]"
		}
	} else if numericHostLabel(host) {
		// Browsers parse short/octal/hex/integer IPv4 names differently from
		// net/url (e.g. 127.1 => 127.0.0.1). Reject rather than accept settings
		// whose browser Origin would never match. Use canonical dotted IPv4.
		return "", fmt.Errorf("use a canonical dotted-decimal IPv4 origin")
	}
	port := u.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("invalid origin port")
		}
		port = strconv.Itoa(value)
	}
	if port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return scheme + "://" + host, nil
}

func numericHostLabel(host string) bool {
	label := strings.TrimSuffix(host, ".")
	label = label[strings.LastIndexByte(label, '.')+1:]
	if label == "" {
		return false
	}
	if strings.HasPrefix(label, "0x") {
		for _, c := range label[2:] {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	for _, c := range label {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
