package plugin

import (
	"sort"
	"sync/atomic"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/manifest"
)

// hookIndex numbers the catalog, so a set of hooks is a bit mask.
var hookIndex = func() map[manifest.Hook]int {
	m := make(map[manifest.Hook]int, len(manifest.Catalog))
	for i, h := range manifest.Catalog {
		m[h.Hook] = i
	}
	return m
}()

func bit(h manifest.Hook) uint64 { return 1 << hookIndex[h] }

// route is one target of one running plugin.
type route struct {
	p      *Running
	fn     string
	m      *manifest.Matcher
	self   bool
	hold   bool // core holds what the filter accepts for this route
	req    bool // the hook is about the request: filter on its content type
	prio   int
	millis int
}

// routing is who wants what: immutable, rebuilt whenever a plugin starts or
// stops, and swapped in whole.
type routing struct {
	mask   uint64 // every hook some route is on
	byHook [64][]route
	// holds are the intercept routes with hold set, by direction.
	holdReq, holdResp []route
	// holdWS are the websocket.on_message_pending routes with hold set.
	holdWS []route
}

// Dispatcher turns what happens in core into plugin calls. Its subscribers
// run on whichever goroutine made the change — for traffic, the proxy's —
// and do nothing but match and enqueue.
type Dispatcher struct {
	routes    atomic.Pointer[routing]
	noTargets func() bool
	// allEntities sends every entity's created and updated hooks, not just
	// those of entities in a target (see SetAllEntities).
	allEntities atomic.Bool
}

// SetAllEntities says whether the entity created and updated hooks run for
// every entity (config.Targets.AllEntityHooks) or only for those that
// belong to a target (entities.Event.InTarget), which is the default.
// Deleted hooks always run. While no target is set up, every entity is
// in scope regardless.
func (d *Dispatcher) SetAllEntities(on bool) { d.allEntities.Store(on) }

// newDispatcher returns a dispatcher with no routes. noTargets reports
// whether no targets are configured (everything is then in scope).
func newDispatcher(noTargets func() bool) *Dispatcher {
	return &Dispatcher{noTargets: noTargets}
}

// set replaces the routes from the running plugins.
func (d *Dispatcher) set(plugins []*Running) {
	rt := &routing{}
	for _, p := range plugins {
		for _, t := range p.man.Targets {
			info, ok := manifest.Lookup(t.Hook)
			if !ok {
				continue
			}
			m, err := t.Match.Compile(info)
			if err != nil {
				continue // validated at load; can't happen
			}
			i := hookIndex[t.Hook]
			rt.byHook[i] = append(rt.byHook[i], route{
				p: p, fn: t.Function, m: m, self: t.Self, hold: t.Hold, req: info.Request,
				prio: t.Priority, millis: t.Timeout(),
			})
			rt.mask |= 1 << i
			if t.Hold {
				r := rt.byHook[i][len(rt.byHook[i])-1]
				switch t.Hook {
				case manifest.InterceptOnRequestPending:
					rt.holdReq = append(rt.holdReq, r)
				case manifest.InterceptOnResponsePending:
					rt.holdResp = append(rt.holdResp, r)
				case manifest.WebSocketOnMessagePending:
					rt.holdWS = append(rt.holdWS, r)
				}
			}
		}
	}
	for i := range rt.byHook {
		routes := rt.byHook[i]
		sort.SliceStable(routes, func(a, b int) bool {
			if routes[a].prio != routes[b].prio {
				return routes[a].prio < routes[b].prio
			}
			return routes[a].p.src.Name < routes[b].p.src.Name
		})
	}
	if rt.mask == 0 {
		d.routes.Store(nil)
		return
	}
	d.routes.Store(rt)
}

// holdFunc is the Interceptor's PluginHoldFunc for the current routes, or
// nil when no route asks to hold. It reads the routes at each call, so it
// follows plugins starting and stopping without being reinstalled.
func (d *Dispatcher) holdFunc() intercept.PluginHoldFunc {
	rt := d.routes.Load()
	if rt == nil || len(rt.holdReq)+len(rt.holdResp) == 0 {
		return nil
	}
	return d.wantsHold
}

