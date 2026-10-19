package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
)

// SOCKS5 protocol constants (RFC 1928).
const (
	socksVersion = 0x05

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF

	userPassVersion = 0x01

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSucceeded           = 0x00
	repGeneralFailure      = 0x01
	repNetworkUnreachable  = 0x03
	repHostUnreachable     = 0x04
	repConnectionRefused   = 0x05
	repCommandNotSupported = 0x07
	repAddrNotSupported    = 0x08
)

// errUnsupported marks handshake failures caused by the client asking for
// something this proxy deliberately doesn't do (auth, BIND, UDP).
var errUnsupported = errors.New("unsupported")

// negotiate performs method selection. "No authentication" is what browsers
// use: the proxy only listens on loopback. When an origin resolver is set,
// username/password (RFC 1929) is preferred if the client offers it — not to
// keep anyone out, but because it is how reproxy's own plugins label the
// connections they open through the proxy (see OriginFunc). The credentials
// are returned for the resolver. Any credentials are accepted: ones the
// resolver doesn't recognise just make the connection an ordinary one.
func negotiate(rw io.ReadWriter, userPass bool) (user, pass string, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return "", "", fmt.Errorf("reading greeting: %w", err)
	}
	if hdr[0] != socksVersion {
		return "", "", fmt.Errorf("%w: SOCKS version %d (is the browser set to SOCKS4 or HTTP proxy?)", errUnsupported, hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(rw, methods); err != nil {
		return "", "", fmt.Errorf("reading auth methods: %w", err)
	}
	if userPass && bytes.IndexByte(methods, methodUserPass) >= 0 {
		if _, err := rw.Write([]byte{socksVersion, methodUserPass}); err != nil {
			return "", "", err
		}
		return readUserPass(rw)
	}
	if bytes.IndexByte(methods, methodNoAuth) >= 0 {
		_, err := rw.Write([]byte{socksVersion, methodNoAuth})
		return "", "", err
	}
	_, _ = rw.Write([]byte{socksVersion, methodNoAcceptable})
	return "", "", fmt.Errorf("%w: client requires authentication", errUnsupported)
}

// readUserPass reads the RFC 1929 sub-negotiation and accepts it: whether
// the credentials mean anything is the origin resolver's business.
func readUserPass(rw io.ReadWriter) (user, pass string, err error) {
	var ver [1]byte
	if _, err := io.ReadFull(rw, ver[:]); err != nil {
		return "", "", err
	}
	if ver[0] != userPassVersion {
		return "", "", fmt.Errorf("%w: username/password version %d", errUnsupported, ver[0])
	}
	field := func() (string, error) {
		var n [1]byte
		if _, err := io.ReadFull(rw, n[:]); err != nil {
			return "", err
		}
		b := make([]byte, n[0])
		_, err := io.ReadFull(rw, b)
		return string(b), err
	}
	if user, err = field(); err != nil {
		return "", "", err
	}
	if pass, err = field(); err != nil {
		return "", "", err
	}
	_, err = rw.Write([]byte{userPassVersion, 0x00})
	return user, pass, err
}

// readRequest reads a SOCKS5 request and returns the target as host:port.
// Unsupported commands and address types are answered before returning.
func readRequest(rw io.ReadWriter) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return "", fmt.Errorf("reading request: %w", err)
	}
	if hdr[0] != socksVersion {
		return "", fmt.Errorf("%w: SOCKS version %d in request", errUnsupported, hdr[0])
	}

	var host string
	switch hdr[3] {
	case atypIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(rw, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case atypIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(rw, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(rw, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(rw, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		_ = writeReply(rw, repAddrNotSupported)
		return "", fmt.Errorf("%w: address type %d", errUnsupported, hdr[3])
	}

	var port [2]byte
	if _, err := io.ReadFull(rw, port[:]); err != nil {
		return "", err
	}

	if hdr[1] != cmdConnect {
		_ = writeReply(rw, repCommandNotSupported)
		return "", fmt.Errorf("%w: command %d (only CONNECT is supported)", errUnsupported, hdr[1])
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// writeReply sends a reply with an unspecified bound address. Browsers ignore
// BND.ADDR for CONNECT, and reporting the real upstream-facing address would
// only leak detail.
func writeReply(w io.Writer, rep byte) error {
	_, err := w.Write([]byte{socksVersion, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// replyForDialError maps a dial failure to the closest SOCKS reply code so
// the browser can show a meaningful error page.
func replyForDialError(err error) byte {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return repConnectionRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return repNetworkUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return repHostUnreachable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return repHostUnreachable
	}
	return repGeneralFailure
}
