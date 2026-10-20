package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"reproxy/internal/capture"
)

// handleHTTP serves a client that speaks HTTP to the proxy. A CONNECT opens
// a tunnel exactly as a SOCKS request does, so HTTPS is decrypted (with a
// CA) and recorded the same way. Any other method is a plain request naming
// its URL in full (absolute form), which is forwarded (see forward).
//
// A Basic Proxy-Authorization on the first request is handed to the origin
// resolver, as SOCKS5 credentials are. Like those, it isn't checked: the
// proxy only listens on loopback.
func (s *Server) handleHTTP(log *slog.Logger, client net.Conn, br *bufio.Reader) {
	head, err := readHead(br)
	if err != nil {
		log.Debug("reading proxy request", "err", err)
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		_ = writeHTTPError(client, http.StatusBadRequest, "reProxy could not read the request: "+err.Error())
		return
	}
	source, origin := s.originOf(proxyCredentials(req.Header))

	if req.Method != http.MethodConnect {
		s.forward(log, client, br, head, source, origin)
		return
	}
	target := req.RequestURI
	if _, _, err := net.SplitHostPort(target); err != nil {
		_ = writeHTTPError(client, http.StatusBadRequest, "CONNECT needs a host and port, not "+target)
		return
	}
	s.connect(log, client, br, target, source, origin, func(err error) error {
		if err != nil {
			return writeHTTPError(client, statusForDialError(err), "reProxy could not connect to "+target+": "+err.Error())
		}
		_, err = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		return err
	})
}

// handleHTTPS serves an HTTPS proxy: the client opens TLS to the proxy
// itself, then speaks HTTP proxy inside it. The proxy's certificate is
// minted by the interception CA for the name the client asked for (or the
// address it connected to), so a browser that trusts the CA trusts the
// proxy; without a CA there is no certificate to offer, and the connection
// is closed.
func (s *Server) handleHTTPS(log *slog.Logger, client net.Conn, br *bufio.Reader) {
	if s.ca == nil {
		log.Debug("HTTPS proxy connection refused: no CA to sign the proxy's certificate")
		return
	}
	local := client.LocalAddr().String()
	conn := tls.Server(bufferedConn{r: br, Conn: client}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.ca.LeafFor(certNameFor(hi.ServerName, local))
		},
	})
	if err := conn.Handshake(); err != nil {
		log.Debug("HTTPS proxy handshake failed", "err", err)
		return
	}
	s.handleHTTP(log, conn, bufio.NewReaderSize(conn, parseBufSize))
}

// forward serves an HTTP proxy's plain requests, starting with head.
//
// Each request names its origin, and one client connection may carry
// requests for many. They are relayed one origin at a time: a tunnel to the
// first request's origin carries every request after it until one names
// another, and once the responses still due are relayed, the next tunnel is
// opened for that one. Each request is rewritten for the origin server
// (origin-form request line, Host set from the URL, Proxy-* headers taken
// out), and recorded and held as it was sent. An https:// URL is sent over
// TLS, unverified as mitm's upstream side is.
func (s *Server) forward(log *slog.Logger, client net.Conn, br *bufio.Reader, head []byte, source capture.Source, origin string) {
	for head != nil {
		a, err := absoluteTargetOf(head)
		if err != nil {
			_ = writeHTTPError(client, http.StatusBadRequest, err.Error())
			return
		}
		upstream, err := s.open(client, a.addr, source, origin)
		if err != nil {
			log.Debug("connect failed", "target", a.addr, "err", err)
			_ = writeHTTPError(client, statusForDialError(err), "reProxy could not connect to "+a.addr+": "+err.Error())
			return
		}
		conn := upstream
		if a.secure {
			if conn, err = tlsTo(upstream, a.addr); err != nil {
				s.untrack(upstream)
				log.Debug("upstream TLS handshake failed", "target", a.addr, "err", err)
				_ = writeHTTPError(client, http.StatusBadGateway, "reProxy could not open TLS to "+a.addr+": "+err.Error())
				return
			}
		}
		_ = client.SetDeadline(time.Time{})

		f := &forwardState{key: a.key(), first: head}
		s.relayForward(client, br, conn, a.addr, a.secure, f, source, origin)
		s.untrack(upstream)
		if !f.resume {
			return
		}
		head = f.next
	}
}

