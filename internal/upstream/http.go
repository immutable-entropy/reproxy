package upstream

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// maxConnectResponse caps the CONNECT reply read from a proxy, and
// maxConnectHeaders the number of header lines in it, so a hostile or
// broken proxy can't make the handshake read without end.
const (
	maxConnectResponse = 64 << 10
	maxConnectHeaders  = 100
)

// httpConnect opens a tunnel to target with an HTTP CONNECT request
// (RFC 9110 §9.3.6). It is used for both the HTTP and HTTPS kinds; for
// HTTPS, c is already the TLS connection to the proxy.
func (h Hop) httpConnect(c net.Conn, target string) (net.Conn, error) {
	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if h.Username != "" || h.Password != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(h.Username + ":" + h.Password))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	// Older proxies need the hop-by-hop spelling to keep the tunnel open.
	req.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := io.WriteString(c, req.String()); err != nil {
		return nil, fmt.Errorf("sending CONNECT: %w", err)
	}

	// The reply is read by hand rather than with http.ReadResponse: a
	// CONNECT reply has no body, but a 200 without Content-Length looks to
	// net/http like a body that runs to end of connection, and closing it
	// would swallow the tunnel.
	br := bufio.NewReader(io.LimitReader(c, maxConnectResponse))
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading CONNECT response: %w", err)
	}
	code, reason, err := parseStatusLine(status)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading CONNECT response headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		if i >= maxConnectHeaders {
			return nil, fmt.Errorf("CONNECT response has more than %d headers", maxConnectHeaders)
		}
	}
	switch {
	case code == 407:
		return nil, fmt.Errorf("CONNECT to %s needs proxy authentication (%d %s)", target, code, reason)
	case code < 200 || code > 299:
		return nil, fmt.Errorf("CONNECT to %s refused: %d %s", target, code, reason)
	}

	// Anything the reader took in past the blank line is the start of the
	// tunnel and has to be handed back with the connection.
	if n := br.Buffered(); n > 0 {
		pre := make([]byte, n)
		if _, err := io.ReadFull(br, pre); err != nil {
			return nil, fmt.Errorf("reading CONNECT response: %w", err)
		}
		return &prefixConn{Conn: c, pre: pre}, nil
	}
	return c, nil
}

// parseStatusLine splits "HTTP/1.1 200 Connection established" into its
// code and reason phrase.
func parseStatusLine(line string) (int, string, error) {
	trimmed := strings.TrimRight(line, "\r\n")
	proto, rest, ok := strings.Cut(trimmed, " ")
	if !ok || !strings.HasPrefix(proto, "HTTP/") {
		return 0, "", fmt.Errorf("not an HTTP response: %q", trimmed)
	}
	codeText, reason, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return 0, "", fmt.Errorf("bad status line %q", trimmed)
	}
	return code, reason, nil
}
