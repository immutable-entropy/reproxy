// Package upstream routes the proxy's outbound connections through a chain
// of other proxies.
//
// A chain is ordered: the first proxy is dialled directly, each proxy after
// it is reached through the one before it, and the target server is reached
// through the last. That encapsulation is what lets traffic reach a network
// that is only visible from behind several hops, such as a staging
// environment behind a local proxy.
//
// It is core (it sits under intercept/replay; see CLAUDE.md) and has no Qt
// dependency. Nothing here runs per message: a Dialer is built once when the
// settings change and used once per connection.
package upstream

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// Kind is an upstream proxy's protocol.
type Kind string

// The protocols a chain can use. HTTPS is HTTP CONNECT over a TLS
// connection to the proxy itself; the tunnel inside it is unchanged.
const (
	HTTP    Kind = "http"
	HTTPS   Kind = "https"
	SOCKS4  Kind = "socks4"
	SOCKS4a Kind = "socks4a"
	SOCKS5  Kind = "socks5"
)

// Kinds lists every supported protocol, in the order the UI offers them.
var Kinds = []Kind{HTTP, HTTPS, SOCKS4, SOCKS4a, SOCKS5}

// Valid reports whether k is a protocol this package speaks.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Proxy is one hop in the chain, as configured by the user. Its password is
// not kept here: it lives in the secrets file, keyed by ID.
type Proxy struct {
	// ID is a stable random identifier; it keys the saved password.
	ID string `json:"id"`
	// Enabled includes this proxy in the chain. A disabled proxy keeps its
	// settings but is skipped, so a hop can be taken out without losing it.
	Enabled bool `json:"enabled"`
	// Kind is the protocol spoken to this proxy.
	Kind Kind `json:"kind"`
	// Host is the proxy's host name or IP address, Port its TCP port.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Username authenticates to the proxy: HTTP Basic for HTTP/HTTPS,
	// USERID for SOCKS4/4a, RFC 1929 for SOCKS5. Empty means no
	// authentication is offered.
	Username string `json:"username,omitempty"`
}

// Addr returns the proxy's host:port.
func (p Proxy) Addr() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.Port)) }

// Chain is the configured list of upstream proxies, in the order they are
// traversed. It is stored in the settings file as Config.Upstream.
type Chain struct {
	// Enabled routes connections through the chain. Off, reProxy connects
	// to targets directly and the list is kept for later.
	Enabled bool `json:"enabled"`
	// VerifyTLS checks the certificate of every HTTPS proxy in the chain
	// against the system roots. Turn it off for a proxy with a self-signed
	// certificate. It says nothing about the target server's certificate,
	// which the proxy never sees.
	VerifyTLS bool `json:"verify_tls"`
	// Proxies is the chain, first hop first.
	Proxies []Proxy `json:"proxies"`
}

// DefaultChain returns the chain used on first launch: none configured, and
// HTTPS proxy certificates verified if one is added.
func DefaultChain() Chain { return Chain{VerifyTLS: true} }

// Active returns the hops that connections actually traverse: the enabled
// proxies, in order, or none when the chain itself is off.
func (c Chain) Active() []Proxy {
	if !c.Enabled {
		return nil
	}
	out := make([]Proxy, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// Validate reports whether the chain can be used. Disabled proxies are
// checked too, so a typo is caught where it was made rather than when the
// hop is later switched on.
func (c Chain) Validate() error {
	for i, p := range c.Proxies {
		if !p.Kind.Valid() {
			return fmt.Errorf("proxy %d: unknown type %q", i+1, p.Kind)
		}
		if strings.TrimSpace(p.Host) == "" {
			return fmt.Errorf("proxy %d: no host", i+1)
		}
		if strings.ContainsAny(p.Host, " \t/") {
			return fmt.Errorf("proxy %d: host %q is not a host name or IP address", i+1, p.Host)
		}
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("proxy %d (%s): port %d is out of range (1-65535)", i+1, p.Host, p.Port)
		}
		// A NUL would end the USERID field early and let the rest of the
		// name be read as the SOCKS4a hostname.
		if strings.ContainsRune(p.Username, 0) {
			return fmt.Errorf("proxy %d (%s): the username cannot contain a NUL byte", i+1, p.Host)
		}
		if (p.Kind == SOCKS4 || p.Kind == SOCKS4a) && len(p.Username) > 255 {
			return fmt.Errorf("proxy %d (%s): SOCKS4 usernames are at most 255 bytes", i+1, p.Host)
		}
		if p.Kind == SOCKS5 && len(p.Username) > 255 {
			return fmt.Errorf("proxy %d (%s): SOCKS5 usernames are at most 255 bytes", i+1, p.Host)
		}
	}
	return nil
}

// Hops turns the active proxies into the hops a Dialer traverses, taking
// each password from password(id). Returns nil when nothing is configured,
// which a Dialer treats as connecting directly.
func (c Chain) Hops(password func(id string) string) []Hop {
	active := c.Active()
	if len(active) == 0 {
		return nil
	}
	hops := make([]Hop, 0, len(active))
	for _, p := range active {
		var pw string
		if password != nil {
			pw = password(p.ID)
		}
		hops = append(hops, Hop{
			Kind:      p.Kind,
			Addr:      p.Addr(),
			Username:  p.Username,
			Password:  pw,
			VerifyTLS: c.VerifyTLS,
		})
	}
	return hops
}