func (d *Dispatcher) wantsHold(dir intercept.Direction, s intercept.Subject) bool {
	rt := d.routes.Load()
	if rt == nil {
		return false
	}
	routes := rt.holdReq
	if dir == intercept.Response {
		routes = rt.holdResp
	}
	if len(routes) == 0 {
		return false
	}
	origin, _ := OriginOf(s.Origin)
	facts := manifest.Facts{
		Source: sourceName(capture.Source(s.Source)), TargetID: s.TargetID, Method: s.Method,
		Status: s.Status, Host: s.Host, Path: s.Path, ContentType: s.ContentType,
	}
	if s.TargetID == "" {
		facts.NoTargets = d.noTargets()
	}
	for i := range routes {
		if origin != routes[i].p.src.Name && routes[i].m.Match(&facts) {
			return true
		}
	}
	return false
}

// wants reports whether any route is on a hook.
func (d *Dispatcher) wants(h manifest.Hook) bool {
	rt := d.routes.Load()
	return rt != nil && rt.mask&bit(h) != 0
}

// flowHooks are the hooks a capture event means, from the flow's state
// before and after.
func flowHooks(e capture.Event) uint64 {
	f := &e.Flow
	switch e.Kind {
	case capture.FlowAdded:
		switch f.State {
		case capture.StatePending: // streamed: forwarded as it was received
			return bit(manifest.TrafficOnRequestReceived) | bit(manifest.TrafficOnRequestForwarded)
		case capture.StateHeldRequest:
			return bit(manifest.TrafficOnRequestReceived)
		}
	case capture.FlowUpdated:
		if e.Prev == f.State {
			return 0
		}
		var m uint64
		switch f.State {
		case capture.StatePending:
			if e.Prev == capture.StateHeldRequest {
				m = bit(manifest.TrafficOnRequestForwarded)
			}
		case capture.StateHeldResponse:
			m = bit(manifest.TrafficOnResponseReceived)
		case capture.StateComplete:
			m = bit(manifest.TrafficOnResponseForwarded) | bit(manifest.TrafficOnFlowComplete)
			if e.Prev != capture.StateHeldResponse {
				m |= bit(manifest.TrafficOnResponseReceived)
			}
			if sentByReplay(f.Source) {
				m |= bit(manifest.ReplayOnSent)
			}
		case capture.StateFailed:
			m = bit(manifest.TrafficOnFlowFailed)
			if sentByReplay(f.Source) {
				m |= bit(manifest.ReplayOnSent)
			}
		case capture.StateDropped:
			m = bit(manifest.TrafficOnFlowDropped)
		}
		return m
	}
	return 0
}

// OnFlow is the capture.Store subscriber. With nothing routed to traffic it
// costs one atomic load.
func (d *Dispatcher) OnFlow(e capture.Event) {
	rt := d.routes.Load()
	if rt == nil || e.Flow.IsDNS() { // traffic hooks are about HTTP messages
		return
	}
	mask := flowHooks(e) & rt.mask
	if mask == 0 {
		return
	}
	f := &e.Flow
	origin, depth := OriginOf(f.Origin)
	facts := manifest.Facts{
		Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
		Status: f.StatusCode, Host: f.Host, Path: f.Path,
	}
	if f.TargetID == "" {
		facts.NoTargets = d.noTargets()
	}
	var flow *capture.Flow // copied once, for every plugin that wants it
	for i := 0; mask != 0; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		mask &^= 1 << i
		hook := manifest.Catalog[i].Hook
		for j := range rt.byHook[i] {
			r := &rt.byHook[i][j]
			if origin == r.p.src.Name && !r.self {
				continue // the plugin's own traffic
			}
			if r.req {
				facts.Head = f.Request.Raw
			} else {
				facts.Head = f.Response.Raw
			}
			if !r.m.Match(&facts) {
				continue
			}
			if flow == nil {
				c := *f
				flow = &c
			}
			ev := flow
			r.p.observe(hook, r.fn, depth, func(vm *goja.Runtime) goja.Value {
				return event(vm, hook, "flow", flowValue(vm, ev))
			})
		}
	}
}

