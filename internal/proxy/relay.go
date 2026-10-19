package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/websocket"
)

// parseBufSize is the read buffer used on each side of a tunnel.
const parseBufSize = 64 << 10

// exchange links a request to the response that answers it. HTTP/1.x answers
// requests in order, so a FIFO of exchanges pairs them.
type exchange struct {
	method  string
	host    string
	path    string
	upgrade bool
	// targetID is the target the request fell in, for the response's hold.
	targetID string
	// wsEditable marks a WebSocket upgrade whose compression was stripped,
	// so its messages can be edited; set in requests(), read in responses().
	wsEditable bool
	// passthrough marks the point where the request side stopped parsing;
	// the response side does the same once it gets here.
	passthrough bool
	// record is false for a request that belongs to no target: both sides
	// relay it without touching the store or the interceptor.
	record bool
	// id delivers the flow ID once the request is fully recorded; it is
	// closed without a value if the request never completed. It is unused
	// when record is false.
	id chan uint64
}

// tunnel is one SOCKS CONNECT carrying HTTP/1.x.
//
// Two loops run per tunnel: requests (browser→target) and responses
// (target→browser). Each reads one message's head, decides whether to hold
// it, then either streams it straight through or holds the complete message
// until the user forwards or drops it. Messages are forwarded byte-for-byte
// unless the user edited them.
type tunnel struct {
	s        *Server
	ctx      context.Context
	client   net.Conn
	upstream net.Conn
	cbr, ubr *bufio.Reader
	target   string
	ip       netip.Addr
	// source and origin label the flows: the proxy's own (the zero
	// values), or a plugin's that authenticated as one (see OriginFunc).
	source capture.Source
	origin string

	secure bool // true once the tunnel has been decrypted from TLS

	// ws is set by the response loop when it sees a 101 WebSocket upgrade,
	// so both directions know to frame the rest as WebSocket messages.
	ws *wsState
	// nextWSEditable carries a stripped WebSocket upgrade's editability from
	// the scope check to newExchange; owned by the request goroutine.
	nextWSEditable bool

	pending  chan *exchange
	upgraded chan bool     // response loop → request loop, for upgrade requests
	respDone chan struct{} // closed when the response loop exits

	closeOnce sync.Once
}

// wsState is a tunnel carrying WebSocket frames after its 101 upgrade.
type wsState struct {
	flowID   uint64
	editable bool
	accepted websocket.Accepted
}

// relay runs a tunnel until both directions are finished.
func (s *Server) relay(ctx context.Context, client, upstream net.Conn, target string, source capture.Source, origin string) {
	t := &tunnel{
		source:   source,
		origin:   origin,
		s:        s,
		ctx:      ctx,
		client:   client,
		upstream: upstream,
		cbr:      bufio.NewReaderSize(client, parseBufSize),
		ubr:      bufio.NewReaderSize(upstream, parseBufSize),
		target:   target,
		pending:  make(chan *exchange, 16),
		upgraded: make(chan bool, 1),
		respDone: make(chan struct{}),
	}
	if ap, err := netip.ParseAddrPort(upstream.RemoteAddr().String()); err == nil {
		t.ip = ap.Addr()
	}

	switch {
	case isClientHello(t.cbr):
		if s.ca == nil {
			// No CA: pass the TLS connection through untouched, not recorded.
			t.raw()
			return
		}
		if !t.mitm() {
			return // handshake failed; connection already closed
		}
	case !looksLikeHTTP(t.cbr):
		// Not HTTP and not TLS we can intercept: pass through.
		t.raw()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.requests() }()
	go func() { defer wg.Done(); t.responses() }()
	wg.Wait()
}

// mitm terminates the browser's TLS connection with a certificate minted for
// the requested host, opens a fresh TLS connection to the real server, and
// replaces the tunnel's connections with the decrypted streams so the normal
// HTTP loops record the traffic. It reports false if either handshake fails.
//
// The upstream connection is deliberately not verified: staging environments
// often use internal-CA or self-signed certificates, and failing the QA run
// on those would defeat the purpose. (A future setting can tighten this.)
func (t *tunnel) mitm() bool {
	clientTLS := tls.Server(bufferedConn{r: t.cbr, Conn: t.client}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return t.s.ca.LeafFor(certNameFor(hi.ServerName, t.target))
		},
	})
	if err := clientTLS.Handshake(); err != nil {
		t.s.log.Debug("client TLS handshake failed", "err", err, "target", t.target)
		t.abort()
		return false
	}

	name := certNameFor(clientTLS.ConnectionState().ServerName, t.target)
	upstreamTLS := tls.Client(t.upstream, &tls.Config{
		ServerName:         name,
		InsecureSkipVerify: true, // staging certs are not verified (see doc above)
		MinVersion:         tls.VersionTLS12,
	})
	if err := upstreamTLS.Handshake(); err != nil {
		t.s.log.Debug("upstream TLS handshake failed", "err", err, "target", t.target)
		t.abort()
		return false
	}

	t.client, t.upstream = clientTLS, upstreamTLS
	t.cbr = bufio.NewReaderSize(clientTLS, parseBufSize)
	t.ubr = bufio.NewReaderSize(upstreamTLS, parseBufSize)
	t.secure = true
	return true
}

