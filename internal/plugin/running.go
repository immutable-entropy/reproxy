package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/plugin/jsrt"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/plugin/sdk"
)

// Tuning for a running plugin.
const (
	// queueSize bounds a plugin's queue of observe events. Past it, events
	// are dropped (and counted): the proxy never waits on a plugin.
	queueSize = 1024
	// lifecycleBudget bounds a lifecycle hook, sync part and await alike.
	lifecycleBudget = 2 * time.Second
	// turnBudget bounds one turn of a plugin's JavaScript on its own
	// goroutine: a queued traffic or entity hook, or the continuation of an
	// await. Nothing waits on those — they are observed asynchronously, and
	// a turn that overruns only delays that plugin's own queue — so the
	// budget is there to stop a runaway loop, not to keep traffic moving.
	// jsrt's own 250ms default was too tight for honest work: scanning a
	// 145 KiB HTML body for secrets costs about 250ms in goja, so a plugin
	// doing what it was written to do was being interrupted. The hooks the
	// user does wait on are bounded separately: an intercept hook by its
	// target's timeout_ms (see stage.go), a lifecycle hook by
	// lifecycleBudget.
	turnBudget = 2 * time.Second
	// faultLimit errors within faultWindow mark a plugin faulted: its hooks
	// stop being called until it is reloaded.
	faultLimit  = 5
	faultWindow = time.Minute
	// logLines is how much of a plugin's log is kept for the Settings page.
	logLines = 500
	// maxRequests bounds a plugin's outbound requests in flight.
	maxRequests = 16
)

// Source is where a plugin's files are.
type Source struct {
	Name string
	// FS is the plugin's directory.
	FS fs.FS
	// Path says where that is, for people: a directory.
	Path string
}

// Settings are a plugin's effective setting values, by key, as JSON.
type Settings map[string]json.RawMessage

// LogLine is one line of a plugin's log.
type LogLine struct {
	Time  time.Time
	Level slog.Level
	Msg   string
}

// Stats count what happened to a plugin's events.
type Stats struct {
	Delivered uint64 // hook calls that finished without error
	Dropped   uint64 // events lost because the plugin's queue was full
	Errors    uint64 // hook calls that threw or rejected
	Timeouts  uint64 // hook calls interrupted for running too long
	LastError string
}

// Running is a loaded plugin: its runtime, its exported functions and its
// counters.
type Running struct {
	src  Source
	man  *manifest.Manifest
	deps *Deps
	rt   *jsrt.Runtime

	// exports are the plugin's functions that targets and actions name.
	exports map[string]goja.Callable
	// actions are the manifest's actions with their filters compiled, in
	// the manifest's order.
	actions []compiledAction
	// events is the plugin's queue of observe events.
	events chan func(*jsrt.Runtime)

	// settings is replaced whole when the user changes them.
	settings atomic.Pointer[Settings]

	ctx    context.Context // ends when the plugin unloads
	cancel context.CancelFunc
	sem    chan struct{} // outbound requests in flight

	delivered, dropped, errs, timeouts atomic.Uint64

	mu      sync.Mutex
	lastErr string
	logs    []LogLine
	faults  []time.Time
	faulted bool
	onFault func(error)

	// Owned by the plugin's goroutine.
	depth   int // of the event being handled: see Origin
	regexes map[string]*regexp.Regexp
	storage *fileStorage
}

// loadOptions vary a load for the test runner.
type loadOptions struct {
	// entry replaces main.ts as what is bundled; modules adds virtual
	// modules to the SDK's.
	entry   string
	modules map[string]string
	// globals installs more host functions before the bundle runs.
	globals func(*Running)
}

