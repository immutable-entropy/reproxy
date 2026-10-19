package intercept

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
)

// Direction says which side of an exchange a held message is.
type Direction int

const (
	Request Direction = iota
	Response
)

func (d Direction) String() string {
	if d == Response {
		return "response"
	}
	return "request"
}

// Verdict is what happens to a held message.
type Verdict int

const (
	// Forward sends the message on, possibly edited.
	Forward Verdict = iota
	// Drop discards the message and closes the browser's connection.
	Drop
)

// Decision is the outcome of a hold.
type Decision struct {
	Verdict Verdict
	// Raw is the bytes to send when forwarding.
	Raw []byte
	// Edited reports whether Raw differs from the held message.
	Edited bool
}

// ErrNotHeld is returned when resolving a flow that isn't (or is no longer)
// being held.
var ErrNotHeld = errors.New("intercept: message is not held")

type held struct {
	dir Direction
	// original is the message as it arrived; current is what the user is
	// shown and edits — the same bytes, unless a plugin edited it first.
	original []byte
	current  []byte
	decide   chan Decision // buffered(1); receives exactly one decision
}

// Pending is a message the interceptor is about to hold, as the stage sees
// it.
type Pending struct {
	FlowID uint64
	Dir    Direction
	Raw    []byte
	// ForUser is set when the user's interception holds the message; when
	// it is clear, a plugin asked for the hold, and the message goes on as
	// soon as the stage is done with it.
	ForUser bool
}

// HoldKind says who a message is held for: a set of these bits.
type HoldKind uint8

const (
	// HoldUser: the user's interception rules hold it.
	HoldUser HoldKind = 1 << iota
	// HoldPlugin: a plugin asked for it to be held (a target with hold:
	// true), whether or not interception is on.
	HoldPlugin
)

// PluginHoldFunc reports whether a plugin wants a message held. It runs for
// every message the proxy sees while installed, so it must be cheap (the
// plugin package installs one only while some enabled plugin asks to hold).
type PluginHoldFunc func(dir Direction, s Subject) bool

// StageResult is what a stage made of a pending message.
type StageResult struct {
	// Raw replaces the message; nil leaves it as it was.
	Raw []byte
	// Release, when set, decides the message now: it is forwarded (as Raw)
	// or dropped without ever reaching the user's queue.
	Release *Verdict
}

// StageFunc runs before a message is held for the user: it is how plugins
// see, edit, forward or drop what interception catches (see package
// plugin). It runs on the proxy goroutine that is holding the message and
// may block, bounded by ctx; nothing else waits on it.
type StageFunc func(ctx context.Context, p Pending) StageResult

// Interceptor decides what to hold and keeps held messages until they are
// resolved. Held messages are keyed by flow ID; a flow has at most one
// message held at a time (its request, then possibly its response).
type Interceptor struct {
	enabled  atomic.Bool
	updateCL atomic.Bool
	// userHold answers whether the user's interception (configured per
	// target) holds a subject's direction; nil falls back to holding
	// requests. The app installs one that consults the targets.
	userHold atomic.Pointer[UserHoldFunc]
	// stage is the plugin stage, nil when no plugin wants held messages.
	stage atomic.Pointer[StageFunc]
	// pluginHold decides plugins' holds; nil when no plugin asks for any.
	pluginHold atomic.Pointer[PluginHoldFunc]

	mu   sync.Mutex
	held map[uint64]*held

	subMu   sync.Mutex
	nextSub int
	subs    map[int]func(HeldEvent)
}

// HeldEvent reports a message starting or ending its hold. Held events are
// emitted after the message is registered, so a subscriber may Forward or
// Drop it straight away.
type HeldEvent struct {
	FlowID   uint64
	Dir      Direction
	Released bool // false: now held; true: forwarded, dropped or abandoned
}

// HeldMessage describes one held message.
type HeldMessage struct {
	FlowID uint64
	Dir    Direction
	// Raw is the message as it stands: as it arrived, or as a plugin's
	// stage edited it. It is never mutated.
	Raw []byte
}