// certNameFor picks the name a leaf certificate is issued for: the SNI the
// browser sent, falling back to the CONNECT target's host.
func certNameFor(sni, target string) string {
	if sni != "" {
		return sni
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}

// bufferedConn is a net.Conn whose reads come from r (a bufio.Reader holding
// bytes already peeked from the real connection), so wrapping it in a TLS
// server doesn't lose the ClientHello.
type bufferedConn struct {
	r io.Reader
	net.Conn
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// abort closes both connections, ending both loops.
func (t *tunnel) abort() {
	t.closeOnce.Do(func() {
		_ = t.client.Close()
		_ = t.upstream.Close()
	})
}

// raw copies both directions verbatim until both are done.
func (t *tunnel) raw() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.rawRequests() }()
	go func() { defer wg.Done(); t.rawResponses() }()
	wg.Wait()
}

// wsPipe carries one direction of the tunnel after a 101. With a WebSocket
// upgrade it frames the stream so each message is recorded (and, on an
// editable connection, offered to plugins); otherwise it is a byte copy,
// like any other upgraded protocol.
func (t *tunnel) wsPipe(src *bufio.Reader, dst net.Conn, dir websocket.Direction) {
	if t.ws == nil {
		if dir == websocket.ClientToServer {
			t.rawRequests()
		} else {
			t.rawResponses()
		}
		return
	}
	ws := t.ws
	compressed := ws.accepted.Compressed
	ctxTakeover := ws.accepted.ServerContextTakeover
	if dir == websocket.ClientToServer {
		ctxTakeover = ws.accepted.ClientContextTakeover
	}
	limit := t.s.captureLimit
	handle := func(m websocket.Message) websocket.Decision {
		var d websocket.Decision
		if ws.editable && t.s.ws != nil {
			d = t.s.ws.Decide(t.ctx, WSMessageInfo{FlowID: ws.flowID, Dir: m.Dir, Type: m.Type, Data: m.Data})
		}
		rec := capture.WSMessage{
			Dir: capture.WSDirection(m.Dir), Type: capture.WSType(m.Type),
			Data: m.Data, Truncated: m.Truncated, At: time.Now(),
		}
		switch {
		case d.Drop:
			rec.Dropped, rec.Original = true, m.Data
			rec.Data = m.Data
		case d.Data != nil:
			rec.Edited, rec.Original = true, m.Data
			rec.Data = d.Data
		}
		t.s.store.AddWSMessage(ws.flowID, rec)
		return d
	}
	err := websocket.RelayWith(src, dst, dir, ws.editable, compressed, ctxTakeover, limit, handle)
	if err != nil {
		t.abort()
	}
	if dir == websocket.ServerToClient && t.s.ws != nil {
		t.s.ws.Closed(ws.flowID)
	}
	closeWrite(dst)
}

func (t *tunnel) rawRequests() {
	_, _ = io.Copy(t.upstream, t.cbr)
	closeWrite(t.upstream)
}

func (t *tunnel) rawResponses() {
	_, _ = io.Copy(t.client, t.ubr)
	closeWrite(t.client)
}

// ---- browser → target ------------------------------------------------------

