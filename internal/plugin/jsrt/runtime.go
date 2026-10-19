package jsrt

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// DefaultBudget is how long one turn of a plugin's code may run before it
// is interrupted: a hook call, a timer, the continuation of an await. Time
// spent waiting on a host call (a request in flight) doesn't count; only
// JavaScript running does.
const DefaultBudget = 250 * time.Millisecond

// maxInflight is how many handlers may be awaiting at once before the event
// lane stops taking more, so a plugin that awaits slow requests applies
// back-pressure to its queue (and drops) rather than piling up promises.
const maxInflight = 8

// ErrBudget is the error a turn that ran past its budget fails with.
var ErrBudget = errors.New("ran past its time budget")

// ErrClosed is returned for work offered to a runtime that has stopped.
var ErrClosed = errors.New("the plugin's runtime has stopped")

// Options configure a Runtime.
type Options struct {
	// Log receives console output and unhandled rejections.
	Log func(level slog.Level, msg string)
	// Budget overrides DefaultBudget.
	Budget time.Duration
	// Events is the lane of queued events, if any: work offered by the
	// dispatcher, taken only while fewer than maxInflight handlers are
	// awaiting. Its owner closes it, or leaves it, when the runtime stops.
	Events <-chan func(*Runtime)
}

// Runtime is one goja runtime and the goroutine that owns it. Everything
// that touches the JavaScript runs on that goroutine, in turns: urgent work
// (lifecycle hooks, intercept decisions) first, then callbacks (timers, host
// calls completing), then queued events.
type Runtime struct {
	vm     *goja.Runtime
	log    func(slog.Level, string)
	budget time.Duration

	urgent    chan func()
	callbacks chan func()
	events    <-chan func(*Runtime)

	mu       sync.Mutex
	closed   bool
	overflow []func() // callbacks that found the channel full

	quit chan struct{}
	done chan struct{}

	// Owned by the loop goroutine.
	timers    map[int64]*time.Timer
	nextTimer int64
	inflight  int
}

// New starts a runtime with the standard globals installed.
func New(opts Options) *Runtime {
	r := &Runtime{
		vm:        goja.New(),
		log:       opts.Log,
		budget:    opts.Budget,
		urgent:    make(chan func()),
		callbacks: make(chan func(), 256),
		events:    opts.Events,
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		timers:    make(map[int64]*time.Timer),
	}
	if r.log == nil {
		r.log = func(slog.Level, string) {}
	}
	if r.budget <= 0 {
		r.budget = DefaultBudget
	}
	r.vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	r.vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		if op == goja.PromiseRejectionReject {
			r.log(slog.LevelWarn, "unhandled promise rejection: "+describe(p.Result()))
		}
	})
	r.installGlobals()
	go r.loop()
	return r
}

// VM is the runtime's goja instance. Use it only from code the runtime runs
// (Do, Post, an event, a host function).
func (r *Runtime) VM() *goja.Runtime { return r.vm }

// Do runs fn on the runtime's goroutine, ahead of anything queued, and
// waits for it. budget bounds the JavaScript it runs (0: the default).
func (r *Runtime) Do(budget time.Duration, fn func(*Runtime) error) error {
	errc := make(chan error, 1)
	task := func() { errc <- r.guard(budget, func() error { return fn(r) }) }
	select {
	case r.urgent <- task:
	case <-r.quit:
		return ErrClosed
	}
	select {
	case err := <-errc:
		return err
	case <-r.done:
		return ErrClosed
	}
}

// Post queues fn to run on the runtime's goroutine, as the completion of a
// host call or a timer does. It never blocks; it is dropped if the runtime
// has stopped.
func (r *Runtime) Post(fn func(*Runtime)) {
	task := func() { r.guard(0, func() error { fn(r); return nil }) }
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if len(r.overflow) == 0 {
		select {
		case r.callbacks <- task:
			return
		default:
		}
	}
	r.overflow = append(r.overflow, task)
}

// Close stops the runtime: whatever is running is interrupted, timers are
// cancelled, and later work is refused.
func (r *Runtime) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.overflow = nil
	r.mu.Unlock()
	close(r.quit)
	r.vm.Interrupt(ErrClosed)
	<-r.done
}

func (r *Runtime) loop() {
	defer close(r.done)
	defer func() {
		for _, t := range r.timers {
			t.Stop()
		}
	}()
	for {
		r.drainOverflow()
		// Urgent work first, then callbacks, then events when there is room.
		select {
		case task := <-r.urgent:
			task()
			continue
		case <-r.quit:
			return
		default:
		}
		select {
		case task := <-r.callbacks:
			task()
			continue
		default:
		}
		events := r.events
		if r.inflight >= maxInflight {
			events = nil // a nil channel is never ready
		}
		select {
		case task := <-r.urgent:
			task()
		case task := <-r.callbacks:
			task()
		case ev, ok := <-events:
			if !ok {
				r.events = nil
				continue
			}
			r.guard(0, func() error { ev(r); return nil })
		case <-r.quit:
			return
		}
	}
}