// New returns a disabled Interceptor using s.
func New(s Settings) (*Interceptor, error) {
	i := &Interceptor{held: make(map[uint64]*held), subs: make(map[int]func(HeldEvent))}
	if err := i.Configure(s); err != nil {
		return nil, err
	}
	return i, nil
}

// Configure replaces the settings. Messages already held stay held.
func (i *Interceptor) Configure(s Settings) error {
	i.updateCL.Store(s.UpdateContentLength)
	return nil
}

// UserHoldFunc reports whether the user's interception holds a message of
// the given direction — the choice a target carries (see package target).
type UserHoldFunc func(dir Direction, s Subject) bool

// SetUserHold installs the function that decides the user's holds, or
// removes it (nil, so requests are held by default).
func (i *Interceptor) SetUserHold(fn UserHoldFunc) {
	if fn == nil {
		i.userHold.Store(nil)
		return
	}
	i.userHold.Store(&fn)
}

// Enabled reports whether interception is switched on.
func (i *Interceptor) Enabled() bool { return i.enabled.Load() }

// SetEnabled switches interception on or off. Switching it off forwards
// every held message unchanged, so nothing is left hanging.
func (i *Interceptor) SetEnabled(on bool) {
	i.enabled.Store(on)
	if on {
		return
	}
	i.mu.Lock()
	pending := i.held
	i.held = make(map[uint64]*held)
	i.mu.Unlock()
	for id, h := range pending {
		h.decide <- Decision{Verdict: Forward, Raw: h.current, Edited: !bytes.Equal(h.current, h.original)}
		i.emit(HeldEvent{FlowID: id, Dir: h.dir, Released: true})
	}
}

// Subscribe registers fn for every hold and release. fn runs on the
// goroutine that caused the change (often a proxy connection) and must not
// block or call back into the Interceptor synchronously. The returned
// function removes the subscription.
func (i *Interceptor) Subscribe(fn func(HeldEvent)) (unsubscribe func()) {
	i.subMu.Lock()
	id := i.nextSub
	i.nextSub++
	i.subs[id] = fn
	i.subMu.Unlock()
	return func() {
		i.subMu.Lock()
		delete(i.subs, id)
		i.subMu.Unlock()
	}
}

func (i *Interceptor) emit(e HeldEvent) {
	i.subMu.Lock()
	fns := make([]func(HeldEvent), 0, len(i.subs))
	for _, fn := range i.subs {
		fns = append(fns, fn)
	}
	i.subMu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// Held returns every held message, oldest flow first.
func (i *Interceptor) Held() []HeldMessage {
	i.mu.Lock()
	out := make([]HeldMessage, 0, len(i.held))
	for id, h := range i.held {
		out = append(out, HeldMessage{FlowID: id, Dir: h.dir, Raw: h.current})
	}
	i.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].FlowID < out[b].FlowID })
	return out
}

// Message returns the held message for flowID.
func (i *Interceptor) Message(flowID uint64) (HeldMessage, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	h, ok := i.held[flowID]
	if !ok {
		return HeldMessage{}, false
	}
	return HeldMessage{FlowID: flowID, Dir: h.dir, Raw: h.current}, true
}

// Holds reports who a message should be held for, if anyone: 0 means it
// streams. It is called for every message the proxy sees, so it stays
// cheap: two atomic loads when interception is off and no plugin holds.
func (i *Interceptor) Holds(dir Direction, s Subject) HoldKind {
	var k HoldKind
	if i.enabled.Load() && i.userWants(dir, s) {
		k |= HoldUser
	}
	if fn := i.pluginHold.Load(); fn != nil && !s.DNS && (*fn)(dir, s) {
		k |= HoldPlugin
	}
	return k
}

// userWants is the user's hold decision for a subject, from the installed
// UserHoldFunc, or the default of holding requests.
func (i *Interceptor) userWants(dir Direction, s Subject) bool {
	if fn := i.userHold.Load(); fn != nil {
		return (*fn)(dir, s)
	}
	return dir == Request
}

// SetPluginHold installs the function deciding plugins' holds, or removes
// it (nil).
func (i *Interceptor) SetPluginHold(fn PluginHoldFunc) {
	if fn == nil {
		i.pluginHold.Store(nil)
		return
	}
	i.pluginHold.Store(&fn)
}

