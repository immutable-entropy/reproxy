// Package capture holds the HTTP traffic recorded by the proxy.
//
// It has no dependency on the proxy or the UI: the proxy writes flows into a
// Store and the UI subscribes to it. Keeping it free of Qt means it can be
// unit-tested (and reused by future features such as export or replay)
// without a GUI.
package capture

import "time"

// FlowState describes how far a request/response exchange has progressed.
type FlowState int

const (
	// StatePending means the request has been seen but no response yet.
	StatePending FlowState = iota
	// StateComplete means a full response was recorded.
	StateComplete
	// StateFailed means the connection ended before a response arrived.
	StateFailed
	// StateHeldRequest means interception is holding the request.
	StateHeldRequest
	// StateHeldResponse means interception is holding the response.
	StateHeldResponse
	// StateDropped means the user dropped the request or response.
	StateDropped
)

// Held reports whether the flow is waiting on the user.
func (s FlowState) Held() bool {
	return s == StateHeldRequest || s == StateHeldResponse
}

// Source says where a flow came from. The request list shows them all
// together; the source tells them apart.
type Source string

const (
	// SourceProxy is traffic that went through the proxy: what the browser
	// sent, intercepted or not. It is the zero value.
	SourceProxy Source = ""
	// SourceReplay is a request the user sent from the Replay tool.
	SourceReplay Source = "replay"
	// SourceAgent is a request an AI agent sent (through a replay session).
	SourceAgent Source = "agent"
	// SourcePlugin is a request a plugin sent. Flow.Origin names the plugin.
	SourcePlugin Source = "plugin"
	// SourceCrawl is a request Autocrawl sent (see package crawl).
	SourceCrawl Source = "crawl"
)

// Protocol says what kind of exchange a flow is. The zero value is HTTP,
// which is almost everything the store holds.
type Protocol string

const (
	// ProtocolHTTP is an HTTP/1.x exchange (or a WebSocket that began as
	// one). It is the zero value.
	ProtocolHTTP Protocol = ""
	// ProtocolDNS is a name lookup made on the way to a connection (see
	// package dns). Its Request and Response are text, not wire bytes: the
	// query and the answer in the form dns.QueryText and dns.AnswerText
	// write, which is also what interception shows and edits. Method is
	// "DNS", Host the name looked up, and Path a one-line summary of the
	// answer.
	ProtocolDNS Protocol = "dns"
)

// Message is one side of an exchange exactly as it appeared on the wire.
type Message struct {
	// Raw holds the bytes of the message: start line, headers and body. It is
	// capped at the store's capture limit; see Truncated.
	Raw []byte
	// Size is the full on-the-wire size, which exceeds len(Raw) when the
	// message was truncated.
	Size int64
	// Truncated reports whether Raw is shorter than the real message.
	Truncated bool
}

// Flow is a single HTTP request and its response.
//
// Flows handed out by a Store are copies; the byte slices they reference are
// never mutated after being recorded, so a Flow is safe to read from any
// goroutine.
type Flow struct {
	ID         uint64
	State      FlowState
	ClientAddr string // browser side of the SOCKS connection
	Target     string // host:port the browser asked the proxy to reach

	Method string
	Host   string // Host header, falling back to Target
	Path   string // request-target, e.g. /search?q=1
	Secure bool   // true when the exchange was carried over TLS (https)

	// TargetID is the ID of the target whose scope this flow fell in, or ""
	// when no targets are in use. Traffic that belongs to no target is
	// passed through without being recorded at all, so a recorded flow only
	// lacks an ID when the scope rule is switched off.
	TargetID string

	// Source is where the flow came from: the proxy, or a replay sent by the
	// user, by an agent or by a plugin.
	Source Source
	// Origin names the plugin that sent the flow, for traffic a plugin
	// originated — directly, or through the proxy so that it is recorded
	// and interceptable like the browser's. It is "" otherwise. A plugin's
	// hooks skip the flows it originated, which is what stops a plugin that
	// sends traffic from re-triggering itself.
	Origin string

	// Protocol is HTTP (the zero value) or DNS. Everything that works on
	// HTTP messages — replay, the Explorer, plugins' traffic hooks — skips the
	// flows that aren't.
	Protocol Protocol

	StatusCode int    // 0 until the response arrives
	StatusText string // e.g. "200 OK"

	Request  Message
	Response Message

	// OriginalRequest and OriginalResponse hold the message as it arrived
	// when the user edited it before forwarding; nil if it wasn't edited.
	// Request and Response are always what was actually sent on.
	OriginalRequest  *Message
	OriginalResponse *Message

	Started  time.Time     // when the request finished arriving
	Duration time.Duration // until the response body finished
	Error    string        // set when State is StateFailed or StateDropped

	// Messages holds the WebSocket messages exchanged after this flow's
	// 101 upgrade, in order. It is nil for an ordinary HTTP flow.
	Messages []WSMessage
	// wsBytes totals the recorded message payloads, for the budget.
	wsBytes int64
}

// Message budget per flow: a chatty socket must not grow without bound.
const (
	maxWSMessages = 2000
	maxWSBytes    = 32 << 20
)

// WSDirection is which way a WebSocket message went.
type WSDirection int8

const (
	// Sent is a message the browser sent (client → server).
	Sent WSDirection = iota
	// Received is a message from the server (server → client).
	Received
)

func (d WSDirection) String() string {
	if d == Received {
		return "received"
	}
	return "sent"
}

// WSType is a WebSocket data message's kind.
type WSType int8

const (
	WSText WSType = iota
	WSBinary
)

func (t WSType) String() string {
	if t == WSBinary {
		return "binary"
	}
	return "text"
}

// WSMessage is one WebSocket message exchanged over a flow's connection.
type WSMessage struct {
	// Seq is the message's position in the flow, from 1.
	Seq  int
	Dir  WSDirection
	Type WSType
	// Data is the message payload, decompressed. It may be shorter than the
	// message was (Truncated) when the message passed the size limit.
	Data      []byte
	Truncated bool
	// Edited is set when a plugin changed the payload before it was
	// forwarded; Original then holds what arrived. Dropped is set when a
	// plugin dropped the message, so it was recorded but never forwarded.
	Edited   bool
	Dropped  bool
	Original []byte
	At       time.Time
}

// Edited reports whether either message was changed by the user.
func (f Flow) Edited() bool {
	return f.OriginalRequest != nil || f.OriginalResponse != nil
}

// IsDNS reports whether the flow is a name lookup rather than HTTP.
func (f Flow) IsDNS() bool { return f.Protocol == ProtocolDNS }

// URL returns a display URL for the flow: https when the exchange was
// decrypted from TLS, http otherwise, and dns://name for a lookup.
func (f Flow) URL() string {
	if f.Protocol == ProtocolDNS {
		return "dns://" + f.Host
	}
	scheme := "http"
	if f.Secure {
		scheme = "https"
	}
	return scheme + "://" + f.Host + f.Path
}

// SearchText returns the text the request-list filter matches against: the
// method, URL and status on one line, e.g. "GET https://host/path 200 OK",
// followed by the source for a replayed flow ("... replay" or "... agent"),
// so a filter can pick those out.
// Keeping it here (not in the UI) lets the filter, and later plugins, share
// one notion of what a row's searchable content is.
func (f Flow) SearchText() string {
	s := f.Method + " " + f.URL()
	if f.StatusText != "" {
		s += " " + f.StatusText
	}
	if f.Protocol == ProtocolDNS && f.Path != "" {
		s += " " + f.Path // the answer, so a filter can find an address
	}
	if f.Source != SourceProxy {
		s += " " + string(f.Source)
	}
	return s
}
