package replay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"reproxy/internal/capture"
)

// Owner says who created a replay session or made a send: OwnerUser for the
// person at the keyboard, or "agent:<id>" for an agent session.
type Owner string

// OwnerUser is the user working in a Replay window.
const OwnerUser Owner = "user"

// AgentOwner returns the Owner for the agent session with the given ID.
func AgentOwner(agentSessionID string) Owner { return Owner("agent:" + agentSessionID) }

// maxHistory bounds how many sends a session keeps, oldest dropped first,
// so a long automated run can't grow memory without limit.
const maxHistory = 500

// Template is what a replay session starts from: a captured request and
// where it was going.
type Template struct {
	FlowID uint64 // the captured flow it came from (0 if none)
	Addr   string // host:port to connect to
	Secure bool
	Host   string // the original Host, which the scope rule is measured from
	// TargetID is the target the original flow fell in, so its replays are
	// tagged with the same one.
	TargetID string
	Method   string
	URL      string // display URL of the original request
	Raw      []byte // the original request bytes
}

// Entry is one send in a session's history.
type Entry struct {
	Seq int // 1-based, in send order; stable even after old entries are dropped
	At  time.Time
	By  Owner
	// FlowID is the flow this send was recorded as in the capture store, or
	// 0 when sends aren't being recorded. It is what lets a finding raised
	// in a replayed response point at the request that produced it.
	FlowID   uint64
	Request  []byte   // exact bytes sent
	Response Response // valid when Err == ""
	Err      string
}

// Session is a Replay tool session: a template plus the sends made from it.
// Sessions handed out by Sessions are copies; their byte slices are never
// mutated, so they are safe to read from any goroutine.
type Session struct {
	ID       uint64
	Owner    Owner
	Created  time.Time
	Template Template
	History  []Entry // oldest first
}

// Last returns the most recent send, if any.
func (s Session) Last() (Entry, bool) {
	if len(s.History) == 0 {
		return Entry{}, false
	}
	return s.History[len(s.History)-1], true
}

// SessionEventKind identifies what changed in a Sessions registry.
type SessionEventKind int

const (
	SessionCreated SessionEventKind = iota
	// SessionSent: a send finished; Entry holds it.
	SessionSent
	SessionClosed
)

// SessionEvent describes a change. Session is a summary copy without
// History; use Sessions.Get for the full history.
type SessionEvent struct {
	Kind    SessionEventKind
	Session Session
	Entry   Entry
}

// ErrNoSession is returned for an unknown or closed session ID.
var ErrNoSession = errors.New("replay: no such session")

// Sender performs one exchange; Send is the default. Tests substitute it.
type Sender func(context.Context, Request) (Response, error)

// Sessions is the concurrency-safe registry of replay sessions in this
// process: the user's Replay windows and agents' sessions alike, so each
// can see the other's work.
type Sessions struct {
	send Sender

	mu       sync.Mutex
	store    *capture.Store // where sends are recorded; nil records nowhere
	nextID   uint64
	sessions map[uint64]*Session

	subMu   sync.Mutex
	nextSub int
	subs    map[int]func(SessionEvent)
}

// NewSessions returns an empty registry that sends with send (nil: Send).
func NewSessions(send Sender) *Sessions {
	if send == nil {
		send = Send
	}
	return &Sessions{
		send:     send,
		nextID:   1,
		sessions: make(map[uint64]*Session),
		subs:     make(map[int]func(SessionEvent)),
	}
}

// Subscribe registers fn for every change. fn runs on the goroutine that
// made the change and must not block. The returned function unsubscribes.
func (s *Sessions) Subscribe(fn func(SessionEvent)) (unsubscribe func()) {
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.subMu.Unlock()
	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Sessions) emit(e SessionEvent) {
	s.subMu.Lock()
	fns := make([]func(SessionEvent), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.subMu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// Create starts a session from t.
func (s *Sessions) Create(owner Owner, t Template) Session {
	s.mu.Lock()
	sess := &Session{ID: s.nextID, Owner: owner, Created: time.Now(), Template: t}
	s.nextID++
	s.sessions[sess.ID] = sess
	out := *sess
	s.mu.Unlock()

	s.emit(SessionEvent{Kind: SessionCreated, Session: out})
	return out
}

// RecordTo makes every send also appear in store as a flow, so the request
// list shows replays (the user's and agents') beside proxied traffic. The
// flow is added when the send starts and completed when it finishes.
func (s *Sessions) RecordTo(store *capture.Store) {
	s.mu.Lock()
	s.store = store
	s.mu.Unlock()
}

// Get returns a copy of a session, history included.
func (s *Sessions) Get(id uint64) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, false
	}
	out := *sess
	out.History = append([]Entry(nil), sess.History...)
	return out, true
}

