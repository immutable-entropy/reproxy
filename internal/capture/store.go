package capture

import (
	"sync"
	"time"
)

// EventKind identifies what changed in a Store.
type EventKind int

const (
	// FlowAdded is emitted when a new request is recorded.
	FlowAdded EventKind = iota
	// FlowUpdated is emitted when a flow's response (or failure) is recorded.
	FlowUpdated
	// FlowsCleared is emitted when all flows are removed. Flow is zero.
	FlowsCleared
	// WSMessageAdded is emitted when a WebSocket message is recorded on a
	// flow. The message is in WSMessage; Flow is the flow it belongs to.
	WSMessageAdded
	// FlowsReset is emitted when the whole list is replaced at once, as
	// when an engagement's saved traffic is loaded (see Reset). The new
	// flows are in Flows; Flow is zero. A subscriber that only follows
	// single flows can treat it like FlowsCleared.
	FlowsReset
)

// Event describes a change to a Store.
type Event struct {
	Kind EventKind
	Flow Flow
	// Message is the WebSocket message for WSMessageAdded.
	Message WSMessage
	// Prev is the flow's state before an update: for FlowUpdated it tells a
	// transition (held request → pending is "forwarded") from a change that
	// left the state alone. It is the zero state for every other kind.
	Prev FlowState
	// Flows carries the whole list for FlowsReset, in arrival order. It is
	// nil for every other kind.
	Flows []Flow
}

// Store is a concurrency-safe, in-memory list of flows in arrival order.
type Store struct {
	mu     sync.Mutex
	nextID uint64
	flows  []*Flow
	byID   map[uint64]*Flow

	subMu   sync.Mutex
	nextSub int
	subs    map[int]func(Event)
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		nextID: 1,
		byID:   make(map[uint64]*Flow),
		subs:   make(map[int]func(Event)),
	}
}

// Subscribe registers fn to be called for every change. fn runs on the
// goroutine that made the change and must not block; GUI code should hand the
// event off to its own thread. The returned function removes the
// subscription.
func (s *Store) Subscribe(fn func(Event)) (unsubscribe func()) {
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

func (s *Store) emit(e Event) {
	s.subMu.Lock()
	fns := make([]func(Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.subMu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// Add records a new flow and returns its assigned ID. Any ID already set on
// f is overwritten; a zero State means StatePending.
func (s *Store) Add(f Flow) uint64 {
	s.mu.Lock()
	f.ID = s.nextID
	s.nextID++
	stored := f
	s.flows = append(s.flows, &stored)
	s.byID[f.ID] = &stored
	s.mu.Unlock()

	s.emit(Event{Kind: FlowAdded, Flow: f})
	return f.ID
}

// Complete attaches a response to a pending flow. It is a no-op if the flow
// no longer exists (for example because the store was cleared meanwhile).
func (s *Store) Complete(id uint64, resp Message, statusCode int, statusText string, finished time.Time) {
	s.Update(id, func(f *Flow) {
		f.State = StateComplete
		f.Response = resp
		f.StatusCode = statusCode
		f.StatusText = statusText
		f.Duration = finished.Sub(f.Started)
	})
}

// Fail marks a pending flow as having ended without a response.
func (s *Store) Fail(id uint64, reason string, finished time.Time) {
	s.Update(id, func(f *Flow) {
		f.State = StateFailed
		f.Error = reason
		f.Duration = finished.Sub(f.Started)
	})
}

// Update applies mutate to the flow with the given ID and notifies
// subscribers. mutate runs under the store's lock and must not call back into
// the store. It is a no-op if the flow no longer exists.
func (s *Store) Update(id uint64, mutate func(*Flow)) {
	s.mu.Lock()
	f, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	prev := f.State
	mutate(f)
	snapshot := *f
	s.mu.Unlock()

	s.emit(Event{Kind: FlowUpdated, Flow: snapshot, Prev: prev})
}

// Get returns a copy of the flow with the given ID.
func (s *Store) Get(id uint64) (Flow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byID[id]
	if !ok {
		return Flow{}, false
	}
	return *f, true
}

// List returns copies of all flows in arrival order.
func (s *Store) List() []Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Flow, len(s.flows))
	for i, f := range s.flows {
		out[i] = *f
	}
	return out
}

// Len returns the number of recorded flows.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flows)
}

// Reset replaces every flow at once, as when an engagement is opened and its
// saved traffic is loaded. The flows keep the IDs they arrive with — the
// archive they came from identifies them that way — and the next flow
// recorded takes the first ID above them, so a late update for a flow that is
// no longer here can never land on one that is.
//
// It emits a single FlowsReset rather than a clear and one add per flow:
// seeding a window with thousands of saved requests must cost one update.
func (s *Store) Reset(flows []Flow) {
	stored := make([]*Flow, len(flows))
	byID := make(map[uint64]*Flow, len(flows))
	next := uint64(1)
	for i := range flows {
		f := flows[i]
		stored[i] = &f
		byID[f.ID] = &f
		if f.ID >= next {
			next = f.ID + 1
		}
	}

	s.mu.Lock()
	if next < s.nextID {
		next = s.nextID // never hand out an ID this process has already used
	}
	s.flows, s.byID, s.nextID = stored, byID, next
	s.mu.Unlock()

	s.emit(Event{Kind: FlowsReset, Flows: flows})
}

// AddWSMessage records a WebSocket message on a flow and returns it with its
// Seq set, or false if the flow no longer exists or its message budget is
// spent. It never blocks the proxy beyond the store lock.
func (s *Store) AddWSMessage(flowID uint64, m WSMessage) (WSMessage, bool) {
	s.mu.Lock()
	f, ok := s.byID[flowID]
	if !ok {
		s.mu.Unlock()
		return WSMessage{}, false
	}
	if len(f.Messages) >= maxWSMessages || f.wsBytes >= maxWSBytes {
		s.mu.Unlock()
		return WSMessage{}, false
	}
	m.Seq = len(f.Messages) + 1
	f.Messages = append(f.Messages, m)
	f.wsBytes += int64(len(m.Data))
	snapshot := *f
	s.mu.Unlock()

	s.emit(Event{Kind: WSMessageAdded, Flow: snapshot, Message: m})
	return m, true
}

// Clear removes all flows. IDs keep increasing so that late updates for a
// cleared flow can never be mistaken for a new one.
func (s *Store) Clear() {
	s.mu.Lock()
	s.flows = nil
	s.byID = make(map[uint64]*Flow)
	s.mu.Unlock()

	s.emit(Event{Kind: FlowsCleared})
}
