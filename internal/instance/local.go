package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/filter"
	"reproxy/internal/focus"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

// Local is an Instance over this process's own core objects.
type Local struct {
	info        Info
	store       *capture.Store
	intercept   *intercept.Interceptor
	replays     *replay.Sessions
	focus       *focus.Tracker
	proxyStatus func() (string, error)
	// commands is the command line's registry, set by whoever built it
	// (internal/app) once the objects its commands act on exist. It is
	// atomic because the instance is already being served by then.
	commands atomic.Pointer[cli.Registry]
}

// SetCommands points the instance at the command registry. Until it is
// called — in a process that has none — command-line requests are refused
// rather than half-answered.
func (l *Local) SetCommands(r *cli.Registry) { l.commands.Store(r) }

// RunCommand runs one command-line invocation against this process.
func (l *Local) RunCommand(ctx context.Context, req cli.Request) (cli.Result, error) {
	reg := l.commands.Load()
	if reg == nil {
		return cli.Result{}, errors.New("this instance has no command line")
	}
	return reg.Run(ctx, req)
}

// NewLocal returns the Instance for this process. proxyStatus reports where
// the proxy listens (or why it doesn't).
func NewLocal(info Info, store *capture.Store, ic *intercept.Interceptor, replays *replay.Sessions,
	tracker *focus.Tracker, proxyStatus func() (string, error)) *Local {
	return &Local{info: info, store: store, intercept: ic, replays: replays, focus: tracker, proxyStatus: proxyStatus}
}

// ID returns the instance ID.
func (l *Local) ID() string { return l.info.ID }

func (l *Local) Info(context.Context) (Info, error) {
	i := l.info
	if l.proxyStatus != nil {
		addr, err := l.proxyStatus()
		i.ProxyAddr = addr
		if err != nil {
			i.ProxyErr = err.Error()
		}
	}
	return i, nil
}

func (l *Local) ListFlows(_ context.Context, q FlowQuery) (FlowPage, error) {
	m, err := filter.Compile(q.Filter, q.Regex)
	if err != nil {
		return FlowPage{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)

	flows := l.store.List()
	var only map[uint64]bool
	if len(q.IDs) > 0 {
		only = make(map[uint64]bool, len(q.IDs))
		for _, id := range q.IDs {
			only[id] = true
		}
	}
	page := FlowPage{Total: len(flows), Flows: []FlowSummary{}}
	visit := func(f capture.Flow) {
		if only != nil && !only[f.ID] {
			return
		}
		switch q.Protocol {
		case ProtocolHTTP:
			if f.IsDNS() {
				return
			}
		case ProtocolDNS:
			if !f.IsDNS() {
				return
			}
		}
		if !matches(m, q.In, f) {
			return
		}
		page.Matched++
		switch {
		case q.After != 0 && !q.Newest && f.ID <= q.After,
			q.After != 0 && q.Newest && f.ID >= q.After:
			return
		case len(page.Flows) >= limit:
			page.More = true
			return
		}
		page.Flows = append(page.Flows, Summarize(f))
	}
	if q.Newest {
		for i := len(flows) - 1; i >= 0; i-- {
			visit(flows[i])
		}
	} else {
		for _, f := range flows {
			visit(f)
		}
	}
	return page, nil
}

// matches applies a filter to the part of a flow selected by in.
func matches(m *filter.Matcher, in string, f capture.Flow) bool {
	if m.MatchesAll() {
		return true
	}
	switch in {
	case InHeaders:
		return m.Match(headOf(f.Request.Raw)) || m.Match(headOf(f.Response.Raw))
	case InBody:
		return m.Match(bodyOf(f.Request.Raw)) || m.Match(bodyOf(f.Response.Raw))
	case InAll:
		return m.Match(string(f.Request.Raw)) || m.Match(string(f.Response.Raw))
	}
	return m.Match(f.SearchText())
}

func headOf(raw []byte) string {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return string(raw[:i])
	}
	return string(raw)
}

func bodyOf(raw []byte) string {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return string(raw[i+4:])
	}
	return ""
}

func (l *Local) GetFlow(_ context.Context, id uint64) (capture.Flow, error) {
	f, ok := l.store.Get(id)
	if !ok {
		return capture.Flow{}, ErrNoFlow
	}
	return f, nil
}

func (l *Local) Intercept(context.Context) (InterceptState, error) {
	st := InterceptState{Enabled: l.intercept.Enabled(), Held: []Held{}}
	for _, h := range l.intercept.Held() {
		st.Held = append(st.Held, l.describeHeld(h))
	}
	return st, nil
}

func (l *Local) describeHeld(h intercept.HeldMessage) Held {
	out := Held{FlowID: h.FlowID, Direction: h.Dir.String(), Size: len(h.Raw)}
	if f, ok := l.store.Get(h.FlowID); ok {
		out.Method, out.URL = f.Method, f.URL()
	}
	return out
}

func (l *Local) SetIntercept(_ context.Context, enabled bool) error {
	l.intercept.SetEnabled(enabled)
	return nil
}

func (l *Local) WaitHeld(ctx context.Context, timeout time.Duration) (Held, bool, error) {
	// Subscribe before looking, so a hold between the two isn't missed.
	arrived := make(chan struct{}, 1)
	unsub := l.intercept.Subscribe(func(e intercept.HeldEvent) {
		if !e.Released {
			select {
			case arrived <- struct{}{}:
			default:
			}
		}
	})
	defer unsub()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if held := l.intercept.Held(); len(held) > 0 {
			return l.describeHeld(held[0]), true, nil
		}
		select {
		case <-arrived:
		case <-timer.C:
			return Held{}, false, nil
		case <-ctx.Done():
			return Held{}, false, ctx.Err()
		}
	}
}

func (l *Local) Forward(_ context.Context, flowID uint64, edited *string) error {
	return mapHeldErr(l.intercept.Forward(flowID, edited))
}

func (l *Local) Drop(_ context.Context, flowID uint64) error {
	return mapHeldErr(l.intercept.Drop(flowID))
}

func mapHeldErr(err error) error {
	if errors.Is(err, intercept.ErrNotHeld) {
		return ErrNotHeld
	}
	return err
}

func (l *Local) ReplayCreate(_ context.Context, owner replay.Owner, flowID uint64, addr string) (replay.Session, error) {
	f, ok := l.store.Get(flowID)
	if !ok {
		return replay.Session{}, ErrNoFlow
	}
	if f.IsDNS() {
		return replay.Session{}, fmt.Errorf("flow %d is a DNS lookup, which can't be replayed", flowID)
	}
	t := replay.TemplateFrom(f)
	if addr != "" {
		t.Addr = addr
	}
	return l.replays.Create(owner, t), nil
}

func (l *Local) ReplaySend(ctx context.Context, sessionID uint64, raw []byte, by replay.Owner) (replay.Entry, error) {
	e, err := l.replays.Send(ctx, sessionID, raw, by)
	if errors.Is(err, replay.ErrNoSession) {
		return e, ErrNoSession
	}
	// A failed exchange is a result (recorded in e.Err), not a call failure.
	return e, nil
}

func (l *Local) ReplayGet(_ context.Context, sessionID uint64) (replay.Session, error) {
	s, ok := l.replays.Get(sessionID)
	if !ok {
		return replay.Session{}, ErrNoSession
	}
	return s, nil
}

func (l *Local) ReplayList(context.Context) ([]replay.Session, error) {
	return l.replays.List(), nil
}

func (l *Local) Focus(_ context.Context, at time.Time) (focus.Snapshot, error) {
	return l.focus.At(at)
}