// load bundles the plugin, starts its runtime and runs its top level. It
// fails if a function a target names isn't exported.
func load(src Source, man *manifest.Manifest, deps *Deps, settings Settings, opts loadOptions) (*Running, error) {
	entry := opts.entry
	if entry == "" {
		entry = manifest.EntryName
	}
	modules := sdk.Modules()
	for k, v := range opts.modules {
		modules[k] = v
	}
	bundle, err := jsrt.Build(src.FS, entry, modules)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Running{
		src:     src,
		man:     man,
		deps:    deps,
		exports: make(map[string]goja.Callable),
		events:  make(chan func(*jsrt.Runtime), queueSize),
		ctx:     ctx,
		cancel:  cancel,
		sem:     make(chan struct{}, maxRequests),
		regexes: make(map[string]*regexp.Regexp),
	}
	r.settings.Store(&settings)
	r.rt = jsrt.New(jsrt.Options{Log: r.logf, Events: r.events, Budget: turnBudget})

	err = r.rt.Do(lifecycleBudget, func(rt *jsrt.Runtime) error {
		r.installHost(rt.VM())
		if opts.globals != nil {
			opts.globals(r)
		}
		if _, err := rt.VM().RunScript(src.Name+".js", bundle.JS); err != nil {
			return jsrt.Unwrap(err)
		}
		exported := rt.VM().Get(jsrt.GlobalName)
		if exported == nil || goja.IsUndefined(exported) {
			return errors.New("the bundle exported nothing")
		}
		obj := exported.ToObject(rt.VM())
		var missing []string
		for _, name := range functionNames(man) {
			fn, ok := goja.AssertFunction(obj.Get(name))
			if !ok {
				missing = append(missing, name)
				continue
			}
			r.exports[name] = fn
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s doesn't export %s", entry, strings.Join(missing, ", "))
		}
		return nil
	})
	if err != nil {
		r.close()
		return nil, err
	}
	for _, a := range man.Actions {
		m, err := a.Match.CompileAction()
		if err != nil {
			r.close()
			return nil, fmt.Errorf("action %s: %w", a.ID, err) // validated at load; can't happen
		}
		r.actions = append(r.actions, compiledAction{Action: a, m: m})
	}
	return r, nil
}

// functionNames are the functions the manifest's targets, actions and
// commands name, each once, in the order they are named.
func functionNames(man *manifest.Manifest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(fn string) {
		if fn != "" && !seen[fn] {
			seen[fn] = true
			out = append(out, fn)
		}
	}
	for _, t := range man.Targets {
		add(t.Function)
	}
	for _, a := range man.Actions {
		add(a.Function)
	}
	for _, c := range man.Commands {
		add(c.Function)
	}
	return out
}

// Name is the plugin's name.
func (r *Running) Name() string { return r.src.Name }

// close stops the runtime and anything the plugin had in flight.
func (r *Running) close() {
	r.cancel()
	r.rt.Close()
}

// observe queues a call of fn for an event. It never blocks: a full queue
// drops the event, and counts it.
func (r *Running) observe(hook manifest.Hook, fn string, depth int, build func(*goja.Runtime) goja.Value) bool {
	task := func(rt *jsrt.Runtime) { r.call(rt, hook, fn, depth, build, nil) }
	select {
	case r.events <- task:
		return true
	default:
		r.dropped.Add(1)
		return false
	}
}

// call runs fn with the event build makes, on the plugin's goroutine, and
// counts how it went once it settles. done, if set, gets the result.
func (r *Running) call(rt *jsrt.Runtime, hook manifest.Hook, fn string, depth int,
	build func(*goja.Runtime) goja.Value, done func(goja.Value, error)) {
	f, ok := r.exports[fn]
	if !ok {
		return
	}
	r.depth = depth
	v, err := rt.Call(f, build(rt.VM()))
	finish := func(v goja.Value, err error) {
		if err != nil {
			r.fail(hook, fn, err)
		} else {
			r.delivered.Add(1)
		}
		if done != nil {
			done(v, err)
		}
	}
	if err != nil {
		finish(nil, err)
		return
	}
	rt.Settle(v, finish)
}

// await calls fn for a lifecycle or decide hook and waits, up to budget in
// all, for its result.
func (r *Running) await(hook manifest.Hook, fn string, depth int, budget time.Duration,
	build func(*goja.Runtime) goja.Value) (any, error) {
	return r.awaitFor(hook, fn, depth, budget, budget, build)
}

