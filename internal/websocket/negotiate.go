package websocket

import (
	"bytes"
	"net/http"
	"strings"
)

// The handshake. A WebSocket starts as an HTTP request with Upgrade:
// websocket, answered with 101 Switching Protocols. permessage-deflate is
// negotiated through Sec-WebSocket-Extensions: the client offers it in the
// request, the server accepts it in the 101.
//
// When the proxy means to let a plugin edit the traffic it strips the
// offer from the request (StripExtensions), so the server can't turn
// compression on and every message flows as plain text or binary — nothing
// to re-compress after an edit. Otherwise the handshake is left alone and
// the proxy reads the 101 to learn whether, and how, messages are
// compressed (Accepted).

// IsRequest reports whether req is a WebSocket upgrade.
func IsRequest(req *http.Request) bool {
	return headerHasToken(req.Header, "Connection", "upgrade") &&
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

// IsResponse reports whether resp is a WebSocket upgrade's 101.
func IsResponse(resp *http.Response) bool {
	return resp.StatusCode == http.StatusSwitchingProtocols &&
		strings.EqualFold(resp.Header.Get("Upgrade"), "websocket")
}

// Accepted describes the compression a 101 response turned on.
type Accepted struct {
	Compressed bool
	// ServerContextTakeover and ClientContextTakeover say whether each side
	// shares its DEFLATE window across messages (the default; a
	// no_context_takeover parameter turns it off).
	ServerContextTakeover bool
	ClientContextTakeover bool
}

// Accept reads a 101 response's Sec-WebSocket-Extensions.
func Accept(resp *http.Response) Accepted {
	a := Accepted{ServerContextTakeover: true, ClientContextTakeover: true}
	for _, ext := range resp.Header.Values("Sec-WebSocket-Extensions") {
		for _, part := range strings.Split(ext, ",") {
			tokens := strings.Split(part, ";")
			if !strings.EqualFold(strings.TrimSpace(tokens[0]), "permessage-deflate") {
				continue
			}
			a.Compressed = true
			for _, p := range tokens[1:] {
				switch strings.TrimSpace(strings.ToLower(p)) {
				case "server_no_context_takeover":
					a.ServerContextTakeover = false
				case "client_no_context_takeover":
					a.ClientContextTakeover = false
				}
			}
		}
	}
	return a
}

// StripExtensions removes the Sec-WebSocket-Extensions header from a raw
// request head, so the server is offered no compression. The head keeps its
// CRLF framing. It is a no-op when the header isn't there.
func StripExtensions(head []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(head))
	for len(head) > 0 {
		line := head
		if i := bytes.Index(head, []byte("\r\n")); i >= 0 {
			line, head = head[:i+2], head[i+2:]
		} else {
			head = nil
		}
		name := line
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			name = line[:i]
		}
		if bytes.EqualFold(bytes.TrimSpace(name), []byte("Sec-WebSocket-Extensions")) {
			continue
		}
		out.Write(line)
	}
	return out.Bytes()
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
