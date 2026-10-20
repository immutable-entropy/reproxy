package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// SOCKS4 and SOCKS4a. There is no RFC; the protocol is the original NEC
// memo, and 4a is its extension for passing a host name.
const (
	socks4Version = 0x04

	socks4Granted  = 0x5A
	socks4Rejected = 0x5B

	// maxSocks4Field bounds the USERID and host name, which are
	// NUL-terminated and so otherwise unbounded.
	maxSocks4Field = 255
)

// readSocks4Request reads a SOCKS4 or SOCKS4a CONNECT request and returns
// the target as host:port and the USERID it carried. A request this proxy
// won't serve (BIND, or a malformed address) is rejected before returning.
//
// SOCKS4 carries only an IPv4 address. SOCKS4a marks a name with the
// address 0.0.0.x (x non-zero) and sends the name after the USERID, so the
// proxy resolves it: that is what makes a lookup go through reproxy's
// resolver (and the upstream chain) rather than the browser's.
func readSocks4Request(br *bufio.Reader, w io.Writer) (target, user string, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return "", "", fmt.Errorf("reading request: %w", err)
	}
	if hdr[0] != socks4Version {
		return "", "", fmt.Errorf("%w: SOCKS version %d", errUnsupported, hdr[0])
	}
	port := binary.BigEndian.Uint16(hdr[2:4])
	ip := net.IPv4(hdr[4], hdr[5], hdr[6], hdr[7])

	if user, err = readNulField(br); err != nil {
		return "", "", fmt.Errorf("reading USERID: %w", err)
	}
	host := ip.String()
	if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 {
		if host, err = readNulField(br); err != nil {
			return "", "", fmt.Errorf("reading SOCKS4a host: %w", err)
		}
		if host == "" {
			_ = writeSocks4Reply(w, socks4Rejected)
			return "", "", fmt.Errorf("%w: empty SOCKS4a host", errUnsupported)
		}
	} else if ip.IsUnspecified() {
		_ = writeSocks4Reply(w, socks4Rejected)
		return "", "", fmt.Errorf("%w: SOCKS4 address 0.0.0.0", errUnsupported)
	}

	if hdr[1] != cmdConnect {
		_ = writeSocks4Reply(w, socks4Rejected)
		return "", "", fmt.Errorf("%w: command %d (only CONNECT is supported)", errUnsupported, hdr[1])
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), user, nil
}

// readNulField reads a NUL-terminated string of at most maxSocks4Field
// bytes, not counting the NUL.
func readNulField(br *bufio.Reader) (string, error) {
	var b []byte
	for {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if c == 0 {
			return string(b), nil
		}
		if len(b) == maxSocks4Field {
			return "", errors.New("field too long")
		}
		b = append(b, c)
	}
}

// writeSocks4Reply answers a SOCKS4 request. Like the SOCKS5 reply, the
// bound address is left unspecified: clients ignore it for CONNECT.
func writeSocks4Reply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{0x00, code, 0, 0, 0, 0, 0, 0})
	return err
}