// List returns every session, oldest first, without history; Sends reports
// the number of sends each has made.
func (s *Sessions) List() []Session {
	s.mu.Lock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		c := *sess
		c.History = nil
		out = append(out, c)
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Sends returns how many sends a session has made in total (including any
// dropped from its bounded history).
func (s *Sessions) Sends(id uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || len(sess.History) == 0 {
		return 0
	}
	return sess.History[len(sess.History)-1].Seq
}

// Send sends raw to the session's target and records the result. A failed
// exchange is recorded too and returned as both entry.Err and err.
func (s *Sessions) Send(ctx context.Context, id uint64, raw []byte, by Owner) (Entry, error) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	var t Template
	if ok {
		t = sess.Template
	}
	store := s.store
	s.mu.Unlock()
	if !ok {
		return Entry{}, ErrNoSession
	}

	var flowID uint64
	if store != nil {
		flowID = store.Add(flowFor(t, raw, by, time.Now()))
	}
	// A lookup made on the way to the server is listed as this send's.
	ctx = capture.WithSource(ctx, SourceOf(by), "")
	resp, err := s.send(ctx, Request{Addr: t.Addr, Secure: t.Secure, Raw: raw})
	if store != nil {
		if err != nil {
			store.Fail(flowID, err.Error(), time.Now())
		} else {
			store.Complete(flowID, capture.Message{Raw: resp.Raw, Size: int64(resp.Size)},
				resp.StatusCode, resp.Status, time.Now())
		}
	}
	e := Entry{At: time.Now(), By: by, FlowID: flowID, Request: raw, Response: resp}
	if err != nil {
		e.Err = err.Error()
		e.Response = Response{}
	}

	s.mu.Lock()
	sess, ok = s.sessions[id]
	if !ok { // closed while in flight
		s.mu.Unlock()
		return e, errors.Join(ErrNoSession, err)
	}
	e.Seq = 1
	if n := len(sess.History); n > 0 {
		e.Seq = sess.History[n-1].Seq + 1
	}
	sess.History = append(sess.History, e)
	if len(sess.History) > maxHistory {
		sess.History = append([]Entry(nil), sess.History[len(sess.History)-maxHistory:]...)
	}
	summary := *sess
	summary.History = nil
	s.mu.Unlock()

	s.emit(SessionEvent{Kind: SessionSent, Session: summary, Entry: e})
	return e, err
}

// Close removes a session. Sends in flight finish but aren't recorded.
func (s *Sessions) Close(id uint64) {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	var summary Session
	if ok {
		summary = *sess
		summary.History = nil
	}
	s.mu.Unlock()
	if ok {
		s.emit(SessionEvent{Kind: SessionClosed, Session: summary})
	}
}

// TemplateFrom builds a session template from a captured flow.
func TemplateFrom(f capture.Flow) Template {
	return Template{
		FlowID:   f.ID,
		Addr:     f.Target,
		Secure:   f.Secure,
		Host:     f.Host,
		TargetID: f.TargetID,
		Method:   f.Method,
		URL:      f.URL(),
		Raw:      f.Request.Raw,
	}
}

// SourceOf maps who made a send to where its flow came from.
func SourceOf(by Owner) capture.Source {
	if strings.HasPrefix(string(by), "agent:") {
		return capture.SourceAgent
	}
	return capture.SourceReplay
}

// flowFor describes a send that is starting, for the capture store. The
// method, path and Host come from the bytes being sent, which may have been
// edited away from the template's.
func flowFor(t Template, raw []byte, by Owner, started time.Time) capture.Flow {
	method, path, host := requestHead(raw)
	if host == "" {
		host = t.Host
	}
	return capture.Flow{
		State:    capture.StatePending,
		Target:   t.Addr,
		Method:   method,
		Host:     host,
		Path:     path,
		Secure:   t.Secure,
		TargetID: t.TargetID,
		Source:   SourceOf(by),
		Request:  capture.Message{Raw: raw, Size: int64(len(raw))},
		Started:  started,
	}
}

// requestHead reads the method, request-target and Host header from a raw
// request. An absolute-form target ("GET http://host/path") is split into
// its host and path.
func requestHead(raw []byte) (method, path, host string) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	if !sc.Scan() {
		return "", "", ""
	}
	parts := strings.Fields(sc.Text())
	if len(parts) > 0 {
		method = parts[0]
	}
	if len(parts) > 1 {
		path = parts[1]
		if u, err := url.Parse(path); err == nil && u.Host != "" {
			host, path = u.Host, u.RequestURI()
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && host == "" &&
			strings.EqualFold(strings.TrimSpace(name), "Host") {
			host = strings.TrimSpace(value)
		}
	}
	return method, path, host
}