// awaitFor is await with the first turn and the whole call bounded apart:
// an action's code gets an ordinary turn, but may wait on the network for
// far longer. It gives up early if the plugin stops.
func (r *Running) awaitFor(hook manifest.Hook, fn string, depth int, turn, total time.Duration,
	build func(*goja.Runtime) goja.Value) (any, error) {
	type result struct {
		v   any
		err error
	}
	res := make(chan result, 1)
	deadline := time.Now().Add(total)
	err := r.rt.Do(turn, func(rt *jsrt.Runtime) error {
		r.call(rt, hook, fn, depth, build, func(v goja.Value, err error) {
			var out any
			if v != nil && err == nil {
				out = v.Export()
			}
			res <- result{out, err}
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	select {
	case got := <-res:
		return got.v, got.err
	case <-r.ctx.Done():
		return nil, fmt.Errorf("%s: the plugin stopped before it answered", fn)
	case <-time.After(time.Until(deadline)):
		err := fmt.Errorf("%s: no answer within %s", fn, total)
		r.fail(hook, fn, fmt.Errorf("%w: %v", jsrt.ErrBudget, err))
		return nil, err
	}
}

// lifecycle calls every function the plugin maps to a lifecycle hook.
func (r *Running) lifecycle(hook manifest.Hook, payload map[string]any) {
	for _, t := range r.man.Targets {
		if t.Hook != hook {
			continue
		}
		_, _ = r.await(hook, t.Function, 0, lifecycleBudget, func(vm *goja.Runtime) goja.Value {
			return vm.ToValue(payload)
		})
	}
}

// fail counts a hook call that went wrong, logs it, and marks the plugin
// faulted when it keeps happening.
func (r *Running) fail(hook manifest.Hook, fn string, err error) {
	if errors.Is(err, jsrt.ErrBudget) {
		r.timeouts.Add(1)
	} else {
		r.errs.Add(1)
	}
	msg := fmt.Sprintf("%s (%s): %v", fn, hook, err)
	r.logf(slog.LevelError, msg)

	now := time.Now()
	r.mu.Lock()
	r.lastErr = msg
	kept := r.faults[:0]
	for _, t := range r.faults {
		if now.Sub(t) < faultWindow {
			kept = append(kept, t)
		}
	}
	r.faults = append(kept, now)
	trip := len(r.faults) >= faultLimit && !r.faulted && r.onFault != nil
	if trip {
		r.faulted = true
	}
	onFault := r.onFault
	r.mu.Unlock()
	if trip {
		go onFault(fmt.Errorf("%d errors within %s; last: %s", faultLimit, faultWindow, msg))
	}
}

// logf records a line of the plugin's log, and passes it to the app's.
func (r *Running) logf(level slog.Level, msg string) {
	r.mu.Lock()
	r.logs = append(r.logs, LogLine{Time: time.Now(), Level: level, Msg: msg})
	if n := len(r.logs) - logLines; n > 0 {
		r.logs = append(r.logs[:0:0], r.logs[n:]...)
	}
	r.mu.Unlock()
	r.deps.log().Log(context.Background(), level, msg, "plugin", r.src.Name)
}

// Log returns the plugin's recent log, oldest first.
func (r *Running) Log() []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LogLine(nil), r.logs...)
}

// Stats returns the plugin's counters.
func (r *Running) Stats() Stats {
	r.mu.Lock()
	last := r.lastErr
	r.mu.Unlock()
	return Stats{
		Delivered: r.delivered.Load(), Dropped: r.dropped.Load(),
		Errors: r.errs.Load(), Timeouts: r.timeouts.Load(), LastError: last,
	}
}

// setting returns one effective setting value.
func (r *Running) setting(key string) (json.RawMessage, bool) {
	s := *r.settings.Load()
	v, ok := s[key]
	return v, ok
}

// ---- Origins ------------------------------------------------------------------

// labelPrefix starts the SOCKS5 username a plugin's connection through the
// proxy authenticates with: "reproxy-plugin/<name>/<depth>".
const labelPrefix = "reproxy-plugin/"

// OriginLabel is the label a plugin's connection through the proxy carries.
func OriginLabel(name string, depth int) string {
	return labelPrefix + name + "/" + strconv.Itoa(depth)
}

// ParseLabel reads a connection label back into the Origin its flows are
// recorded with: the plugin's name, with "#<depth>" past the first level.
func ParseLabel(label string) (origin string, ok bool) {
	rest, ok := strings.CutPrefix(label, labelPrefix)
	if !ok {
		return "", false
	}
	name, d, ok := strings.Cut(rest, "/")
	depth, err := strconv.Atoi(d)
	if !ok || err != nil || depth < 1 || name == "" {
		return "", false
	}
	if depth == 1 {
		return name, true
	}
	return name + "#" + d, true
}

// OriginOf splits a flow's Origin into the plugin that sent it and the depth
// it was sent at: 0 for traffic no plugin sent.
func OriginOf(origin string) (name string, depth int) {
	if origin == "" {
		return "", 0
	}
	name, d, ok := strings.Cut(origin, "#")
	if !ok {
		return name, 1
	}
	depth, err := strconv.Atoi(d)
	if err != nil {
		return name, 1
	}
	return name, depth
}