// OnFinding is the findings.Store subscriber.
func (d *Dispatcher) OnFinding(e findings.Event) {
	var hook manifest.Hook
	switch e.Kind {
	case findings.Added:
		hook = manifest.FindingOnCreated
	case findings.Updated:
		hook = manifest.FindingOnUpdated
	case findings.Removed:
		hook = manifest.FindingOnRemoved
	default:
		return
	}
	rt := d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	f := e.Finding
	facts := manifest.Facts{Source: string(f.Source), Types: f.Types}
	for _, r := range rt.byHook[hookIndex[hook]] {
		if f.Source == findings.SourcePlugin && f.Author == r.p.src.Name {
			continue // what the plugin recorded itself
		}
		if r.m.Match(&facts) {
			r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
				return event(vm, hook, "finding", findingValue(vm, f))
			})
		}
	}
}

// fileHooks are the lifecycle hooks of a recovered file, by event:
// created, updated, deleted.
var fileHooks = [3]manifest.Hook{manifest.FileOnCreated, manifest.FileOnUpdated, manifest.FileOnDeleted}

// OnFile is the files.Store subscriber: a file the work recovered became a
// plugin event. Nothing here is on the proxy's goroutine — files are
// recovered at a human's, an agent's or a plugin's pace — but it costs one
// atomic load when no plugin targets files, like the rest.
//
// A recovery of thousands of files raises thousands of events, more than a
// plugin's queue holds, so the files are in the store before the first one
// is sent and the batch's own hook follows them: a plugin that fell behind
// reads the whole recovery with files.list({batch}) instead.
func (d *Dispatcher) OnFile(e files.Event) {
	var hook manifest.Hook
	switch e.Kind {
	case files.Added:
		hook = fileHooks[0]
	case files.Updated:
		hook = fileHooks[1]
	case files.Removed:
		hook = fileHooks[2]
	case files.BatchDone:
		d.onFileBatch(e.Batch)
		return
	default:
		return // Reset: the engagement changed, which has its own hooks
	}
	rt := d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	f := e.File
	facts := manifest.Facts{
		Source: string(f.Source), Host: f.Host, Path: f.Path,
		ContentType: f.MediaType, Types: []string{f.Kind},
	}
	for _, r := range rt.byHook[hookIndex[hook]] {
		if recoveredBy(f.Source, f.Author, r.p.src.Name) && !r.self {
			continue // what the plugin recovered itself
		}
		if r.m.Match(&facts) {
			r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
				return event(vm, hook, "file", fileValue(vm, f))
			})
		}
	}
}

// onFileBatch sends file.on_batch_complete: one event for a whole recovery.
// The filter's path and content type say nothing about a batch, so only the
// source, the author, the host and the kind are matched — a filter on the
// path still picks the per-file events.
func (d *Dispatcher) onFileBatch(b files.Batch) {
	hook := manifest.FileOnBatchComplete
	rt := d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	facts := manifest.Facts{Source: string(b.Source), Host: b.Host, Types: []string{b.Kind}}
	for _, r := range rt.byHook[hookIndex[hook]] {
		if recoveredBy(b.Source, b.Author, r.p.src.Name) && !r.self {
			continue
		}
		// A batch has no path or media type of its own, so a filter on
		// either can only be about the files, not the recovery.
		if r.m.WantsFileBatch(&facts) {
			r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
				return event(vm, hook, "batch", batchValue(vm, b))
			})
		}
	}
}

// recoveredBy reports whether a plugin recovered this file itself, so it
// isn't told about its own work unless its target asks for it (self).
func recoveredBy(source files.Source, author, plugin string) bool {
	return source == files.SourcePlugin && author == plugin
}

// entityHooks are the lifecycle hooks of each type of entity, by event:
// created, updated, deleted.
var entityHooks = map[entities.Type][3]manifest.Hook{
	entities.TypeDomain: {manifest.DomainOnCreated, manifest.DomainOnUpdated, manifest.DomainOnDeleted},
	entities.TypeHost:   {manifest.HostOnCreated, manifest.HostOnUpdated, manifest.HostOnDeleted},
	entities.TypeURL:    {manifest.URLOnCreated, manifest.URLOnUpdated, manifest.URLOnDeleted},
	"":                  {manifest.EntityOnCreated, manifest.EntityOnUpdated, manifest.EntityOnDeleted},
}