// SetStage installs the stage that runs before each hold, or removes it
// (nil). With none, holding costs nothing extra.
func (i *Interceptor) SetStage(fn StageFunc) {
	if fn == nil {
		i.stage.Store(nil)
		return
	}
	i.stage.Store(&fn)
}

// Hold blocks until the message for flowID is resolved with Forward or Drop,
// interception is switched off, or ctx ends. raw must be the complete
// message. If ctx ends first, ctx.Err() is returned.
//
// When a stage is installed it sees the message first: it may edit it (the
// user is then shown the edit, and Decision.Edited compares against what
// arrived) or decide it outright, in which case the user never sees it.
func (i *Interceptor) Hold(ctx context.Context, flowID uint64, dir Direction, raw []byte) (Decision, error) {
	return i.HoldAs(ctx, flowID, dir, raw, HoldUser)
}

// HoldAs holds a message for whoever kind says (see Holds). A message held
// only for plugins never reaches the user's queue: once the stage is done
// with it, it is forwarded as the stage left it.
func (i *Interceptor) HoldAs(ctx context.Context, flowID uint64, dir Direction, raw []byte, kind HoldKind) (Decision, error) {
	current := raw
	forUser := kind&HoldUser != 0
	if st := i.stage.Load(); st != nil {
		res := (*st)(ctx, Pending{FlowID: flowID, Dir: dir, Raw: raw, ForUser: forUser})
		if err := ctx.Err(); err != nil {
			return Decision{}, err
		}
		if res.Raw != nil {
			current = res.Raw
		}
		edited := !bytes.Equal(current, raw)
		if res.Release != nil {
			if *res.Release == Drop {
				return Decision{Verdict: Drop}, nil
			}
			return Decision{Verdict: Forward, Raw: current, Edited: edited}, nil
		}
		if !forUser || !i.enabled.Load() { // only plugins held it, or interception went off
			return Decision{Verdict: Forward, Raw: current, Edited: edited}, nil
		}
	} else if !forUser {
		return Decision{Verdict: Forward, Raw: raw}, nil // the plugins have gone
	}
	h := &held{dir: dir, original: raw, current: current, decide: make(chan Decision, 1)}
	i.mu.Lock()
	i.held[flowID] = h
	i.mu.Unlock()
	i.emit(HeldEvent{FlowID: flowID, Dir: dir})

	select {
	case d := <-h.decide:
		return d, nil
	case <-ctx.Done():
		i.mu.Lock()
		abandoned := i.held[flowID] == h
		if abandoned {
			delete(i.held, flowID)
		}
		i.mu.Unlock()
		if abandoned {
			i.emit(HeldEvent{FlowID: flowID, Dir: dir, Released: true})
		}
		return Decision{}, ctx.Err()
	}
}

// IsHeld reports whether flowID has a message held, and which one.
func (i *Interceptor) IsHeld(flowID uint64) (Direction, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	h, ok := i.held[flowID]
	if !ok {
		return 0, false
	}
	return h.dir, true
}

// Forward releases the held message for flowID. edited is the message as
// text in the form produced by EditableText; nil sends the original bytes.
func (i *Interceptor) Forward(flowID uint64, edited *string) error {
	h, err := i.take(flowID)
	if err != nil {
		return err
	}
	d := Decision{Verdict: Forward, Raw: h.current}
	if edited != nil {
		d.Raw = ApplyEdit(h.current, *edited, i.updateCL.Load())
	}
	d.Edited = !bytes.Equal(d.Raw, h.original)
	h.decide <- d
	return nil
}

// Drop discards the held message for flowID.
func (i *Interceptor) Drop(flowID uint64) error {
	h, err := i.take(flowID)
	if err != nil {
		return err
	}
	h.decide <- Decision{Verdict: Drop}
	return nil
}

func (i *Interceptor) take(flowID uint64) (*held, error) {
	i.mu.Lock()
	h, ok := i.held[flowID]
	if ok {
		delete(i.held, flowID)
	}
	i.mu.Unlock()
	if !ok {
		return nil, ErrNotHeld
	}
	i.emit(HeldEvent{FlowID: flowID, Dir: h.dir, Released: true})
	return h, nil
}