func (r *Runtime) drainOverflow() {
	r.mu.Lock()
	for len(r.overflow) > 0 {
		select {
		case r.callbacks <- r.overflow[0]:
			r.overflow = r.overflow[1:]
			continue
		default:
		}
		break
	}
	r.mu.Unlock()
}

// guard runs one turn: under a time budget, with a panic in it reported as
// an error rather than taking the process down.
func (r *Runtime) guard(budget time.Duration, fn func() error) (err error) {
	if budget <= 0 {
		budget = r.budget
	}
	// The budget is named in the interrupt, so a plugin's log says how
	// long its turn was given, not just that it ran out.
	t := time.AfterFunc(budget, func() { r.vm.Interrupt(fmt.Errorf("%w of %s", ErrBudget, budget)) })
	defer func() {
		t.Stop()
		r.vm.ClearInterrupt()
		if p := recover(); p != nil {
			err = fmt.Errorf("plugin code panicked: %v", p)
			r.log(slog.LevelError, err.Error())
		}
	}()
	return Unwrap(fn())
}

// Unwrap turns goja's errors into plain ones: an exception keeps its
// message and stack, an interrupt becomes the reason given for it.
func Unwrap(err error) error {
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		if v, ok := ie.Value().(error); ok {
			return v
		}
		return errors.New(ie.Error())
	}
	return err
}

// Call calls a JavaScript function. Use it from code the runtime runs.
func (r *Runtime) Call(fn goja.Callable, args ...any) (goja.Value, error) {
	vals := make([]goja.Value, len(args))
	for i, a := range args {
		if v, ok := a.(goja.Value); ok {
			vals[i] = v
		} else {
			vals[i] = r.vm.ToValue(a)
		}
	}
	v, err := fn(goja.Undefined(), vals...)
	return v, Unwrap(err)
}

// Settle calls then with v's value once it is known: at once for a plain
// value, when it settles for a promise. A pending promise counts against the
// handlers in flight until then. Use it from code the runtime runs.
func (r *Runtime) Settle(v goja.Value, then func(goja.Value, error)) {
	p, ok := promiseOf(v)
	if !ok {
		then(v, nil)
		return
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
		then(p.Result(), nil)
		return
	case goja.PromiseStateRejected:
		then(nil, Rejection(p.Result()))
		return
	}
	r.inflight++
	done := false
	settle := func(val goja.Value, err error) {
		if done {
			return
		}
		done = true
		r.inflight--
		then(val, err)
	}
	thenFn, _ := goja.AssertFunction(v.ToObject(r.vm).Get("then"))
	_, err := thenFn(v,
		r.vm.ToValue(func(c goja.FunctionCall) goja.Value { settle(c.Argument(0), nil); return goja.Undefined() }),
		r.vm.ToValue(func(c goja.FunctionCall) goja.Value { settle(nil, Rejection(c.Argument(0))); return goja.Undefined() }))
	if err != nil {
		settle(nil, Unwrap(err))
	}
}

func promiseOf(v goja.Value) (*goja.Promise, bool) {
	if v == nil {
		return nil, false
	}
	p, ok := v.Export().(*goja.Promise)
	return p, ok
}

// Rejection makes a rejected promise's reason an error.
func Rejection(v goja.Value) error {
	if v == nil {
		return errors.New("rejected")
	}
	if obj, ok := v.(*goja.Object); ok {
		if stack := obj.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			return errors.New(stack.String())
		}
	}
	return errors.New(describe(v))
}

// Promise returns a new promise and functions to settle it from Go: on any
// goroutine, the settling is posted to the runtime's. Use it from code the
// runtime runs.
func (r *Runtime) Promise() (p *goja.Promise, resolve func(any), reject func(error)) {
	p, res, rej := r.vm.NewPromise()
	resolve = func(v any) {
		r.Post(func(rt *Runtime) {
			if f, ok := v.(func(*goja.Runtime) goja.Value); ok {
				v = f(rt.vm) // values built on the runtime's goroutine
			}
			res(v)
		})
	}
	reject = func(err error) {
		r.Post(func(rt *Runtime) { rej(rt.Error(err)) })
	}
	return p, resolve, reject
}

// Error makes a Go error a JavaScript Error, to throw or reject with. Use it
// from code the runtime runs.
func (r *Runtime) Error(err error) *goja.Object {
	o, e := r.vm.New(r.vm.Get("Error"), r.vm.ToValue(err.Error()))
	if e != nil {
		return r.vm.NewGoError(err)
	}
	return o
}

// ---- Globals ------------------------------------------------------------------

