// Package dns makes the name lookups that connections need, in one place,
// so that every one of them can be seen and intercepted.
//
// Browsers launched by reproxy send host names to the proxy rather than
// resolving them (SOCKS5 with remote DNS), and replays and plugins dial
// through here too, so this Resolver is where names are turned into
// addresses. With no upstream proxy chain it asks this machine's resolver;
// when the chain can carry names it leaves resolution to the far end, so
// DNS goes out through the upstream proxies like everything else.
//
// Each lookup that falls in scope is recorded in the capture.Store as a flow
// whose Protocol is DNS (see the text forms in text.go), and interception
// may hold its query and its answer when the target opts in
// (target.Intercept.DNS). Answers are cached for a short time, as a real
// resolver would, so a page that opens six connections to a host makes one
// lookup, and an answer the user edited keeps applying while it is cached.
//
// It is core and Qt-free. It runs once per new connection, never per
// message: a cached answer costs a map lookup and, while interception is on,
// one hold check.
package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
)

const (
	// DefaultTTL is how long an answer is reused. The system resolver
	// doesn't say what the record's TTL was, so this is a fixed, short one.
	DefaultTTL = 60 * time.Second
	// NegativeTTL is how long a failed lookup is reused.
	NegativeTTL = 5 * time.Second
	// lookupTimeout bounds one lookup (not a hold, which waits on the user).
	lookupTimeout = 10 * time.Second
	// sweepAt is the cache size past which expired entries are swept.
	sweepAt = 1024
)

// ErrDropped is returned for a lookup the user dropped while it was held.
var ErrDropped = errors.New("dns: lookup dropped")

// ScopeFunc decides whether a lookup is recorded and which target it
// belongs to. ips is empty for the query and holds the answer's addresses
// afterwards, so a lookup can fall in an IP or CIDR target once answered.
type ScopeFunc func(host string, ips []netip.Addr) (targetID string, collect bool)

// LookupFunc resolves a name on this machine.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// Who is the connection a lookup is made for.
type Who struct {
	Source     capture.Source
	Origin     string
	ClientAddr string
}

// Options configures a Resolver. Store and Intercept are required.
type Options struct {
	Store     *capture.Store
	Intercept *intercept.Interceptor
	// Scope decides what is recorded; nil records every lookup.
	Scope ScopeFunc
	// Remote reports whether the upstream chain resolves names (see
	// upstream.Dialer.ResolvesRemotely); nil means it never does.
	Remote func() bool
	// Lookup resolves locally; nil uses the system resolver.
	Lookup LookupFunc
	// TTL and NegativeTTL override the defaults when non-zero.
	TTL, NegativeTTL time.Duration
	// Now is the clock, for tests.
	Now func() time.Time
}

// Answer is what to connect to: the name itself, for an upstream proxy to
// resolve, or the addresses it resolved to, in order.
type Answer struct {
	Hosts []string
}

// Resolver resolves, records and caches lookups. The zero value is not
// usable; call New.
type Resolver struct {
	o Options

	mu    sync.Mutex
	cache map[string]*entry
	gen   uint64 // bumped by Flush, so a lookup in flight isn't cached
}

type entry struct {
	ready   chan struct{} // closed when the lookup is done
	answer  Answer
	err     error
	expires time.Time
	// held is set when interception could have held this lookup. An entry
	// made while nothing would have isn't reused once something would, so
	// switching DNS interception on takes effect at once.
	held bool
}

