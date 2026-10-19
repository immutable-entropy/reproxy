package dns

import (
	"context"
	"errors"
	"net"
	"time"

	"reproxy/internal/capture"
)

// DialFunc opens a connection, as net.Dialer.DialContext does.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// minAttempt is the least time one address of several is given before the
// next is tried.
const minAttempt = 2 * time.Second

// Dialer wraps dial so that the host in each address is resolved here
// first (and so recorded, and interceptable). Who the lookup is for is read
// from ctx (capture.WithSource); without it, it is the proxy's.
func (r *Resolver) Dialer(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dial(ctx, network, addr)
		}
		var who Who
		who.Source, who.Origin, _ = capture.SourceFrom(ctx)
		ans, err := r.Resolve(ctx, host, who)
		if err != nil {
			return nil, err
		}
		return DialAnswer(ctx, network, ans, port, dial)
	}
}

// DialAnswer connects to port on each of the answer's hosts in turn until
// one accepts. When there are several, each but the last is given a share
// of the time left (at least minAttempt), so an address that doesn't
// answer can't use up the whole dial, as net.Dialer does.
func DialAnswer(ctx context.Context, network string, a Answer, port string, dial DialFunc) (net.Conn, error) {
	if len(a.Hosts) == 0 {
		return nil, errors.New("dns: no address to connect to")
	}
	var first error
	for i, h := range a.Hosts {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if left := len(a.Hosts) - i; left > 1 {
			if dl, ok := ctx.Deadline(); ok {
				share := max(time.Until(dl)/time.Duration(left), minAttempt)
				actx, cancel = context.WithTimeout(ctx, share)
			}
		}
		c, err := dial(actx, network, net.JoinHostPort(h, port))
		cancel()
		if err == nil {
			return c, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}