// EntityHooks returns the hooks an entity event calls: its type's, then
// entity's. ok is false for an event that isn't an entity's lifecycle.
func EntityHooks(e entities.Event) (typed, generic manifest.Hook, ok bool) {
	i := map[entities.EventKind]int{entities.Created: 0, entities.Updated: 1, entities.Deleted: 2}
	n, ok := i[e.Kind]
	hooks, known := entityHooks[e.Entity.Type]
	if !ok || !known {
		return "", "", false
	}
	return hooks[n], entityHooks[""][n], true
}

// OnEntity is the entities.Store subscriber.
func (d *Dispatcher) OnEntity(e entities.Event) {
	typed, generic, ok := EntityHooks(e)
	if !ok {
		return
	}
	rt := d.routes.Load()
	if rt == nil || rt.mask&(bit(typed)|bit(generic)) == 0 {
		return
	}
	if !e.InTarget && e.Kind != entities.Deleted && !d.allEntities.Load() && !d.noTargets() {
		return // a related host's, or traffic no target covers
	}
	ent := e.Entity
	facts := manifest.Facts{TargetID: ent.TargetID, Host: ent.Host, Path: ent.Path, Types: []string{string(ent.Type)}}
	if ent.TargetID == "" {
		facts.NoTargets = d.noTargets()
	}
	for _, hook := range [2]manifest.Hook{typed, generic} {
		if rt.mask&bit(hook) == 0 {
			continue
		}
		for _, r := range rt.byHook[hookIndex[hook]] {
			if e.Kind == entities.Created && ent.Origin == entities.PluginOrigin(r.p.src.Name) {
				continue // what the plugin put there itself
			}
			if r.m.Match(&facts) {
				r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
					return event(vm, hook, "entity", entityValue(vm, ent))
				})
			}
		}
	}
}

// OnWSMessage is the capture.Store subscriber for WebSocket messages: it
// delivers websocket.on_message (and, on the first message of a flow it has
// never seen, nothing — on_open is emitted by the WS handler).
func (d *Dispatcher) OnWSMessage(e capture.Event) {
	if e.Kind != capture.WSMessageAdded {
		return
	}
	hook := manifest.WebSocketOnMessage
	rt := d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	f := &e.Flow
	m := e.Message
	origin, _ := OriginOf(f.Origin)
	facts := d.wsFacts(f, m.Dir.String())
	for _, r := range rt.byHook[hookIndex[hook]] {
		if origin == r.p.src.Name && !r.self {
			continue
		}
		if r.m.Match(&facts) {
			msg := m
			flow := f
			r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
				return wsEvent(vm, hook, flow, msg)
			})
		}
	}
}

// wsFacts builds the match facts for a WebSocket hook from its flow and, for
// a message, its direction.
func (d *Dispatcher) wsFacts(f *capture.Flow, direction string) manifest.Facts {
	facts := manifest.Facts{
		Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
		Host: f.Host, Path: f.Path, Direction: direction,
	}
	if f.TargetID == "" {
		facts.NoTargets = d.noTargets()
	}
	return facts
}

// OnHeld is the intercept.Interceptor subscriber.
func (d *Dispatcher) OnHeld(e intercept.HeldEvent) {
	if !e.Released || !d.wants(manifest.InterceptOnReleased) {
		return
	}
	d.Emit(manifest.InterceptOnReleased, map[string]any{"flowId": e.FlowID, "direction": directionName(e.Dir)})
}

// Emit sends an event with no match filter — targets changed, an
// engagement opened — to every plugin targeting the hook.
func (d *Dispatcher) Emit(hook manifest.Hook, payload map[string]any) {
	rt := d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	for _, r := range rt.byHook[hookIndex[hook]] {
		r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
			o := vm.NewObject()
			o.Set("hook", string(hook))
			for k, v := range payload {
				o.Set(k, v)
			}
			return o
		})
	}
}

// event is the object a hook function is called with.
func event(vm *goja.Runtime, hook manifest.Hook, key string, v goja.Value) goja.Value {
	o := vm.NewObject()
	o.Set("hook", string(hook))
	o.Set(key, v)
	return o
}

// sentByReplay reports whether a flow was sent the way replay.on_sent
// covers: from Replay, by an agent or by a plugin. Autocrawl's requests
// are traffic hooks' only.
func sentByReplay(s capture.Source) bool {
	return s != capture.SourceProxy && s != capture.SourceCrawl
}
