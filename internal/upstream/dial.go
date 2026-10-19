package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

// handshakeTimeout bounds the whole traversal when the caller's context has
// no deadline of its own. Each hop's negotiation is a round trip, so a long
// chain over a slow link needs more than a single dial would.
const handshakeTimeout = 30 * time.Second

// expired is a deadline far in the past: setting it unblocks any read or
// write in flight, which is how a cancelled context interrupts a handshake.
var expired = time.Unix(1, 0)

// Hop is one proxy in a chain with its credentials resolved, ready to dial
// through. Build these with Chain.Hops.
type Hop struct {
	Kind     Kind
	Addr     string // host:port
	Username string
	Password string
	// VerifyTLS checks an HTTPS proxy's own certificate. Ignored by the
	// other protocols.
	VerifyTLS bool
}

// Dialer connects through a fixed chain of proxies. It is immutable and
// safe for concurrent use; to change the chain, build another one. A nil
// Dialer, and one with no hops, connects directly, so callers can use one
// unconditionally.
type Dialer struct {
	hops []Hop
}

// NewDialer returns a Dialer that traverses hops in order. It keeps a copy,
// so the caller's slice can be reused.
func NewDialer(hops []Hop) *Dialer {
	if len(hops) == 0 {
		return nil
	}
	return &Dialer{hops: append([]Hop(nil), hops...)}
}

// Hops returns the chain this Dialer traverses, for display. The result is
// a copy.
func (d *Dialer) Hops() []Hop {
	if d == nil {
		return nil
	}
	return append([]Hop(nil), d.hops...)
}

// Len returns how many proxies are in the chain; 0 means direct.
func (d *Dialer) Len() int {
	if d == nil {
		return 0
	}
	return len(d.hops)
}

// ResolvesRemotely reports whether a host name given to DialContext is
// resolved at the far end of the chain rather than on this machine: true
// when there is a chain and its last proxy can carry names. Plain SOCKS4
// can't, so a chain ending in one resolves here.
func (d *Dialer) ResolvesRemotely() bool {
	return d.Len() > 0 && d.hops[len(d.hops)-1].Kind != SOCKS4
}

// DialContext opens a TCP connection to addr through the chain. The first
// proxy is dialled directly, each one after it is reached through the one
// before, and addr is reached through the last — so every hop only ever
// sees the next one, not the target.
//
// ctx bounds the dial and every handshake; the connection handed back has
// no deadline set on it.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("upstream: %s is not supported through a proxy chain", network)
	}
	if d.Len() == 0 {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}

	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", d.hops[0].Addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s proxy %s: %w", d.hops[0].Kind, d.hops[0].Addr, err)
	}

	// Cancelling ctx expires the underlying socket, which unblocks whichever
	// handshake is in flight. stop runs before DialContext returns, so a
	// context cancelled immediately afterwards (as the proxy server does)
	// cannot reach the connection being handed back.
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(expired) })
	defer stop()

	deadline := time.Now().Add(handshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := raw.SetDeadline(deadline); err != nil {
		_ = raw.Close()
		return nil, err
	}

	conn := raw
	for i, h := range d.hops {
		// Each hop is told to connect to the next one; the last is told the
		// real target.
		target := addr
		if i+1 < len(d.hops) {
			target = d.hops[i+1].Addr
		}
		next, err := h.connect(ctx, conn, target)
		if err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("%s proxy %s (hop %d of %d): %w", h.Kind, h.Addr, i+1, len(d.hops), err)
		}
		conn = next
	}

	if err := raw.SetDeadline(time.Time{}); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// connect negotiates one hop and returns the connection to use from here
// on: the same one for the SOCKS protocols, a TLS or buffer-backed wrapper
// for HTTP/HTTPS.
func (h Hop) connect(ctx context.Context, c net.Conn, target string) (net.Conn, error) {
	switch h.Kind {
	case HTTP:
		return h.httpConnect(c, target)
	case HTTPS:
		tc, err := h.tlsClient(ctx, c)
		if err != nil {
			return nil, err
		}
		return h.httpConnect(tc, target)
	case SOCKS4, SOCKS4a:
		return h.socks4Connect(ctx, c, target)
	case SOCKS5:
		return h.socks5Connect(c, target)
	}
	return nil, fmt.Errorf("unknown proxy type %q", h.Kind)
}

// tlsClient wraps the connection to an HTTPS proxy in TLS. The server name
// is the proxy's own host; the target is not revealed until the CONNECT
// inside the tunnel.
func (h Hop) tlsClient(ctx context.Context, c net.Conn) (net.Conn, error) {
	host := h.Addr
	if hh, _, err := net.SplitHostPort(h.Addr); err == nil {
		host = hh
	}
	tc := tls.Client(c, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: !h.VerifyTLS, //nolint:gosec // the user's choice, for self-signed proxies
		MinVersion:         tls.VersionTLS12,
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tc, nil
}

// prefixConn replays bytes a handshake read past the end of its reply
// before reading any more from the connection itself.
type prefixConn struct {
	net.Conn
	pre []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.pre) > 0 {
		n := copy(p, c.pre)
		c.pre = c.pre[n:]
		if len(c.pre) == 0 {
			c.pre = nil
		}
		return n, nil
	}
	return c.Conn.Read(p)
}
