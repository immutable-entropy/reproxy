// Package proxy implements the proxy that browsers connect through. One port
// speaks SOCKS5, SOCKS4, SOCKS4a, HTTP (CONNECT, and plain requests with an
// absolute URL) and HTTPS (the same over TLS to the proxy), told apart by
// the first byte the client sends.
//
// Every CONNECT is tunnelled to its target. Plain HTTP/1.x is recorded into
// a capture.Store, and the intercept.Interceptor may hold messages for the
// user to edit, forward or drop. When a CA is configured, TLS connections are
// intercepted the same way (see mitm); without one they are passed through
// untouched. HTTP/2 and WebSocket frames after an upgrade are always passed
// through without being recorded.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/tlsca"
	"reproxy/internal/websocket"
)

// DefaultCaptureLimit is how many bytes of each request or response are kept.
// Larger messages are still forwarded in full; only the capture is cut.
const DefaultCaptureLimit = 16 << 20

const (
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second
)

// ScopeFunc decides whether a request is in scope. It is called once per
// request, on the hot path, and returns the ID of the target the request
// belongs to (empty when no targets are in use) and whether to record it at
// all.
//
// A request that is not collected is relayed byte-for-byte and never reaches
// the store or the interceptor, so traffic that belongs to none of the
// user's targets leaves no trace. It is defined as a plain function so the
// proxy doesn't depend on the target package.
type ScopeFunc func(host string, ip netip.Addr, path string) (targetID string, collect bool)

// DialFunc opens a connection to addr. The proxy uses it for every upstream
// connection, so a chain of upstream proxies can be put in front of the
// target without the relay knowing (see internal/upstream).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ResolveFunc turns the host a client asked for into the hosts to dial, in
// order: the name itself when the upstream chain resolves names, or the
// addresses it resolved to. It is called once per connection, before the
// dial, and may block while interception holds the lookup (bounded by ctx).
// It is how DNS is recorded and intercepted (see package dns) without the
// proxy depending on it.
type ResolveFunc func(ctx context.Context, host, clientAddr string, source capture.Source, origin string) ([]string, error)

// WSHandler lets a plugin layer watch and edit WebSocket messages. The proxy
// records every message itself; the handler is asked (1) whether a
// connection should be editable, so the proxy strips its compression at the
// handshake, and (2) for a decision on each message of an editable
// connection, before it is forwarded. It is Qt-free and nil when no plugin
// wants WebSocket traffic.
type WSHandler interface {
	// Edits reports whether messages on the connection the handshake opens
	// should be held for plugins to edit or drop.
	Edits(s intercept.Subject) bool
	// Decide returns what to do with one message of an editable connection.
	// It may block (a plugin deciding), bounded by ctx. The zero Decision
	// forwards the message unchanged.
	Decide(ctx context.Context, m WSMessageInfo) websocket.Decision
	// Opened is called when a WebSocket connection's 101 completes, Closed
	// when it ends.
	Opened(flowID uint64, s intercept.Subject)
	Closed(flowID uint64)
}

// WSMessageInfo is one WebSocket message handed to a WSHandler.
type WSMessageInfo struct {
	FlowID uint64
	Dir    websocket.Direction
	Type   websocket.Type
	Data   []byte
}

// Server is the proxy. The zero value is not usable; call New.
type Server struct {
	store        *capture.Store
	intercept    *intercept.Interceptor
	ca           *tlsca.CA
	log          *slog.Logger
	captureLimit int
	scope        ScopeFunc
	origin       OriginFunc
	resolve      ResolveFunc
	ws           WSHandler

	// dial is swapped when the upstream chain changes. It is read once per
	// connection, never per message, so the atomic costs nothing that
	// matters.
	dial atomic.Pointer[DialFunc]

	// ctx ends when the server is closed, releasing any held messages.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Option configures a Server.
type Option func(*Server)

// WithCaptureLimit overrides DefaultCaptureLimit.
func WithCaptureLimit(n int) Option {
	return func(s *Server) { s.captureLimit = n }
}

// WithInterceptor sets the interceptor consulted for every message. Without
// one, nothing is ever held.
func WithInterceptor(i *intercept.Interceptor) Option {
	return func(s *Server) { s.intercept = i }
}

// WithCA enables HTTPS interception: TLS connections are decrypted using
// leaf certificates signed by ca, recorded, and re-encrypted upstream.
// Without a CA, TLS is passed through untouched.
func WithCA(ca *tlsca.CA) Option {
	return func(s *Server) { s.ca = ca }
}

// WithLogger sets the logger used for connection-level diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.log = l }
}