func (t *tunnel) requests() {
	defer close(t.pending)
	for {
		head, err := readHead(t.cbr)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.s.log.Debug("reading request", "err", err)
			}
			closeWrite(t.upstream)
			return
		}
		req, fr, err := parseRequestHead(head)
		if err != nil {
			// Not something we can frame. Send it on and stop parsing; the
			// tunnel becomes a plain byte pipe in this direction.
			t.s.log.Debug("unparseable request; passing through", "err", err)
			done := make(chan uint64)
			close(done) // no flow to report
			t.pending <- &exchange{passthrough: true, id: done}
			if _, err := t.upstream.Write(head); err == nil {
				t.rawRequests()
			}
			return
		}

		// Out-of-scope traffic is relayed and forgotten: no flow, no hold.
		targetID, collect := t.s.inScope(hostOr(req.Host, t.target), t.ip, req.RequestURI)

		var ex *exchange
		if !collect {
			ex, err = t.passRequest(head, req, fr)
		} else {
			subject := intercept.Subject{
				Host: req.Host, IP: t.ip, Path: req.RequestURI, ContentType: req.Header.Get("Content-Type"),
				Method: req.Method, TargetID: targetID, Source: string(t.source), Origin: t.origin,
			}
			// A WebSocket upgrade a plugin wants to edit has its compression
			// stripped here, before the request goes on, so the messages
			// flow uncompressed and an edit never needs re-compressing.
			t.nextWSEditable = false
			if t.s.ws != nil && websocket.IsRequest(req) && t.s.ws.Edits(subject) {
				head = websocket.StripExtensions(head)
				t.nextWSEditable = true
			}
			if kind := t.s.intercept.Holds(intercept.Request, subject); kind != 0 {
				ex, err = t.holdRequest(head, req, fr, targetID, kind)
			} else {
				ex, err = t.streamRequest(head, req, fr, targetID)
			}
		}
		if err != nil {
			return
		}

		if ex.upgrade {
			select {
			case ok := <-t.upgraded:
				if ok {
					t.wsPipe(t.cbr, t.upstream, websocket.ClientToServer)
					return
				}
			case <-t.respDone:
				return
			}
		}
	}
}

// passRequest relays a request that belongs to no target. It is framed
// like any other (so the connection keeps working and later requests on it
// can still be in scope) but nothing about it is kept.
func (t *tunnel) passRequest(head []byte, req *http.Request, fr framing) (*exchange, error) {
	if _, err := t.upstream.Write(head); err != nil {
		t.abort()
		return nil, err
	}
	ex := t.newExchange(req)
	ex.record = false
	t.pending <- ex
	if err := copyBody(t.upstream, t.cbr, fr, io.Discard); err != nil {
		t.abort()
		return nil, err
	}
	return ex, nil
}

func (t *tunnel) streamRequest(head []byte, req *http.Request, fr framing, targetID string) (*exchange, error) {
	if _, err := t.upstream.Write(head); err != nil {
		t.abort()
		return nil, err
	}
	// Queue the exchange before the body so the response loop can relay an
	// interim "100 Continue" the browser may be waiting for.
	ex := t.newExchange(req)
	ex.targetID = targetID
	t.pending <- ex

	body := &capped{limit: max(t.s.captureLimit-len(head), 0)}
	if err := copyBody(t.upstream, t.cbr, fr, body); err != nil {
		close(ex.id)
		t.abort()
		return nil, err
	}

	raw := append(append(make([]byte, 0, len(head)+len(body.buf)), head...), body.buf...)
	ex.id <- t.s.store.Add(capture.Flow{
		ClientAddr: t.client.RemoteAddr().String(),
		Target:     t.target,
		Method:     req.Method,
		Host:       hostOr(req.Host, t.target),
		Path:       req.RequestURI,
		Secure:     t.secure,
		TargetID:   targetID,
		Source:     t.source,
		Origin:     t.origin,
		Request:    capture.Message{Raw: raw, Size: int64(len(head)) + body.size, Truncated: body.truncated()},
		Started:    time.Now(),
	})
	return ex, nil
}

func (t *tunnel) holdRequest(head []byte, req *http.Request, fr framing, targetID string, kind intercept.HoldKind) (*exchange, error) {
	var buf bytes.Buffer
	buf.Write(head)
	if err := copyBody(&buf, t.cbr, fr, io.Discard); err != nil {
		t.abort()
		return nil, err
	}
	raw := buf.Bytes()

	id := t.s.store.Add(capture.Flow{
		State:      capture.StateHeldRequest,
		ClientAddr: t.client.RemoteAddr().String(),
		Target:     t.target,
		Method:     req.Method,
		Host:       hostOr(req.Host, t.target),
		Path:       req.RequestURI,
		Secure:     t.secure,
		TargetID:   targetID,
		Source:     t.source,
		Origin:     t.origin,
		Request:    capture.Message{Raw: raw, Size: int64(len(raw))},
		Started:    time.Now(),
	})

	d, err := t.s.intercept.HoldAs(t.ctx, id, intercept.Request, raw, kind)
	if err != nil {
		t.s.store.Fail(id, "proxy stopped while the request was held", time.Now())
		t.abort()
		return nil, err
	}
	if d.Verdict == intercept.Drop {
		t.s.store.Update(id, func(f *capture.Flow) {
			f.State = capture.StateDropped
			f.Error = "request dropped"
		})
		t.abort()
		return nil, errors.New("dropped")
	}

	// What was sent may differ from what arrived; describe what was sent.
	sent := req
	if d.Edited {
		if r, _, err := parseRequestHead(headOf(d.Raw)); err == nil {
			sent = r
		}
	}
	if _, err := t.upstream.Write(d.Raw); err != nil {
		t.s.store.Fail(id, "could not send request: "+err.Error(), time.Now())
		t.abort()
		return nil, err
	}
	t.s.store.Update(id, func(f *capture.Flow) {
		f.State = capture.StatePending
		f.Started = time.Now() // time the server, not the hold
		if d.Edited {
			orig := f.Request
			f.OriginalRequest = &orig
			f.Request = capture.Message{Raw: d.Raw, Size: int64(len(d.Raw))}
			f.Method, f.Host, f.Path = sent.Method, hostOr(sent.Host, t.target), sent.RequestURI
		}
	})

	ex := t.newExchange(sent)
	ex.targetID = targetID
	ex.id <- id
	t.pending <- ex
	return ex, nil
}

