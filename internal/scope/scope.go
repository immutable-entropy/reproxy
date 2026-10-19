// Package scope enforces the rule from CLAUDE.md that an LLM only sends
// requests to the domain the original request targeted.
//
// "Same domain" means the same registrable domain (eTLD+1, from the public
// suffix list): a flow captured from api.example.com lets an agent reach
// www.example.com but not example.org. Hosts without a registrable domain
// (IP addresses, single-label names such as localhost, or a bare public
// suffix) must match exactly. The port is not restricted.
//
// Widening this rule needs an explicit decision from the project owner.
package scope

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Check reports whether a request may be sent to targetAddr (host:port to
// connect to) carrying hostHeader (the Host header, possibly empty), given
// that it derives from a flow whose host was originHost (host or host:port).
func Check(originHost, targetAddr, hostHeader string) error {
	origin := Domain(originHost)
	if origin == "" {
		return fmt.Errorf("scope: the original request has no host")
	}
	if d := Domain(targetAddr); d != origin {
		return fmt.Errorf("scope: %s is outside %s, the original request's domain", hostOnly(targetAddr), origin)
	}
	if hostHeader != "" {
		if d := Domain(hostHeader); d != origin {
			return fmt.Errorf("scope: Host header %q is outside %s, the original request's domain", hostHeader, origin)
		}
	}
	return nil
}

// Domain returns the unit the scope rule compares: the registrable domain
// of host (which may carry a port), or the normalised host itself when it
// has none.
func Domain(host string) string {
	h := hostOnly(host)
	if h == "" {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil || !strings.Contains(h, ".") {
		return h
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return h // e.g. the host is itself a public suffix
	}
	return d
}

// hostOnly strips any port, IPv6 brackets and trailing dot, and lowercases.
func hostOnly(s string) string {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	return strings.ToLower(strings.TrimSuffix(s, "."))
}
