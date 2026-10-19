package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/plugin/jsrt"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/replay"
	"reproxy/internal/target"
)

// A plugin's unit tests are its *.test.ts files. Each runs in a runtime of
// its own with the plugin's main.ts, through the same bundler and host as a
// live plugin, against stores the caller provides (package plugintest gives
// them a throwaway engagement). Outbound requests are answered by a script
// rather than sent.

// TestResult is one test's outcome.
type TestResult struct {
	File  string
	Name  string
	OK    bool
	Error string
	Time  time.Duration
}

// TestOptions configure a test run.
type TestOptions struct {
	// Filter runs only the tests whose name matches it.
	Filter string
	// Reset empties the stores before each test.
	Reset func()
	// Timeout bounds a whole file.
	Timeout time.Duration
}

// TestFiles lists a plugin's test files.
func TestFiles(fsys fs.FS) []string {
	var out []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != "." && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".test.ts") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// RunTestFile runs one test file of a plugin. deps.Send is replaced by the
// scripted responder.
func RunTestFile(src Source, man *manifest.Manifest, deps Deps, file string, opts TestOptions) ([]TestResult, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Minute
	}
	mock := &mockNet{}
	deps.Send = mock.send
	// Nothing is dialled (mock.send answers), but a direct send needs a
	// dial function all the same: http.send refuses to go without one.
	deps.Dial = mockDirectDial
	deps.ProxyDial = func(context.Context, string, string, string) (net.Conn, error) {
		return nil, errors.New("mock: not dialled")
	}

	settings := defaultSettings(man)
	const entry = "reproxy:test-entry"
	src.Name = man.Name
	modules := map[string]string{
		entry: fmt.Sprintf("export * from %q;\nimport %q;\n", "./"+manifest.EntryName, "./"+file),
	}
	var r *Running
	r, err := load(src, man, &deps, settings, loadOptions{
		entry:   entry,
		modules: modules,
		globals: func(run *Running) { run.installTestHost(mock, settings, opts) },
	})
	if err != nil {
		return nil, err
	}
	defer r.close()

	type outcome struct {
		results []TestResult
		err     error
	}
	done := make(chan outcome, 1)
	err = r.rt.Do(0, func(rt *jsrt.Runtime) error {
		run, ok := goja.AssertFunction(rt.VM().Get("__reproxy_run"))
		if !ok {
			return errors.New("the test file doesn't import reproxy/testing")
		}
		v, err := rt.Call(run, opts.Filter)
		if err != nil {
			return err
		}
		rt.Settle(v, func(v goja.Value, err error) {
			if err != nil {
				done <- outcome{err: err}
				return
			}
			var raw []struct {
				Name  string  `json:"name"`
				OK    bool    `json:"ok"`
				Error string  `json:"error"`
				MS    float64 `json:"ms"`
			}
			b, _ := json.Marshal(v.Export())
			if err := json.Unmarshal(b, &raw); err != nil {
				done <- outcome{err: err}
				return
			}
			out := make([]TestResult, len(raw))
			for i, t := range raw {
				out[i] = TestResult{File: file, Name: t.Name, OK: t.OK, Error: t.Error, Time: time.Duration(t.MS * float64(time.Millisecond))}
			}
			done <- outcome{results: out}
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	select {
	case o := <-done:
		return o.results, o.err
	case <-time.After(opts.Timeout):
		return nil, fmt.Errorf("%s: timed out after %s", file, opts.Timeout)
	}
}

// defaultSettings is what a plugin's settings are with no user choice.
func defaultSettings(man *manifest.Manifest) Settings {
	m := &Manager{deps: &Deps{}}
	return m.effective(&entry{src: Source{Name: man.Name}, man: man})
}

// installTestHost gives a test runtime __reproxy_test, which the testing
// module's mock wraps.
func (r *Running) installTestHost(mock *mockNet, defaults Settings, opts TestOptions) {
	vm := r.rt.VM()
	h := vm.NewObject()
	h.Set("flow", func(f map[string]any) (goja.Value, error) {
		flow, err := fixtureFlow(f)
		if err != nil {
			return nil, err
		}
		msgs, err := fixtureWSMessages(f["wsMessages"])
		if err != nil {
			return nil, err
		}
		flow.ID = r.deps.Store.Add(flow)
		for _, m := range msgs {
			if _, ok := r.deps.Store.AddWSMessage(flow.ID, m); !ok {
				return nil, fmt.Errorf("mock.flow: the WebSocket message budget is spent")
			}
		}
		if stored, ok := r.deps.Store.Get(flow.ID); ok {
			flow = stored
		}
		return flowValue(vm, &flow), nil
	})
	h.Set("index", func(flowID int64, typ string) (goja.Value, error) {
		e, err := r.testIndex(uint64(flowID), entities.Type(typ))
		if err != nil {
			return nil, err
		}
		return entityValue(vm, e), nil
	})
	dispatch := func(run func(func(int, error)) (int, error)) *goja.Promise {
		p, resolve, reject := vm.NewPromise()
		n, err := run(func(count int, err error) {
			if err != nil {
				reject(r.rt.Error(err))
			} else {
				resolve(count)
			}
		})
		if err != nil {
			reject(r.rt.Error(err))
		} else if n == 0 {
			resolve(0)
		}
		return p
	}
	h.Set("dispatch", func(hook string, flowID int64) *goja.Promise {
		return dispatch(func(done func(int, error)) (int, error) {
			return r.testDispatch(manifest.Hook(hook), uint64(flowID), done)
		})
	})
	h.Set("action", func(id string, flowID int64) *goja.Promise {
		p, resolve, reject := vm.NewPromise()
		result := func(ran bool, msg string) goja.Value {
			o := vm.NewObject()
			o.Set("ran", ran)
			o.Set("message", msg)
			return o
		}
		a, ok := r.action(id)
		if !ok {
			reject(r.rt.Error(fmt.Errorf("mock.action: the plugin has no action %q", id)))
			return p
		}
		f, ok := r.deps.Store.Get(uint64(flowID))
		if !ok {
			reject(r.rt.Error(fmt.Errorf("mock.action: no flow %d", flowID)))
			return p
		}
		noTargets := func() bool { return r.deps.Targets == nil || r.deps.Targets.Set().Empty() }
		if !a.m.Match(flowFacts(&f, noTargets)) {
			resolve(result(false, "")) // core wouldn't offer it for this flow
			return p
		}
		r.call(r.rt, actionHook, a.Function, 0, func(vm *goja.Runtime) goja.Value {
			return actionEvent(vm, a, &f)
		}, func(v goja.Value, err error) {
			if err != nil {
				reject(r.rt.Error(err))
				return
			}
			var out any
			if v != nil {
				out = v.Export()
			}
			resolve(result(true, actionMessage(out)))
		})
		return p
	})
	h.Set("dispatchEntity", func(hook string, entityID int64) *goja.Promise {
		return dispatch(func(done func(int, error)) (int, error) {
			return r.testDispatchEntity(manifest.Hook(hook), uint64(entityID), done)
		})
	})
	// A recovered file put in the store as another recoverer's, so the
	// plugin under test is told about it (a plugin is not called for its
	// own recoveries): mock.file.
	h.Set("file", func(d map[string]any) (goja.Value, error) {
		if r.deps.Files == nil {
			return nil, fmt.Errorf("mock.file: no recovered-file store")
		}
		draft, err := r.fileDraft(d)
		if err != nil {
			return nil, fmt.Errorf("mock.file: %w", err)
		}
		if author, ok := d["author"].(string); ok && author != "" {
			draft.Author = author
		} else {
			draft.Author = "mock" // not this plugin, so its hooks are called
		}
		if source, ok := d["source"].(string); ok && source != "" {
			draft.Source = files.Source(source)
		}
		f, _, err := r.deps.Files.Record(draft)
		if err != nil {
			return nil, fmt.Errorf("mock.file: %w", err)
		}
		return fileValue(vm, f), nil
	})
	h.Set("dispatchFile", func(hook string, id int64) *goja.Promise {
		return dispatch(func(done func(int, error)) (int, error) {
			return r.testDispatchFile(manifest.Hook(hook), uint64(id), done)
		})
	})
	h.Set("dispatchFileBatch", func(hook, batch string) *goja.Promise {
		return dispatch(func(done func(int, error)) (int, error) {
			return r.testDispatchFileBatch(manifest.Hook(hook), batch, done)
		})
	})
	h.Set("target", func(kind, value string) (goja.Value, error) {
		if r.deps.Targets == nil {
			return nil, fmt.Errorf("mock.target: no target store")
		}
		t, err := r.deps.Targets.Create(target.Kind(kind), value)
		if err != nil {
			return nil, fmt.Errorf("mock.target: %w", err)
		}
		return vm.ToValue(targetValue(t)), nil
	})
	h.Set("related", func() []string {
		out := []string{}
		for _, rel := range r.deps.Entities.Related() {
			out = append(out, rel.Host)
		}
		return out
	})
	h.Set("sent", func() []map[string]any { return mock.list() })
	h.Set("respond", func(pattern string, resp map[string]any) error { return mock.script(pattern, resp) })
	h.Set("settings", func(values map[string]any) error {
		next := Settings{}
		for k, v := range *r.settings.Load() {
			next[k] = v
		}
		for k, v := range values {
			s, ok := r.man.Setting(k)
			if !ok {
				return fmt.Errorf("mock.settings: the plugin has no setting %q", k)
			}
			raw, _ := json.Marshal(v)
			if err := s.Check(raw); err != nil {
				return fmt.Errorf("mock.settings: %s: %w", k, err)
			}
			next[k] = raw
		}
		r.settings.Store(&next)
		return nil
	})
	h.Set("reset", func() {
		mock.reset()
		d := defaults
		r.settings.Store(&d)
		if opts.Reset != nil {
			opts.Reset()
		}
	})
	vm.Set("__reproxy_test", h)
}

// testDispatch calls this plugin's targets for a hook, as the Dispatcher
// would for a flow: only those whose match filter accepts it. done is
// called once they have all settled (not at all when none matched: n is 0).
func (r *Running) testDispatch(hook manifest.Hook, flowID uint64, done func(int, error)) (n int, err error) {
	info, ok := manifest.Lookup(hook)
	if !ok {
		return 0, fmt.Errorf("mock.dispatch: no hook %q", hook)
	}
	f, ok := r.deps.Store.Get(flowID)
	if !ok {
		return 0, fmt.Errorf("mock.dispatch: no flow %d", flowID)
	}
	if info.Subject == manifest.SubjectWebSocket {
		return r.testDispatchWS(hook, info, f, done)
	}
	facts := manifest.Facts{
		Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
		Status: f.StatusCode, Host: f.Host, Path: f.Path, Head: f.Response.Raw,
	}
	if info.Request {
		facts.Head = f.Request.Raw
	}
	facts.NoTargets = r.deps.Targets == nil || r.deps.Targets.Set().Empty()
	origin, depth := OriginOf(f.Origin)

	return r.fanout(hook, info, &facts, origin, depth, func(vm *goja.Runtime) goja.Value {
		o := event(vm, hook, "flow", flowValue(vm, &f)).ToObject(vm)
		if info.Class == manifest.Decide {
			msg := f.Request
			if !info.Request {
				msg = f.Response
			}
			o.Set("message", messageValue(vm, msg.Raw, msg.Size, false, ""))
		}
		return o
	}, done)
}

// testDispatchWS calls this plugin's targets for a WebSocket hook, as the
// Dispatcher would. A message hook is dispatched once per message recorded
// on the flow (mock.flow's wsMessages), so a test can put a whole
// conversation in a fixture and drive all of it in one call; on_open and
// on_close are dispatched for the connection itself.
func (r *Running) testDispatchWS(hook manifest.Hook, info manifest.HookInfo, f capture.Flow,
	done func(int, error)) (int, error) {

	origin, depth := OriginOf(f.Origin)
	noTargets := r.deps.Targets == nil || r.deps.Targets.Set().Empty()
	facts := func(direction string) manifest.Facts {
		return manifest.Facts{
			Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
			Host: f.Host, Path: f.Path, Direction: direction, NoTargets: noTargets,
		}
	}
	if hook == manifest.WebSocketOnOpen || hook == manifest.WebSocketOnClose {
		fa := facts("")
		return r.fanout(hook, info, &fa, origin, depth, func(vm *goja.Runtime) goja.Value {
			return wsConnValue(vm, hook, &f)
		}, done)
	}
	if len(f.Messages) == 0 {
		return 0, fmt.Errorf("mock.dispatch: flow %d has no WebSocket messages (see mock.flow's wsMessages)", f.ID)
	}

	// Each message is a fanout of its own, and the caller's done is called
	// once every one of them has settled.
	var (
		mu       sync.Mutex
		total    int
		pending  int
		queued   bool // every message has been dispatched
		reported bool
		firstErr error
	)
	settle := func() {
		if queued && pending == 0 && !reported && total > 0 {
			reported = true
			done(total, firstErr)
		}
	}
	for _, m := range f.Messages {
		fa := facts(m.Dir.String())
		msg := m
		n, err := r.fanout(hook, info, &fa, origin, depth, func(vm *goja.Runtime) goja.Value {
			return wsEvent(vm, hook, &f, msg)
		}, func(count int, err error) {
			mu.Lock()
			defer mu.Unlock()
			pending--
			if err != nil && firstErr == nil {
				firstErr = err
			}
			settle()
		})
		if err != nil {
			return 0, err
		}
		if n > 0 {
			mu.Lock()
			total += n
			pending++
			mu.Unlock()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	queued = true
	settle()
	return total, nil
}

// fixtureWSMessages reads the WebSocket messages of a flow fixture.
func fixtureWSMessages(v any) ([]capture.WSMessage, error) {
	list, ok := v.([]any)
	if v == nil || !ok {
		if v != nil {
			return nil, fmt.Errorf("mock.flow: wsMessages must be an array")
		}
		return nil, nil
	}
	out := make([]capture.WSMessage, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mock.flow: each WebSocket message must be an object")
		}
		msg := capture.WSMessage{At: time.Now()}
		switch dir, _ := m["direction"].(string); dir {
		case "", "sent":
		case "received":
			msg.Dir = capture.Received
		default:
			return nil, fmt.Errorf("mock.flow: WebSocket direction %q must be sent or received", dir)
		}
		switch kind, _ := m["type"].(string); kind {
		case "", "text":
		case "binary":
			msg.Type = capture.WSBinary
		default:
			return nil, fmt.Errorf("mock.flow: WebSocket message type %q must be text or binary", kind)
		}
		if text, ok := m["text"].(string); ok {
			msg.Data = []byte(text)
		}
		out = append(out, msg)
	}
	return out, nil
}

// testIndex puts a flow in the Entity Explorer as the Indexer does — the
// visit it is, and the URLs its response links to — and returns the entity
// of the given type the flow itself reached: its URL (the default), host or
// domain. It is what mock.entity builds on, so a test of an entity hook
// starts from traffic rather than a hand-made entity.
func (r *Running) testIndex(flowID uint64, typ entities.Type) (entities.Entity, error) {
	f, ok := r.deps.Store.Get(flowID)
	if !ok {
		return entities.Entity{}, fmt.Errorf("mock.entity: no flow %d", flowID)
	}
	visit, links, ok := entities.Parse(f)
	if !ok {
		return entities.Entity{}, fmt.Errorf("mock.entity: flow %d can't be put in the Explorer", flowID)
	}
	if _, err := r.deps.Entities.Apply(entities.Batch{Visits: []entities.Visit{visit}, Discoveries: links}); err != nil {
		return entities.Entity{}, fmt.Errorf("mock.entity: %w", err)
	}
	e, ok := r.deps.Entities.Get(visit.Address.Key())
	for ok && typ != "" && e.Type != typ {
		e, ok = r.deps.Entities.Get(e.Parent)
	}
	if !ok {
		return entities.Entity{}, fmt.Errorf("mock.entity: flow %d reached no %s", flowID, typ)
	}
	return e, nil
}

// testDispatchEntity calls this plugin's targets for an entity hook, as
// the Dispatcher would: only those whose match filter accepts the entity.
func (r *Running) testDispatchEntity(hook manifest.Hook, id uint64, done func(int, error)) (n int, err error) {
	info, ok := manifest.Lookup(hook)
	if !ok {
		return 0, fmt.Errorf("mock.dispatch: no hook %q", hook)
	}
	if info.Subject != manifest.SubjectEntity {
		return 0, fmt.Errorf("mock.dispatch: %q is not an entity hook", hook)
	}
	var ent entities.Entity
	found := false
	for _, e := range r.deps.Entities.List() {
		if e.ID == id {
			ent, found = e, true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("mock.dispatch: no entity %d", id)
	}
	if prefix, _, _ := strings.Cut(string(hook), "."); prefix != "entity" && prefix != string(ent.Type) {
		return 0, fmt.Errorf("mock.dispatch: %q is not a hook for a %s", hook, ent.Type)
	}
	facts := manifest.Facts{TargetID: ent.TargetID, Host: ent.Host, Path: ent.Path, Types: []string{string(ent.Type)}}
	if ent.TargetID == "" {
		facts.NoTargets = r.deps.Targets == nil || r.deps.Targets.Set().Empty()
	}
	return r.fanout(hook, info, &facts, "", 0, func(vm *goja.Runtime) goja.Value {
		return event(vm, hook, "entity", entityValue(vm, ent))
	}, done)
}

// testDispatchFile calls this plugin's targets for a file hook, as the
// Dispatcher would: only those whose match filter accepts the file.
func (r *Running) testDispatchFile(hook manifest.Hook, id uint64, done func(int, error)) (n int, err error) {
	info, ok := manifest.Lookup(hook)
	if !ok {
		return 0, fmt.Errorf("mock.dispatch: no hook %q", hook)
	}
	if info.Subject != manifest.SubjectFile || hook == manifest.FileOnBatchComplete {
		return 0, fmt.Errorf("mock.dispatch: %q is not a hook about one recovered file", hook)
	}
	if r.deps.Files == nil {
		return 0, fmt.Errorf("mock.dispatch: no recovered-file store")
	}
	f, ok := r.deps.Files.Get(id)
	if !ok {
		return 0, fmt.Errorf("mock.dispatch: no recovered file %d", id)
	}
	origin := ""
	if f.Source == files.SourcePlugin {
		origin = f.Author // a plugin is not called for what it recovered itself
	}
	facts := manifest.Facts{
		Source: string(f.Source), Host: f.Host, Path: f.Path,
		ContentType: f.MediaType, Types: []string{f.Kind},
	}
	return r.fanout(hook, info, &facts, origin, 0, func(vm *goja.Runtime) goja.Value {
		return event(vm, hook, "file", fileValue(vm, f))
	}, done)
}

// testDispatchFileBatch calls this plugin's targets for
// file.on_batch_complete, as the Dispatcher would.
func (r *Running) testDispatchFileBatch(hook manifest.Hook, batch string, done func(int, error)) (n int, err error) {
	info, ok := manifest.Lookup(hook)
	if !ok || hook != manifest.FileOnBatchComplete {
		return 0, fmt.Errorf("mock.dispatch: %q is not the file batch hook", hook)
	}
	if r.deps.Files == nil {
		return 0, fmt.Errorf("mock.dispatch: no recovered-file store")
	}
	var b files.Batch
	found := false
	for _, candidate := range r.deps.Files.Batches() {
		if candidate.ID == batch {
			b, found = candidate, true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("mock.dispatch: no recovery %q (give mock.file a batch)", batch)
	}
	origin := ""
	if b.Source == files.SourcePlugin {
		origin = b.Author
	}
	facts := manifest.Facts{Source: string(b.Source), Host: b.Host, Types: []string{b.Kind}}
	// The batch hook matches on what a recovery has, not on a path or a
	// media type (see Matcher.WantsFileBatch), so the targets are picked
	// the same way here.
	var calls []manifest.Target
	for _, t := range r.man.Targets {
		if t.Hook != hook || (origin != "" && origin == r.src.Name && !t.Self) {
			continue
		}
		m, cerr := t.Match.Compile(info)
		if cerr != nil {
			return 0, cerr
		}
		if m.WantsFileBatch(&facts) {
			calls = append(calls, t)
		}
	}
	return r.callAll(calls, hook, 0, func(vm *goja.Runtime) goja.Value {
		return event(vm, hook, "batch", batchValue(vm, b))
	}, done)
}

// fanout calls every target of the plugin on a hook whose filter accepts the
// facts, with the event build makes. origin is the flow's, so a target
// without self skips the plugin's own traffic; it is empty for events that
// aren't a flow's. done is called once the calls have settled.
func (r *Running) fanout(hook manifest.Hook, info manifest.HookInfo, facts *manifest.Facts,
	origin string, depth int, build func(*goja.Runtime) goja.Value, done func(int, error)) (int, error) {
	var calls []manifest.Target
	for _, t := range r.man.Targets {
		if t.Hook != hook {
			continue
		}
		if origin != "" && origin == r.src.Name && !t.Self {
			continue
		}
		m, err := t.Match.Compile(info)
		if err != nil {
			return 0, err
		}
		if m.Match(facts) {
			calls = append(calls, t)
		}
	}
	return r.callAll(calls, hook, depth, build, done)
}

// callAll calls the targets chosen for a hook with the event build makes,
// calling done once they have all settled (not at all when there are none:
// n is 0).
func (r *Running) callAll(calls []manifest.Target, hook manifest.Hook, depth int,
	build func(*goja.Runtime) goja.Value, done func(int, error)) (int, error) {
	if len(calls) == 0 {
		return 0, nil
	}
	var mu sync.Mutex
	left, firstErr := len(calls), error(nil)
	for _, t := range calls {
		r.call(r.rt, hook, t.Function, depth, build, func(_ goja.Value, err error) {
			mu.Lock()
			left--
			if err != nil && firstErr == nil {
				firstErr = err
			}
			last := left == 0
			mu.Unlock()
			if last {
				done(len(calls), firstErr)
			}
		})
	}
	return len(calls), nil
}

// fixtureFlow builds a finished flow from the testing module's FlowFixture.
func fixtureFlow(f map[string]any) (capture.Flow, error) {
	raw, _ := f["url"].(string)
	out, err := buildRequest(map[string]any{
		"url": raw, "method": f["method"], "headers": f["requestHeaders"], "body": f["requestBody"],
	})
	if err != nil {
		return capture.Flow{}, fmt.Errorf("mock.flow: %w", err)
	}
	flow := capture.Flow{
		State: capture.StateComplete, Target: out.addr, Method: out.method, Host: out.host, Path: out.path,
		Secure: out.secure, Request: capture.Message{Raw: out.raw, Size: int64(len(out.raw))}, Started: time.Now(),
	}
	if s, ok := f["source"].(string); ok && s != "proxy" {
		flow.Source = capture.Source(s)
	}
	flow.TargetID, _ = f["targetId"].(string)
	flow.Origin, _ = f["origin"].(string)

	status := 0
	if n, ok := f["status"].(int64); ok {
		status = int(n)
	}
	_, hasHeaders := f["responseHeaders"]
	body, hasBody := f["responseBody"].(string)
	if status == 0 && (hasHeaders || hasBody) {
		status = 200
	}
	if status == 0 {
		flow.State = capture.StatePending
		return flow, nil
	}
	resp := scriptedResponse(status, headerPairs(f["responseHeaders"]), body)
	flow.StatusCode, flow.StatusText = status, fmt.Sprintf("%d %s", status, statusText(status))
	flow.Response = capture.Message{Raw: resp, Size: int64(len(resp))}
	return flow, nil
}

// mockNet answers a test's outbound requests from a script, and records
// them.
type mockNet struct {
	mu      sync.Mutex
	sentLog []map[string]any
	rules   []mockRule
}

// mockDirectDial is the test runner's upstream chain: never dialled, since
// mock.send answers every request.
func mockDirectDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("mock: not dialled")
}

// isMockDirect tells a direct send (dialled through the test runner's
// upstream chain) from one through the proxy (dialled by a closure in
// http.go).
func isMockDirect(dial func(context.Context, string, string) (net.Conn, error)) bool {
	return dial != nil && reflect.ValueOf(dial).Pointer() == reflect.ValueOf(mockDirectDial).Pointer()
}

type mockRule struct {
	re      *regexp.Regexp
	status  int
	headers [][2]string
	body    string
}

func (m *mockNet) send(_ context.Context, r replay.Request) (replay.Response, error) {
	method, target, host := headLine(r.Raw)
	scheme := "http"
	if r.Secure {
		scheme = "https"
	}
	if host == "" {
		host = r.Addr
	}
	url := scheme + "://" + host + target
	m.mu.Lock()
	m.sentLog = append(m.sentLog, map[string]any{
		"url": url, "method": method, "raw": string(r.Raw), "throughProxy": !isMockDirect(r.Dial),
	})
	rule := mockRule{status: 404, body: "no mock.respond matched " + url}
	for _, rl := range m.rules {
		if rl.re.MatchString(url) {
			rule = rl
			break
		}
	}
	m.mu.Unlock()
	raw := scriptedResponse(rule.status, rule.headers, rule.body)
	return replay.Response{
		Raw: raw, Size: len(raw), StatusCode: rule.status,
		Status: fmt.Sprintf("%d %s", rule.status, statusText(rule.status)),
	}, nil
}

func (m *mockNet) script(pattern string, resp map[string]any) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("mock.respond: %w", err)
	}
	rule := mockRule{re: re, status: 200, headers: headerPairs(resp["headers"])}
	if n, ok := resp["status"].(int64); ok {
		rule.status = int(n)
	}
	rule.body, _ = resp["body"].(string)
	m.mu.Lock()
	m.rules = append(m.rules, rule)
	m.mu.Unlock()
	return nil
}

func (m *mockNet) list() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any{}, m.sentLog...)
}

func (m *mockNet) reset() {
	m.mu.Lock()
	m.sentLog, m.rules = nil, nil
	m.mu.Unlock()
}

func scriptedResponse(status int, headers [][2]string, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, statusText(status))
	hasType := false
	for _, h := range headers {
		if strings.EqualFold(h[0], "Content-Length") {
			continue
		}
		if strings.EqualFold(h[0], "Content-Type") {
			hasType = true
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	if !hasType && body != "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return []byte(b.String())
}

func statusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return t
	}
	return "Status"
}
