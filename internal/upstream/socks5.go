package upstream

import (
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS5 protocol constants (RFC 1928) and its username/password
// authentication (RFC 1929).
const (
	socks5Version = 0x05
	socks5Connect = 0x01

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF

	userPassVersion = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04
)

// socks5Connect negotiates with a SOCKS5 proxy and asks it to connect to
// target. A host name is passed through as a name, so it is resolved by the
// proxy rather than here — which is what makes a host that only exists
// behind the proxy reachable.
func (h Hop) socks5Connect(c net.Conn, target string) (net.Conn, error) {
	if err := h.socks5Auth(c); err != nil {
		return nil, err
	}

	host, port, err := splitHostPort(target)
	if err != nil {
		return nil, err
	}
	req := make([]byte, 0, 22+len(host))
	req = append(req, socks5Version, socks5Connect, 0x00)
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return nil, fmt.Errorf("host name %q is too long for SOCKS5 (255 bytes)", host)
		}
		req = append(req, atypDomain, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, atypIPv4)
		req = append(req, ip.To4()...)
	default:
		req = append(req, atypIPv6)
		req = append(req, ip.To16()...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var reply [4]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return nil, fmt.Errorf("reading reply: %w", err)
	}
	if reply[0] != socks5Version {
		return nil, fmt.Errorf("reply has SOCKS version %d, not 5", reply[0])
	}
	if reply[1] != 0x00 {
		return nil, fmt.Errorf("refused connection to %s: %s", target, socks5Status(reply[1]))
	}
	// The bound address follows and has to be read off even though a
	// CONNECT client has no use for it, or the first bytes of the tunnel
	// would be these.
	if err := discardSocks5Addr(c, reply[3]); err != nil {
		return nil, err
	}
	return c, nil
}

// socks5Auth performs method selection and, if the proxy asks for it,
// username/password authentication.
func (h Hop) socks5Auth(c net.Conn) error {
	methods := []byte{methodNoAuth}
	if h.Username != "" || h.Password != "" {
		methods = append(methods, methodUserPass)
	}
	greeting := make([]byte, 0, 2+len(methods))
	greeting = append(greeting, socks5Version, byte(len(methods)))
	greeting = append(greeting, methods...)
	if _, err := c.Write(greeting); err != nil {
		return fmt.Errorf("sending greeting: %w", err)
	}

	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil {
		return fmt.Errorf("reading method selection: %w", err)
	}
	if sel[0] != socks5Version {
		return fmt.Errorf("method selection has SOCKS version %d, not 5", sel[0])
	}
	switch sel[1] {
	case methodNoAuth:
		return nil
	case methodNoAcceptable:
		if h.Username == "" {
			return fmt.Errorf("the proxy requires authentication, but no username is configured")
		}
		return fmt.Errorf("the proxy accepts neither anonymous nor username/password authentication")
	case methodUserPass:
		return h.socks5UserPass(c)
	}
	return fmt.Errorf("the proxy chose authentication method 0x%02x, which reProxy does not support", sel[1])
}

// socks5UserPass performs the RFC 1929 exchange.
func (h Hop) socks5UserPass(c net.Conn) error {
	if len(h.Username) > 255 || len(h.Password) > 255 {
		return fmt.Errorf("the username and password must each be at most 255 bytes")
	}
	req := make([]byte, 0, 3+len(h.Username)+len(h.Password))
	req = append(req, userPassVersion, byte(len(h.Username)))
	req = append(req, h.Username...)
	req = append(req, byte(len(h.Password)))
	req = append(req, h.Password...)
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("sending credentials: %w", err)
	}

	var reply [2]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		return fmt.Errorf("reading authentication reply: %w", err)
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("the proxy rejected the username and password")
	}
	return nil
}

// discardSocks5Addr reads and throws away the address of the given type
// plus its two port bytes.
func discardSocks5Addr(c net.Conn, atyp byte) error {
	n := 0
	switch atyp {
	case atypIPv4:
		n = 4
	case atypIPv6:
		n = 16
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return fmt.Errorf("reading bound address: %w", err)
		}
		n = int(l[0])
	default:
		return fmt.Errorf("reply has address type %d, which reProxy does not understand", atyp)
	}
	if _, err := io.CopyN(io.Discard, c, int64(n)+2); err != nil {
		return fmt.Errorf("reading bound address: %w", err)
	}
	return nil
}

func socks5Status(code byte) string {
	switch code {
	case 0x01:
		return "general SOCKS server failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	}
	return fmt.Sprintf("status 0x%02x", code)
}

// splitHostPort splits a host:port target and checks the port.
func splitHostPort(target string) (string, int, error) {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, fmt.Errorf("target %q is not host:port: %w", target, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("target %q has no usable port", target)
	}
	return host, port, nil
}