func (t *tunnel) newExchange(req *http.Request) *exchange {
	return &exchange{
		method:     req.Method,
		host:       req.Host,
		path:       req.RequestURI,
		upgrade:    isUpgrade(req),
		wsEditable: t.nextWSEditable,
		record:     true,
		id:         make(chan uint64, 1),
	}
}

// ---- target → browser ------------------------------------------------------

// next says how the response loop carries on after a response.
type next int

const (
	nextResponse next = iota // keep parsing responses
	nextRaw                  // stop parsing; copy the rest verbatim
	nextStop                 // this direction is finished
)

func (t *tunnel) responses() {
	defer close(t.respDone)
	for ex := range t.pending {
		n := nextRaw
		if !ex.passthrough {
			n = t.response(ex)
		}
		if n == nextResponse {
			continue
		}
		// Requests may keep arriving; settle them without blocking the
		// request loop.
		go t.drainPending()
		if n == nextRaw {
			t.wsPipe(t.ubr, t.client, websocket.ServerToClient)
		}
		return
	}
	// The browser finished sending and every request is answered.
	closeWrite(t.client)
}

// drainPending fails every exchange still queued, until the request loop
// stops.
func (t *tunnel) drainPending() {
	for ex := range t.pending {
		t.failExchange(ex, "response could not be recorded")
	}
}

// response relays the response(s) for ex.
func (t *tunnel) response(ex *exchange) next {
	for {
		head, err := readHead(t.ubr)
		if err != nil {
			t.failExchange(ex, "connection closed before a response was received")
			closeWrite(t.client)
			return nextStop
		}
		resp, fr, err := parseResponseHead(head, ex.method)
		if err != nil {
			t.failExchange(ex, "could not parse response: "+err.Error())
			if _, err := t.client.Write(head); err != nil {
				t.abort()
				return nextStop
			}
			return nextRaw
		}

		// Interim responses (100 Continue, 103 Early Hints) go straight on.
		if resp.StatusCode/100 == 1 && resp.StatusCode != http.StatusSwitchingProtocols {
			if _, err := t.client.Write(head); err != nil {
				t.abort()
				return nextStop
			}
			continue
		}

		var ok bool
		var wsID uint64
		if !ex.record {
			ok = t.passResponse(head, fr)
		} else {
			id, have := <-ex.id
			if !have {
				t.abort()
				return nextStop
			}
			wsID = id
			subject := intercept.Subject{
				Host: ex.host, IP: t.ip, Path: ex.path, ContentType: resp.Header.Get("Content-Type"),
				Method: ex.method, Status: resp.StatusCode, TargetID: ex.targetID,
				Source: string(t.source), Origin: t.origin,
			}
			if kind := t.s.intercept.Holds(intercept.Response, subject); kind != 0 {
				ok = t.holdResponse(id, head, resp, fr, kind)
			} else {
				ok = t.streamResponse(id, head, resp, fr)
			}
		}
		if !ok {
			return nextStop
		}

		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols && ex.upgrade:
			if t.s.ws != nil && ex.record && websocket.IsResponse(resp) {
				t.ws = &wsState{flowID: wsID, editable: ex.wsEditable, accepted: websocket.Accept(resp)}
				t.s.ws.Opened(wsID, intercept.Subject{
					Host: ex.host, IP: t.ip, Path: ex.path, Method: ex.method,
					TargetID: ex.targetID, Source: string(t.source), Origin: t.origin,
				})
			}
			t.upgraded <- true
			return nextRaw
		case ex.upgrade:
			t.upgraded <- false
		}
		if fr.kind == bodyUntilClose {
			closeWrite(t.client)
			return nextStop
		}
		return nextResponse
	}
}