// prelude is the part of the globals easiest written in JavaScript.
const prelude = `
(function (g) {
	g.queueMicrotask = function (fn) { Promise.resolve().then(fn); };
	g.structuredClone = function (v) { return v === undefined ? undefined : JSON.parse(JSON.stringify(v)); };
	function TextEncoder() {}
	TextEncoder.prototype.encoding = "utf-8";
	TextEncoder.prototype.encode = function (s) { return new Uint8Array(__host_utf8encode(String(s === undefined ? "" : s))); };
	g.TextEncoder = TextEncoder;
	function TextDecoder(label) {
		if (label !== undefined && String(label).toLowerCase().replace("-", "") !== "utf8") {
			throw new RangeError("only utf-8 is supported");
		}
	}
	TextDecoder.prototype.encoding = "utf-8";
	TextDecoder.prototype.decode = function (b) {
		if (b === undefined) return "";
		if (b instanceof ArrayBuffer) return __host_utf8decode(b);
		return __host_utf8decode(b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength));
	};
	g.TextDecoder = TextDecoder;
})(globalThis);
`

func (r *Runtime) installGlobals() {
	vm := r.vm
	console := vm.NewObject()
	for name, level := range map[string]slog.Level{
		"log": slog.LevelInfo, "info": slog.LevelInfo, "debug": slog.LevelDebug,
		"warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		level := level
		console.Set(name, func(c goja.FunctionCall) goja.Value {
			parts := make([]string, len(c.Arguments))
			for i, a := range c.Arguments {
				parts[i] = r.format(a)
			}
			r.log(level, strings.Join(parts, " "))
			return goja.Undefined()
		})
	}
	vm.Set("console", console)

	vm.Set("setTimeout", func(c goja.FunctionCall) goja.Value { return r.setTimer(c, false) })
	vm.Set("setInterval", func(c goja.FunctionCall) goja.Value { return r.setTimer(c, true) })
	clear := func(c goja.FunctionCall) goja.Value {
		id := c.Argument(0).ToInteger()
		if t, ok := r.timers[id]; ok {
			t.Stop()
			delete(r.timers, id)
		}
		return goja.Undefined()
	}
	vm.Set("clearTimeout", clear)
	vm.Set("clearInterval", clear)

	vm.Set("btoa", func(s string) (string, error) {
		b := make([]byte, 0, len(s))
		for _, c := range s {
			if c > 0xff {
				return "", errors.New("btoa: the string has characters outside Latin-1")
			}
			b = append(b, byte(c))
		}
		return base64.StdEncoding.EncodeToString(b), nil
	})
	vm.Set("atob", func(s string) (string, error) {
		b, err := base64.StdEncoding.DecodeString(strings.TrimRight(s, "=") + strings.Repeat("=", (4-len(strings.TrimRight(s, "="))%4)%4))
		if err != nil {
			return "", errors.New("atob: not valid base64")
		}
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return string(runes), nil
	})
	vm.Set("__host_utf8encode", func(s string) goja.ArrayBuffer { return vm.NewArrayBuffer([]byte(s)) })
	vm.Set("__host_utf8decode", func(b goja.ArrayBuffer) string { return string(b.Bytes()) })
	if _, err := vm.RunString(prelude); err != nil {
		panic(err) // the prelude is ours: a failure is a bug
	}
}

func (r *Runtime) setTimer(c goja.FunctionCall, repeat bool) goja.Value {
	fn, ok := goja.AssertFunction(c.Argument(0))
	if !ok {
		panic(r.vm.NewTypeError("the callback must be a function"))
	}
	delay := time.Duration(c.Argument(1).ToInteger()) * time.Millisecond
	if delay < 0 {
		delay = 0
	}
	if repeat && delay < time.Millisecond {
		delay = time.Millisecond
	}
	args := append([]goja.Value(nil), c.Arguments[min(2, len(c.Arguments)):]...)
	r.nextTimer++
	id := r.nextTimer
	var fire func()
	fire = func() {
		r.Post(func(rt *Runtime) {
			if _, live := rt.timers[id]; !live {
				return
			}
			if repeat {
				rt.timers[id] = time.AfterFunc(delay, fire)
			} else {
				delete(rt.timers, id)
			}
			if _, err := fn(goja.Undefined(), args...); err != nil {
				rt.log(slog.LevelError, "timer: "+Unwrap(err).Error())
			}
		})
	}
	r.timers[id] = time.AfterFunc(delay, fire)
	return r.vm.ToValue(id)
}

// format renders a console argument: strings as they are, errors with
// their stack, everything else as JSON where it can be.
func (r *Runtime) format(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	if s, ok := v.Export().(string); ok {
		return s
	}
	return describe(v)
}

func describe(v goja.Value) string {
	if v == nil {
		return "undefined"
	}
	if obj, ok := v.(*goja.Object); ok {
		if stack := obj.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			return stack.String()
		}
		if b, err := obj.MarshalJSON(); err == nil {
			return string(b)
		}
	}
	return v.String()
}