// tlsTo opens TLS over conn to addr's host, within the dial timeout.
func tlsTo(conn net.Conn, addr string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	tc := tls.Client(conn, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, // as mitm's upstream side: staging certs are not verified
		MinVersion:         tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return tc, nil
}

// forwardState is the part of a tunnel that serves an HTTP proxy's plain
// requests. Its request loop starts with first, rewrites each request for
// the origin server, and stops at one for another origin, leaving it in
// next; the response loop sets resume once every response before it is
// relayed, which is when the client connection can go on to that origin.
type forwardState struct {
	key    string // the origin this tunnel's upstream serves (absTarget.key)
	first  []byte
	next   []byte
	resume bool
}

// rewrite readies a request head for the origin server and reports whether
// it is for this tunnel's origin. A request in origin form (no scheme and
// host) is taken to be: it can only mean the connection's current origin.
func (f *forwardState) rewrite(head []byte) ([]byte, bool) {
	target, ok := requestTarget(head)
	if !ok {
		return head, false
	}
	if strings.HasPrefix(target, "/") {
		return rewriteHead(head, absTarget{}), true
	}
	a, err := parseAbsolute(target)
	if err != nil || a.key() != f.key {
		return head, false
	}
	return rewriteHead(head, a), true
}

// absTarget is what an absolute-form request target names.
type absTarget struct {
	secure    bool
	authority string // host[:port] as the URL wrote it, for the Host header
	addr      string // host:port to dial
	path      string // the origin-form request target
}

// key identifies the origin: requests with the same key can share an
// upstream connection.
func (a absTarget) key() string {
	scheme := "http"
	if a.secure {
		scheme = "https"
	}
	return scheme + "://" + strings.ToLower(a.addr)
}

// absoluteTargetOf reads the absolute-form target of the request in head.
func absoluteTargetOf(head []byte) (absTarget, error) {
	target, ok := requestTarget(head)
	if !ok {
		return absTarget{}, errors.New("reProxy could not read the request line")
	}
	if strings.HasPrefix(target, "/") {
		return absTarget{}, errors.New("reProxy is a proxy, not a web server: set it as the proxy " +
			"(SOCKS5, SOCKS4, or HTTP for both http and https) rather than opening its address as a page")
	}
	return parseAbsolute(target)
}

// requestTarget returns the second field of head's request line.
func requestTarget(head []byte) (string, bool) {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	parts := strings.Split(strings.TrimRight(string(line), "\r"), " ")
	if len(parts) != 3 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// parseAbsolute splits an http:// or https:// URL into the origin to
// connect to and the path to ask it for. User information in the authority
// is dropped (RFC 9110 deprecates it, and it isn't the server's business).
func parseAbsolute(target string) (absTarget, error) {
	var a absTarget
	scheme, rest, ok := strings.Cut(target, "://")
	switch {
	case !ok:
		return a, fmt.Errorf("reProxy can't forward %q: not a URL", target)
	case strings.EqualFold(scheme, "http"):
	case strings.EqualFold(scheme, "https"):
		a.secure = true
	default:
		return a, fmt.Errorf("reProxy can't forward %s: URLs", scheme)
	}
	i := strings.IndexAny(rest, "/?#")
	if i < 0 {
		i = len(rest)
	}
	a.authority, a.path = rest[:i], rest[i:]
	if at := strings.LastIndexByte(a.authority, '@'); at >= 0 {
		a.authority = a.authority[at+1:]
	}
	if frag := strings.IndexByte(a.path, '#'); frag >= 0 {
		a.path = a.path[:frag]
	}
	if !strings.HasPrefix(a.path, "/") {
		a.path = "/" + a.path
	}
	if a.authority == "" {
		return a, fmt.Errorf("reProxy can't forward %q: no host", target)
	}
	if host, port, err := net.SplitHostPort(a.authority); err == nil {
		if host == "" || port == "" {
			return a, fmt.Errorf("reProxy can't forward %q: bad host", target)
		}
		a.addr = a.authority
	} else {
		port := "80"
		if a.secure {
			port = "443"
		}
		a.addr = net.JoinHostPort(strings.Trim(a.authority, "[]"), port)
	}
	return a, nil
}

// rewriteHead turns a proxy request into the one its origin server is sent:
// the request line in origin form, Host naming the URL's authority, and the
// headers meant for the proxy (Proxy-Connection, Proxy-Authorization)
// removed. Everything else is kept byte for byte. A zero absTarget leaves
// the request line and Host alone.
func rewriteHead(head []byte, a absTarget) []byte {
	out := make([]byte, 0, len(head)+len(a.authority)+8)
	hostSeen := a.authority == ""
	for i, rest := 0, head; len(rest) > 0; i++ {
		var l []byte
		if n := bytes.IndexByte(rest, '\n'); n >= 0 {
			l, rest = rest[:n+1], rest[n+1:]
		} else {
			l, rest = rest, nil
		}
		content := bytes.TrimRight(l, "\r\n")
		eol := l[len(content):]
		switch name, value, _ := bytes.Cut(content, []byte(":")); {
		case i == 0:
			if a.path != "" {
				method, after, _ := bytes.Cut(content, []byte(" "))
				_, version, _ := bytes.Cut(after, []byte(" "))
				out = append(append(append(append(out, method...), ' '), a.path...), ' ')
				out = append(append(out, version...), eol...)
				continue
			}
		case len(content) == 0: // the blank line that ends the head
			if !hostSeen {
				out = append(append(append(out, "Host: "...), a.authority...), eol...)
			}
		case bytes.EqualFold(bytes.TrimSpace(name), []byte("Proxy-Connection")),
			bytes.EqualFold(bytes.TrimSpace(name), []byte("Proxy-Authorization")):
			continue
		case a.authority != "" && bytes.EqualFold(bytes.TrimSpace(name), []byte("Host")):
			hostSeen = true
			if !strings.EqualFold(string(bytes.TrimSpace(value)), a.authority) {
				out = append(append(append(out, "Host: "...), a.authority...), eol...)
				continue
			}
		}
		out = append(out, l...)
	}
	return out
}

// proxyCredentials reads a Basic Proxy-Authorization.
func proxyCredentials(h http.Header) (user, pass string) {
	scheme, cred, ok := strings.Cut(h.Get("Proxy-Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", ""
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return "", ""
	}
	user, pass, _ = strings.Cut(string(dec), ":")
	return user, pass
}

// statusForDialError picks the status an HTTP proxy reports for a failed
// connection: 504 when it timed out, 502 otherwise.
func statusForDialError(err error) int {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// writeHTTPError answers a proxy request the proxy couldn't carry out, and
// asks the client to close the connection.
func writeHTTPError(w io.Writer, code int, msg string) error {
	body := msg + "\n"
	_, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
	return err
}