// passResponse relays the response to an out-of-scope request, keeping
// nothing. It mirrors passRequest on the other side of the tunnel.
func (t *tunnel) passResponse(head []byte, fr framing) bool {
	if _, err := t.client.Write(head); err != nil {
		t.abort()
		return false
	}
	if err := copyBody(t.client, t.ubr, fr, io.Discard); err != nil {
		t.abort()
		return false
	}
	return true
}

func (t *tunnel) streamResponse(id uint64, head []byte, resp *http.Response, fr framing) bool {
	if _, err := t.client.Write(head); err != nil {
		t.s.store.Fail(id, "browser closed the connection", time.Now())
		t.abort()
		return false
	}
	body := &capped{limit: max(t.s.captureLimit-len(head), 0)}
	err := copyBody(t.client, t.ubr, fr, body)

	raw := append(append(make([]byte, 0, len(head)+len(body.buf)), head...), body.buf...)
	msg := capture.Message{Raw: raw, Size: int64(len(head)) + body.size, Truncated: body.truncated()}
	if err != nil {
		t.s.store.Update(id, func(f *capture.Flow) {
			f.Response = msg
			f.StatusCode, f.StatusText = resp.StatusCode, resp.Status
		})
		t.s.store.Fail(id, "connection closed before the response body was complete", time.Now())
		t.abort()
		return false
	}
	t.s.store.Complete(id, msg, resp.StatusCode, resp.Status, time.Now())
	return true
}

func (t *tunnel) holdResponse(id uint64, head []byte, resp *http.Response, fr framing, kind intercept.HoldKind) bool {
	var buf bytes.Buffer
	buf.Write(head)
	if err := copyBody(&buf, t.ubr, fr, io.Discard); err != nil {
		t.s.store.Fail(id, "connection closed before the response body was complete", time.Now())
		t.abort()
		return false
	}
	raw := buf.Bytes()
	arrived := time.Now()
	t.s.store.Update(id, func(f *capture.Flow) {
		f.State = capture.StateHeldResponse
		f.Response = capture.Message{Raw: raw, Size: int64(len(raw))}
		f.StatusCode, f.StatusText = resp.StatusCode, resp.Status
		f.Duration = arrived.Sub(f.Started)
	})

	d, err := t.s.intercept.HoldAs(t.ctx, id, intercept.Response, raw, kind)
	if err != nil {
		t.s.store.Fail(id, "proxy stopped while the response was held", time.Now())
		t.abort()
		return false
	}
	if d.Verdict == intercept.Drop {
		t.s.store.Update(id, func(f *capture.Flow) {
			f.State = capture.StateDropped
			f.Error = "response dropped"
		})
		t.abort()
		return false
	}
	if _, err := t.client.Write(d.Raw); err != nil {
		t.s.store.Fail(id, "browser closed the connection", time.Now())
		t.abort()
		return false
	}

	t.s.store.Update(id, func(f *capture.Flow) {
		f.State = capture.StateComplete
		if d.Edited {
			orig := f.Response
			f.OriginalResponse = &orig
			f.Response = capture.Message{Raw: d.Raw, Size: int64(len(d.Raw))}
			if r, _, err := parseResponseHead(headOf(d.Raw), f.Method); err == nil {
				f.StatusCode, f.StatusText = r.StatusCode, r.Status
			}
		}
	})
	return true
}

func (t *tunnel) failExchange(ex *exchange, reason string) {
	if !ex.record {
		return // nothing was recorded, so there is nothing to fail
	}
	if id, ok := <-ex.id; ok {
		t.s.store.Fail(id, reason, time.Now())
	}
}

// ---- helpers ---------------------------------------------------------------

// closeWrite half-closes c so the peer sees EOF while the other direction
// keeps flowing.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// looksLikeHTTP reports whether the stream starts like an HTTP/1.x request
// line (an upper-case method token).
func looksLikeHTTP(br *bufio.Reader) bool {
	b, err := br.Peek(1)
	return err == nil && b[0] >= 'A' && b[0] <= 'Z'
}

// isClientHello reports whether the stream starts with a TLS handshake
// record (0x16), i.e. a browser opening an HTTPS connection.
func isClientHello(br *bufio.Reader) bool {
	b, err := br.Peek(1)
	return err == nil && b[0] == 0x16
}

func isUpgrade(req *http.Request) bool {
	for _, v := range req.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// headOf returns the start line and headers of a raw message.
func headOf(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+4]
	}
	return raw
}

func hostOr(host, fallback string) string {
	if host == "" {
		return fallback
	}
	return host
}
