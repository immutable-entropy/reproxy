// Package approval holds the requests agents make to do something that
// sends or changes traffic, until the user approves or denies them.
//
// A Broker lives in the hub process. Tools call Ask and block; the UI (in
// any process, via the hub) lists pending requests and calls Decide. The
// first decision wins. A request nobody answers is denied after a timeout,
// and one whose caller goes away (its context ends) is withdrawn.
package approval

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"reproxy/internal/randid"
)

// DefaultTimeout is how long a request waits for the user.
const DefaultTimeout = 5 * time.Minute

// Request is one pending approval.
type Request struct {
	ID string `json:"id"`
	// Session is the agent session asking, or "" for an external MCP
	// client.
	Session string `json:"session,omitempty"`
	// Client names an external MCP client, when known.
	Client  string    `json:"client,omitempty"`
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"` // what will happen, for the user
	Detail  string    `json:"detail,omitempty"`
	Created time.Time `json:"created"`
}

// Event reports a request appearing or being resolved.
type Event struct {
	Request  Request `json:"request"`
	Resolved bool    `json:"resolved"`
	Approved bool    `json:"approved"`
}

// ErrUnknown is returned when deciding a request that is no longer pending.
var ErrUnknown = errors.New("approval: no such pending request")

type pending struct {
	req    Request
	result chan bool // buffered(1)
}

// Broker keeps pending requests.
type Broker struct {
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]*pending

	subMu   sync.Mutex
	nextSub int
	subs    map[int]func(Event)
}

// NewBroker returns a Broker that denies unanswered requests after timeout
// (0: DefaultTimeout).
func NewBroker(timeout time.Duration) *Broker {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Broker{timeout: timeout, pending: make(map[string]*pending), subs: make(map[int]func(Event))}
}

// Subscribe registers fn for every change; fn must not block. The returned
// function unsubscribes.
func (b *Broker) Subscribe(fn func(Event)) (unsubscribe func()) {
	b.subMu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = fn
	b.subMu.Unlock()
	return func() {
		b.subMu.Lock()
		delete(b.subs, id)
		b.subMu.Unlock()
	}
}

func (b *Broker) emit(e Event) {
	b.subMu.Lock()
	fns := make([]func(Event), 0, len(b.subs))
	for _, fn := range b.subs {
		fns = append(fns, fn)
	}
	b.subMu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// Ask files req and waits for the user. It returns true if approved; false
// with a nil error if denied or timed out; ctx.Err() if ctx ends first.
func (b *Broker) Ask(ctx context.Context, req Request) (bool, error) {
	req.ID = randid.Hex(6)
	req.Created = time.Now()
	p := &pending{req: req, result: make(chan bool, 1)}

	b.mu.Lock()
	b.pending[req.ID] = p
	b.mu.Unlock()
	b.emit(Event{Request: req})

	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case ok := <-p.result:
		return ok, nil
	case <-timer.C:
		if b.resolve(req.ID, false) == nil {
			return false, nil
		}
		return <-p.result, nil // decided at the last moment
	case <-ctx.Done():
		if b.resolve(req.ID, false) != nil {
			return <-p.result, nil
		}
		return false, ctx.Err()
	}
}

// Decide approves or denies a pending request.
func (b *Broker) Decide(id string, approve bool) error {
	return b.resolve(id, approve)
}

// DenyAll denies every pending request from an agent session (when it
// closes or is stopped).
func (b *Broker) DenyAll(session string) {
	for _, r := range b.Pending() {
		if r.Session == session {
			b.resolve(r.ID, false)
		}
	}
}

func (b *Broker) resolve(id string, approve bool) error {
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	p.result <- approve
	b.emit(Event{Request: p.req, Resolved: true, Approved: approve})
	return nil
}

// Pending returns the open requests, oldest first.
func (b *Broker) Pending() []Request {
	b.mu.Lock()
	out := make([]Request, 0, len(b.pending))
	for _, p := range b.pending {
		out = append(out, p.req)
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}
