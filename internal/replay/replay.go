// Package replay sends a raw HTTP/1.x request straight to a server and reads
// the response, for the Replay tool (like Burp's Repeater). It connects to
// the target itself rather than through the proxy, so a replay is never held
// by interception; Sessions.RecordTo puts each send in the capture store
// instead, marked as a replay, so it still shows with the rest of the
// traffic.
//
// It is core (intercept/edit/replay; see CLAUDE.md) and has no Qt dependency.
package replay

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Request is a single message to send.
type Request struct {
	// Addr is the server to connect to, host:port.
	Addr string
	// Secure sends over TLS. The server certificate is not verified, matching
	// the proxy's HTTPS interception (staging certs are often self-signed).
	Secure bool
	// Raw is the complete request message (start line, headers, blank line,
	// body) with CRLF line endings.
	Raw []byte
	// Dial opens the TCP connection to Addr; nil connects directly. It is
	// how a replay goes out through the configured upstream proxy chain,
	// the same way intercepted traffic does.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Limit caps the response in bytes; a larger one fails the send rather
	// than being held in memory. 0 means no cap.
	Limit int64
}

// ErrTooLarge is returned when a response passes Request.Limit.
var ErrTooLarge = errors.New("response larger than the limit")

// Response is what came back.
type Response struct {
	// Raw is the response exactly as received.
	Raw []byte
	// Size is len(Raw).
	Size int
	// StatusCode and Status are parsed from the status line.
	StatusCode int
	Status     string
	// Duration is the time from sending the request to the body completing.
	Duration time.Duration
}

// defaultTimeout bounds a replay that would otherwise hang on a server that
// keeps the connection open without replying.
const defaultTimeout = 30 * time.Second

// Send performs one request/response exchange. ctx bounds the whole call.
func Send(ctx context.Context, r Request) (Response, error) {
	if len(r.Raw) == 0 {
		return Response{}, fmt.Errorf("empty request")
	}

	deadline := time.Now().Add(defaultTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}

	conn, err := dial(ctx, r)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	// Cancelling ctx abandons the exchange at once, rather than at the
	// deadline: Autocrawl's Stop relies on it.
	defer context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })()

	start := time.Now()
	if _, err := conn.Write(r.Raw); err != nil {
		return Response{}, fmt.Errorf("sending request: %w", err)
	}

	// Record every byte read so the raw response can be shown verbatim; the
	// bytes bufio read ahead past the response aren't part of it.
	var src io.Reader = conn
	if r.Limit > 0 {
		src = &limitReader{r: conn, n: r.Limit}
	}
	rec := &captureReader{r: src}
	br := bufio.NewReader(rec)
	resp, err := http.ReadResponse(br, &http.Request{Method: methodOf(r.Raw)})
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		resp.Body.Close()
		if errors.Is(err, ErrTooLarge) {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("reading response body: %w", err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)

	raw := rec.consumed(br.Buffered())
	return Response{
		Raw:        raw,
		Size:       len(raw),
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Duration:   elapsed,
	}, nil
}

func dial(ctx context.Context, r Request) (net.Conn, error) {
	dialTCP := r.Dial
	if dialTCP == nil {
		dialTCP = (&net.Dialer{}).DialContext
	}
	conn, err := dialTCP(ctx, "tcp", r.Addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", r.Addr, err)
	}
	if !r.Secure {
		return conn, nil
	}
	host := r.Addr
	if h, _, err := net.SplitHostPort(r.Addr); err == nil {
		host = h
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS handshake with %s: %w", r.Addr, err)
	}
	return tlsConn, nil
}

// methodOf returns the request method, for response framing (HEAD has no
// body).
func methodOf(raw []byte) string {
	line := string(raw)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i]
	}
	return "GET"
}

// limitReader fails with ErrTooLarge once more than n bytes have been read.
type limitReader struct {
	r io.Reader
	n int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// captureReader records everything read through it.
type captureReader struct {
	r   io.Reader
	buf []byte
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.buf = append(c.buf, p[:n]...)
	}
	return n, err
}

// consumed returns the recorded bytes that belong to the response, dropping
// the buffered trailing bytes bufio read past its end.
func (c *captureReader) consumed(buffered int) []byte {
	end := len(c.buf) - buffered
	if end < 0 {
		end = 0
	}
	out := make([]byte, end)
	copy(out, c.buf[:end])
	return out
}