// New returns a Resolver.
func New(o Options) *Resolver {
	if o.Lookup == nil {
		o.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.TTL == 0 {
		o.TTL = DefaultTTL
	}
	if o.NegativeTTL == 0 {
		o.NegativeTTL = NegativeTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Resolver{o: o, cache: make(map[string]*entry)}
}

// Flush forgets every cached answer, for when what they were made under
// changes: the upstream chain, or the targets.
func (r *Resolver) Flush() {
	r.mu.Lock()
	r.cache = make(map[string]*entry)
	r.gen++
	r.mu.Unlock()
}

// Resolve returns what to connect to for host. An IP address is returned
// as it is, with no lookup. ctx bounds the wait, holds included.
func (r *Resolver) Resolve(ctx context.Context, host string, who Who) (Answer, error) {
	name := normalize(host)
	if name == "" {
		return Answer{Hosts: []string{host}}, nil
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return Answer{Hosts: []string{host}}, nil
	}

	r.mu.Lock()
	e, ok := r.cache[name]
	if ok {
		select {
		case <-e.ready:
			if r.o.Now().Before(e.expires) && (e.held || !r.wouldHold(name)) {
				r.mu.Unlock()
				return e.answer, e.err
			}
		default:
			// Someone else is looking it up: wait for their answer.
			r.mu.Unlock()
			select {
			case <-e.ready:
				return e.answer, e.err
			case <-ctx.Done():
				return Answer{}, ctx.Err()
			}
		}
	}
	e = &entry{ready: make(chan struct{})}
	r.cache[name] = e
	gen := r.gen
	if len(r.cache) > sweepAt {
		r.sweepLocked()
	}
	r.mu.Unlock()

	e.answer, e.held, e.err = r.lookup(ctx, name, who)

	r.mu.Lock()
	ttl := r.o.TTL
	if e.err != nil {
		ttl = r.o.NegativeTTL
	}
	e.expires = r.o.Now().Add(ttl)
	if (ctx.Err() != nil || errors.Is(e.err, ErrDropped) || r.gen != gen) && r.cache[name] == e {
		delete(r.cache, name) // not an answer worth keeping
	}
	r.mu.Unlock()
	close(e.ready)
	return e.answer, e.err
}

// wouldHold reports whether interception would hold a lookup of name now.
// With interception off it is one atomic load.
func (r *Resolver) wouldHold(name string) bool {
	s := intercept.Subject{DNS: true, Host: name, Method: "DNS"}
	return r.o.Intercept.Holds(intercept.Request, s) != 0 || r.o.Intercept.Holds(intercept.Response, s) != 0
}

func (r *Resolver) sweepLocked() {
	now := r.o.Now()
	for k, e := range r.cache {
		select {
		case <-e.ready:
			if !now.Before(e.expires) {
				delete(r.cache, k)
			}
		default:
		}
	}
}

func (r *Resolver) scope(name string, ips []netip.Addr) (string, bool) {
	if r.o.Scope == nil {
		return "", true
	}
	return r.o.Scope(name, ips)
}

// lookup resolves one name, recording and holding it as its scope and the
// targets say. held reports whether interception could have held it.
func (r *Resolver) lookup(ctx context.Context, name string, who Who) (ans Answer, held bool, err error) {
	remote := r.o.Remote != nil && r.o.Remote()
	ic := r.o.Intercept
	held = r.wouldHold(name)

	l := &recording{r: r, who: who, name: name, remote: remote}
	targetID, collect := r.scope(name, nil)
	if collect {
		l.start(targetID)
		if kind := ic.Holds(intercept.Request, l.subject(nil)); kind != 0 {
			if name, err = l.holdQuery(ctx, kind); err != nil {
				return Answer{}, held, err
			}
		}
	}

	// Resolve: here, or by passing the name on.
	status := StatusUpstream
	var addrs []netip.Addr
	if !remote {
		lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		addrs, err = r.o.Lookup(lctx, name)
		cancel()
		status = statusOf(err)
		for i := range addrs {
			addrs[i] = addrs[i].Unmap()
		}
	}

	if !collect && len(addrs) > 0 {
		// Not in scope by name; the addresses may put it in an IP target.
		if targetID, collect = r.scope(name, addrs); collect {
			l.start(targetID)
		}
	}
	if collect {
		if kind := ic.Holds(intercept.Response, l.subject(addrs)); kind != 0 {
			var herr error
			if addrs, status, herr = l.holdAnswer(ctx, kind, status, addrs); herr != nil {
				return Answer{}, held, herr
			}
			err = nil
			if len(addrs) == 0 && !remote {
				err = &net.DNSError{Err: "no addresses in the edited answer", Name: name, IsNotFound: true}
			}
		}
		l.finish(status, addrs)
	}

	if err != nil {
		return Answer{}, held, err
	}
	if len(addrs) == 0 {
		return Answer{Hosts: []string{name}}, held, nil // the upstream resolves it
	}
	hosts := make([]string, len(addrs))
	for i, a := range addrs {
		hosts[i] = a.String()
	}
	return Answer{Hosts: hosts}, held, nil
}

func statusOf(err error) Status {
	if err == nil {
		return StatusNoError
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return StatusNXDomain
		case de.IsTimeout:
			return StatusTimeout
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusServFail
}

// recording is one lookup's flow in the store.
type recording struct {
	r        *Resolver
	who      Who
	name     string
	remote   bool
	targetID string
	id       uint64
	started  time.Time
}

func (l *recording) subject(addrs []netip.Addr) intercept.Subject {
	s := intercept.Subject{
		DNS: true, Host: l.name, Method: "DNS", TargetID: l.targetID,
		Source: string(l.who.Source), Origin: l.who.Origin,
	}
	if len(addrs) > 0 {
		s.IP = addrs[0]
	}
	return s
}

func (l *recording) start(targetID string) {
	l.targetID = targetID
	l.started = l.r.o.Now()
	q := QueryText(l.name, l.remote)
	l.id = l.r.o.Store.Add(capture.Flow{
		Protocol:   capture.ProtocolDNS,
		ClientAddr: l.who.ClientAddr,
		Target:     l.name,
		Method:     "DNS",
		Host:       l.name,
		TargetID:   targetID,
		Source:     l.who.Source,
		Origin:     l.who.Origin,
		Request:    capture.Message{Raw: q, Size: int64(len(q))},
		Started:    l.started,
	})
}

// holdQuery holds the query and returns the name to look up: the one the
// user left in it.
func (l *recording) holdQuery(ctx context.Context, kind intercept.HoldKind) (string, error) {
	store := l.r.o.Store
	store.Update(l.id, func(f *capture.Flow) { f.State = capture.StateHeldRequest })
	f, _ := store.Get(l.id)
	d, err := l.r.o.Intercept.HoldAs(ctx, l.id, intercept.Request, f.Request.Raw, kind)
	if err != nil {
		store.Fail(l.id, "stopped while the lookup was held", l.r.o.Now())
		return "", err
	}
	if d.Verdict == intercept.Drop {
		store.Update(l.id, func(f *capture.Flow) {
			f.State = capture.StateDropped
			f.Error = "lookup dropped"
		})
		return "", ErrDropped
	}
	name := l.name
	if d.Edited {
		if n, ok := ParseQuery(d.Raw); ok {
			name = n
		}
	}
	store.Update(l.id, func(f *capture.Flow) {
		f.State = capture.StatePending
		if d.Edited {
			orig := f.Request
			f.OriginalRequest = &orig
			f.Request = capture.Message{Raw: d.Raw, Size: int64(len(d.Raw))}
			f.Host = name
		}
	})
	l.name = name
	return name, nil
}

// holdAnswer holds the answer and returns the addresses the user left in
// it.
func (l *recording) holdAnswer(ctx context.Context, kind intercept.HoldKind, status Status, addrs []netip.Addr) ([]netip.Addr, Status, error) {
	store := l.r.o.Store
	raw := AnswerText(status, addrs)
	store.Update(l.id, func(f *capture.Flow) {
		f.State = capture.StateHeldResponse
		f.Response = capture.Message{Raw: raw, Size: int64(len(raw))}
		f.StatusText = string(status)
		f.Path = Summary(status, addrs)
	})
	d, err := l.r.o.Intercept.HoldAs(ctx, l.id, intercept.Response, raw, kind)
	if err != nil {
		store.Fail(l.id, "stopped while the answer was held", l.r.o.Now())
		return nil, status, err
	}
	if d.Verdict == intercept.Drop {
		store.Update(l.id, func(f *capture.Flow) {
			f.State = capture.StateDropped
			f.Error = "answer dropped"
		})
		return nil, status, ErrDropped
	}
	if !d.Edited {
		return addrs, status, nil
	}
	edited := ParseAnswer(d.Raw)
	switch {
	case len(edited) > 0:
		status = StatusNoError
	case !l.remote:
		status = StatusNXDomain
	}
	store.Update(l.id, func(f *capture.Flow) {
		orig := capture.Message{Raw: raw, Size: int64(len(raw))}
		f.OriginalResponse = &orig
	})
	return edited, status, nil
}

func (l *recording) finish(status Status, addrs []netip.Addr) {
	now := l.r.o.Now()
	raw := AnswerText(status, addrs)
	l.r.o.Store.Update(l.id, func(f *capture.Flow) {
		f.State = capture.StateComplete
		f.Response = capture.Message{Raw: raw, Size: int64(len(raw))}
		f.StatusText = string(status)
		f.Path = Summary(status, addrs)
		f.Duration = now.Sub(l.started)
	})
}