// WithDialer sets how upstream connections are made. Without one, the proxy
// dials targets directly.
func WithDialer(d DialFunc) Option {
	return func(s *Server) { s.SetDialer(d) }
}

// SetWebSocket installs the WebSocket handler. It is set once, before the
// proxy starts listening, so it needs no synchronisation.
func (s *Server) SetWebSocket(h WSHandler) { s.ws = h }

// SetDialer replaces the dialer while the proxy is running, for a settings
// change. Connections already open are unaffected.
func (s *Server) SetDialer(d DialFunc) {
	if d == nil {
		s.dial.Store(nil)
		return
	}
	s.dial.Store(&d)
}

// dialUpstream connects to target, through the configured dialer when there
// is one.
func (s *Server) dialUpstream(ctx context.Context, target string) (net.Conn, error) {
	if d := s.dial.Load(); d != nil {
		return (*d)(ctx, "tcp", target)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", target)
}

// resolveTarget resolves the host in target, when a resolver is set. The
// client's handshake deadline is lifted while it runs: the lookup may be
// held for the user, and the client is only waiting on the reply.
func (s *Server) resolveTarget(client net.Conn, target string, source capture.Source, origin string) (hosts []string, port string, err error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || s.resolve == nil {
		return []string{target}, "", nil
	}
	_ = client.SetDeadline(time.Time{})
	hosts, err = s.resolve(s.ctx, host, client.RemoteAddr().String(), source, origin)
	_ = client.SetDeadline(time.Now().Add(handshakeTimeout))
	return hosts, port, err
}

// dialHosts connects to the first of hosts that accepts, within
// dialTimeout. port is "" when hosts already carry one. Each but the last
// gets a share of the time left, so one that doesn't answer can't use it
// all.
func (s *Server) dialHosts(hosts []string, port string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	var first error
	for i, h := range hosts {
		addr := h
		if port != "" {
			addr = net.JoinHostPort(h, port)
		}
		actx, acancel := ctx, context.CancelFunc(func() {})
		if left := len(hosts) - i; left > 1 {
			dl, _ := ctx.Deadline()
			actx, acancel = context.WithTimeout(ctx, max(time.Until(dl)/time.Duration(left), 2*time.Second))
		}
		c, err := s.dialUpstream(actx, addr)
		acancel()
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
	if first == nil {
		first = errors.New("no address to connect to")
	}
	return nil, first
}

// OriginFunc recognises a connection opened by reproxy itself — a plugin
// sending traffic through the proxy so that it is recorded — from the
// username and password it authenticated with (SOCKS5's, or an HTTP
// request's Proxy-Authorization). ok is false for anyone else, whose traffic
// is the browser's.
type OriginFunc func(user, pass string) (source capture.Source, origin string, ok bool)

// WithOrigin sets the origin resolver. Without one, SOCKS5 offers only "no
// authentication", credentials are ignored, and every flow is the proxy's.
func WithOrigin(fn OriginFunc) Option {
	return func(s *Server) { s.origin = fn }
}

// WithResolver sets how the hosts clients ask for are resolved. Without
// one, the dialer is handed the host as it is.
func WithResolver(fn ResolveFunc) Option {
	return func(s *Server) { s.resolve = fn }
}

// WithWebSocket installs the handler that watches and edits WebSocket
// messages. Without one, WebSocket traffic is recorded and relayed but not
// offered to plugins.
func WithWebSocket(h WSHandler) Option {
	return func(s *Server) { s.ws = h }
}

// WithScope limits what is recorded (see ScopeFunc). Without one, everything
// is recorded and no flow carries a target.
func WithScope(fn ScopeFunc) Option {
	return func(s *Server) { s.scope = fn }
}

// inScope asks the scope function about a request. With no scope function
// configured, everything is in scope.
func (s *Server) inScope(host string, ip netip.Addr, path string) (string, bool) {
	if s.scope == nil {
		return "", true
	}
	return s.scope(host, ip, path)
}

// New returns a proxy that records into store. It does not listen until
// Listen is called.
func New(store *capture.Store, opts ...Option) *Server {
	s := &Server{
		store:        store,
		log:          slog.Default(),
		captureLimit: DefaultCaptureLimit,
		conns:        make(map[net.Conn]struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	if s.intercept == nil {
		s.intercept, _ = intercept.New(intercept.DefaultSettings()) // disabled
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// ErrServerClosed is returned by Listen after Close has been called.
var ErrServerClosed = errors.New("proxy: server closed")

// Listen starts accepting connections on addr (e.g. "127.0.0.1:7193").
//
// If the server is already listening, the new address is bound first and
// the old listener is only closed once that succeeds, so a failed port
// change leaves the proxy running where it was. Connections that are already
// open are not interrupted.
func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return ErrServerClosed
	}
	old := s.ln
	s.ln = ln
	s.wg.Add(1)
	s.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	go s.serve(ln)
	s.log.Info("proxy listening", "addr", ln.Addr().String())
	return nil
}

// Addr returns the address being listened on, or "" if not listening.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops listening, closes every open connection and waits for their
// goroutines to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.closed = true
	s.cancel()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) serve(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("accept failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !s.track(conn) {
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handle(conn)
		}()
	}
}

// track registers c so Close can interrupt it. It reports false once the
// server has been closed.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	_ = c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// handle serves one client connection. The protocol is told from its first
// byte, so one port serves all of them: 0x05 is SOCKS5, 0x04 is SOCKS4 or
// SOCKS4a, a letter is the start of an HTTP proxy request (CONNECT for a
// tunnel, or a plain request with an absolute URL), and a TLS handshake is
// an HTTPS proxy: HTTP proxy requests over TLS to the proxy itself.
func (s *Server) handle(client net.Conn) {
	log := s.log.With("client", client.RemoteAddr().String())

	_ = client.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReaderSize(client, parseBufSize)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	switch {
	case first[0] == socksVersion:
		s.handleSocks5(log, client, br)
	case first[0] == socks4Version:
		s.handleSocks4(log, client, br)
	case looksLikeHTTP(br):
		s.handleHTTP(log, client, br)
	case isClientHello(br):
		s.handleHTTPS(log, client, br)
	default:
		log.Debug("unrecognised proxy protocol", "first", first[0])
	}
}

func (s *Server) handleSocks5(log *slog.Logger, client net.Conn, br *bufio.Reader) {
	rw := struct {
		io.Reader
		io.Writer
	}{br, client}
	user, pass, err := negotiate(rw, s.origin != nil)
	if err != nil {
		log.Debug("socks negotiation failed", "err", err)
		return
	}
	source, origin := s.originOf(user, pass)
	target, err := readRequest(rw)
	if err != nil {
		log.Debug("socks request failed", "err", err)
		return
	}
	s.connect(log, client, br, target, source, origin, func(err error) error {
		if err != nil {
			return writeReply(client, replyForDialError(err))
		}
		return writeReply(client, repSucceeded)
	})
}

// handleSocks4 serves SOCKS4 and SOCKS4a. Its USERID is handed to the
// origin resolver as a username with no password, which no plugin label
// matches, so these connections are always the browser's.
func (s *Server) handleSocks4(log *slog.Logger, client net.Conn, br *bufio.Reader) {
	target, user, err := readSocks4Request(br, client)
	if err != nil {
		log.Debug("socks4 request failed", "err", err)
		return
	}
	source, origin := s.originOf(user, "")
	s.connect(log, client, br, target, source, origin, func(err error) error {
		if err != nil {
			return writeSocks4Reply(client, socks4Rejected)
		}
		return writeSocks4Reply(client, socks4Granted)
	})
}

// originOf labels a connection from the credentials it presented: a
// plugin's, when the origin resolver recognises them, or the proxy's.
func (s *Server) originOf(user, pass string) (capture.Source, string) {
	if s.origin == nil || user == "" && pass == "" {
		return "", ""
	}
	if source, origin, ok := s.origin(user, pass); ok {
		return source, origin
	}
	return capture.SourceProxy, ""
}

// connect resolves and dials target, tells the client how that went through
// reply (nil for success), and relays the connection until it ends.
func (s *Server) connect(log *slog.Logger, client net.Conn, br *bufio.Reader, target string, source capture.Source, origin string, reply func(error) error) {
	log = log.With("target", target)
	upstream, err := s.open(client, target, source, origin)
	if err != nil {
		log.Debug("connect failed", "err", err)
		_ = reply(err)
		return
	}
	defer s.untrack(upstream)

	if err := reply(nil); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})

	log.Debug("tunnel open")
	s.relay(s.ctx, client, br, upstream, target, source, origin)
	log.Debug("tunnel closed")
}

// open resolves and dials target on the client's behalf. The connection it
// returns is tracked; the caller untracks (and so closes) it.
func (s *Server) open(client net.Conn, target string, source capture.Source, origin string) (net.Conn, error) {
	hosts, port, err := s.resolveTarget(client, target, source, origin)
	if err != nil {
		return nil, err
	}
	upstream, err := s.dialHosts(hosts, port)
	if err != nil {
		return nil, err
	}
	if !s.track(upstream) {
		_ = upstream.Close()
		return nil, ErrServerClosed
	}
	return upstream, nil
}
