package upstream

import (
	"context"
	"fmt"
	"io"
	"net"
)

// SOCKS4/4a protocol constants. SOCKS4 has no RFC; this follows the
// original specification and the SOCKS4a extension that adds host names.
const (
	socks4Version = 0x04
	socks4Connect = 0x01

	socks4Granted      = 0x5A
	socks4Rejected     = 0x5B
	socks4NoIdentd     = 0x5C
	socks4IdentdFailed = 0x5D
)

// socks4Connect asks a SOCKS4 or SOCKS4a proxy to connect to target.
//
// SOCKS4 carries only an IPv4 address, so a host name has to be resolved
// here — which means the name is looked up on this machine, not behind the
// proxy. SOCKS4a sends the name instead and leaves the lookup to the proxy;
// prefer it when the target is only resolvable over there.
func (h Hop) socks4Connect(ctx context.Context, c net.Conn, target string) (net.Conn, error) {
	host, port, err := splitHostPort(target)
	if err != nil {
		return nil, err
	}

	req := make([]byte, 0, 9+len(h.Username)+len(host))
	req = append(req, socks4Version, socks4Connect, byte(port>>8), byte(port))

	var name string
	switch ip := net.ParseIP(host); {
	case ip != nil && ip.To4() != nil:
		req = append(req, ip.To4()...)
	case ip != nil:
		return nil, fmt.Errorf("SOCKS4 cannot reach the IPv6 address %s; use SOCKS5 for this hop", host)
	case h.Kind == SOCKS4a:
		// 0.0.0.x with x non-zero is the SOCKS4a marker: the host name
		// follows the user ID.
		req = append(req, 0, 0, 0, 1)
		name = host
	default:
		ip4, err := resolve4(ctx, host)
		if err != nil {
			return nil, err
		}
		req = append(req, ip4...)
	}

	req = append(req, h.Username...)
	req = append(req, 0)
	if name != "" {
		req = append(req, name...)
		req = append(req, 0)
	}
	if _, err := c.Write(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var reply [8]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return nil, fmt.Errorf("reading reply: %w", err)
	}
	if reply[1] != socks4Granted {
		return nil, fmt.Errorf("refused connection to %s: %s", target, socks4Status(reply[1]))
	}
	return c, nil
}

func socks4Status(code byte) string {
	switch code {
	case socks4Rejected:
		return "request rejected or failed"
	case socks4NoIdentd:
		return "the proxy could not reach an identd on this machine"
	case socks4IdentdFailed:
		return "identd reported a different user than the one sent"
	}
	return fmt.Sprintf("status 0x%02x", code)
}

// resolve4 looks up the first IPv4 address of host, for plain SOCKS4.
func resolve4(ctx context.Context, host string) (net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolving %s for SOCKS4: %w", host, err)
	}
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("%s has no IPv4 address; SOCKS4 cannot reach it (use SOCKS4a or SOCKS5)", host)
}
