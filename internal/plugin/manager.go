package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/jsrt"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/plugin/sdk"
	"reproxy/internal/websocket"
)

// State is where a plugin is.
type State string

const (
	// StateDisabled: switched off, by its manifest or the user.
	StateDisabled State = "disabled"
	// StateRunning: loaded, its hooks live.
	StateRunning State = "running"
	// StateFaulted: it kept failing, so its hooks were stopped for the
	// session. Reload (or a change to its files) tries again.
	StateFaulted State = "faulted"
	// StateError: it can't run — its manifest or code has problems, or it
	// failed to load.
	StateError State = "error"
)

// Override is the user's choice for one plugin, kept in the settings file.
// Secret settings are not here: they are in the secrets file.
type Override struct {
	Status   manifest.Status            `json:"status,omitempty"`
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
}

// Config is the user's plugin settings.
type Config struct {
	Overrides map[string]Override
	// AllowRelease lets intercept hooks forward and drop held messages, so
	// the user never sees them; off, those answers count as edits.
	AllowRelease bool
	// AllowHold lets targets with hold: true have core hold traffic for
	// them; off, intercept hooks only see what the user's interception
	// holds.
	AllowHold bool
}

// Info describes a plugin, for the Settings page.
type Info struct {
	Name string
	Path string
	// Manifest is nil when plugin.json couldn't be read at all.
	Manifest *manifest.Manifest
	// Status is what the user chose, or the manifest's default.
	Status   manifest.Status
	State    State
	Problems []string
	Stats    Stats
	Log      []LogLine
	// Settings are the effective values, secrets left out; SecretSet says
	// which secrets have a value.
	Settings  Settings
	SecretSet map[string]bool
}

// UserDir is where the user's plugins are, under the data directory.
func UserDir(dataDir string) string { return filepath.Join(dataDir, "plugins") }

// entry is one plugin the manager knows of.
type entry struct {
	src      Source
	root     *os.Root // a user plugin's directory, held open while known
	man      *manifest.Manifest
	problems []string
	hash     string
	run      *Running
	// last is the runtime that was running, so a stopped plugin's log and
	// counters still show.
	last    *Running
	faulted bool
	loadErr string // the last load failed; Reload or a change tries again
	// nsErr is set when this plugin's commands would answer to a name
	// that is core's. It is worked out afresh on every reconcile, so a
	// renamed plugin is reconsidered without a reload.
	nsErr    string
	settings Settings
}

// broken reports whether the plugin can't run.
func (e *entry) broken() bool { return len(e.problems) > 0 || e.loadErr != "" || e.nsErr != "" }

func (e *entry) allProblems() []string {
	out := append([]string(nil), e.problems...)
	if e.loadErr != "" {
		out = append(out, e.loadErr)
	}
	if e.nsErr != "" {
		out = append(out, e.nsErr)
	}
	return out
}

// Manager finds plugins, keeps them in step with the user's settings, and
// runs the enabled ones.
type Manager struct {
	deps  *Deps
	disp  *Dispatcher
	stage *stage
	ws    *wsHandler

	mu      sync.Mutex
	entries map[string]*entry
	cfg     Config
	closed  bool

	subMu   sync.Mutex
	subs    map[int]func()
	nextSub int
}

// NewManager returns a manager with nothing loaded: call Rescan and Apply.
func NewManager(deps *Deps) *Manager {
	m := &Manager{deps: deps, entries: make(map[string]*entry), subs: make(map[int]func())}
	m.disp = newDispatcher(func() bool { return deps.Targets == nil || deps.Targets.Set().Empty() })
	m.stage = &stage{d: m.disp, store: deps.Store}
	m.ws = &wsHandler{d: m.disp, store: deps.Store}
	m.stage.allowRelease.Store(true)
	return m
}

// Dispatcher is what core's stores are subscribed to.
func (m *Manager) Dispatcher() *Dispatcher { return m.disp }

// The WebSocket handler methods: app adapts these to proxy.WSHandler so the
// plugin package needn't import proxy.

// WSEdits reports whether a WebSocket connection should be editable.
func (m *Manager) WSEdits(s intercept.Subject) bool { return m.ws.Edits(s) }

// WSDecide returns what to do with one WebSocket message.
func (m *Manager) WSDecide(ctx context.Context, flowID uint64, dir websocket.Direction, typ websocket.Type, data []byte) websocket.Decision {
	return m.ws.Decide(ctx, flowID, dir, typ, data)
}

// WSOpened and WSClosed announce a WebSocket connection's life.
func (m *Manager) WSOpened(flowID uint64) { m.ws.Opened(flowID, intercept.Subject{}) }
func (m *Manager) WSClosed(flowID uint64) { m.ws.Closed(flowID) }

// Subscribe registers fn to be called, on the goroutine that made the
// change, whenever the plugins or their states change.
func (m *Manager) Subscribe(fn func()) (unsubscribe func()) {
	m.subMu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = fn
	m.subMu.Unlock()
	return func() {
		m.subMu.Lock()
		delete(m.subs, id)
		m.subMu.Unlock()
	}
}

func (m *Manager) notify() {
	m.subMu.Lock()
	fns := make([]func(), 0, len(m.subs))
	for _, fn := range m.subs {
		fns = append(fns, fn)
	}
	m.subMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Rescan looks for plugins again in the user's directory.
// New plugins are validated (and started, if enabled); changed ones are
// reloaded; removed ones stopped. It is what happens at startup and
// whenever the Plugins window shows its Plugins section.
func (m *Manager) Rescan() {
	found := m.discover()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		closeRoots(found)
		return
	}
	reasons := map[string]string{}
	for name, e := range m.entries {
		next, ok := found[name]
		switch {
		case !ok:
			m.stop(e, "rescan", false)
			if e.root != nil {
				e.root.Close()
			}
			delete(m.entries, name)
		case next.hash == e.hash:
			if next.root != nil {
				next.root.Close() // keep the one already open
			}
			delete(found, name)
		default:
			wasRunning := e.run != nil
			m.stop(e, "rescan", false)
			if e.root != nil {
				e.root.Close()
			}
			delete(m.entries, name)
			if wasRunning {
				reasons[name] = "rescan"
			}
		}
	}
	for name, e := range found {
		m.entries[name] = e
		if reasons[name] == "" {
			reasons[name] = "startup"
		}
	}
	m.reconcile(reasons)
	m.mu.Unlock()
	m.notify()
}

// Apply takes the user's settings: plugins switched on or off, settings
// changed. It is called at startup and whenever settings are applied.
func (m *Manager) Apply(cfg Config) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.cfg = cfg
	m.stage.allowRelease.Store(cfg.AllowRelease)
	m.reconcile(nil)
	m.mu.Unlock()
	m.notify()
}

// Reload stops a plugin and loads it again from its files, clearing a
// fault.
func (m *Manager) Reload(name string) error {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no plugin %q", name)
	}
	m.stop(e, "reload", false)
	e.faulted, e.loadErr, e.nsErr = false, "", ""
	m.reconcile(map[string]string{name: "reload"})
	var err error
	if e.broken() {
		err = errors.New(strings.Join(e.allProblems(), "; "))
	}
	m.mu.Unlock()
	m.notify()
	return err
}

// Close stops every plugin.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	for _, e := range m.entries {
		m.stop(e, "shutdown", false)
		if e.root != nil {
			e.root.Close()
		}
	}
	m.publish()
	m.mu.Unlock()
}

// List describes every plugin, by name.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, m.info(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get describes one plugin.
func (m *Manager) Get(name string) (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return Info{}, false
	}
	return m.info(e), true
}

func (m *Manager) info(e *entry) Info {
	in := Info{
		Name: e.src.Name, Path: e.src.Path, Manifest: e.man,
		Status: m.status(e), Problems: e.allProblems(),
		Settings: Settings{}, SecretSet: map[string]bool{},
	}
	switch {
	case e.broken():
		in.State = StateError
	case e.faulted:
		in.State = StateFaulted
	case e.run != nil:
		in.State = StateRunning
	default:
		in.State = StateDisabled
	}
	if run := e.run; run != nil || e.last != nil {
		if run == nil {
			run = e.last
		}
		in.Stats, in.Log = run.Stats(), run.Log()
	}
	if e.man != nil {
		for k, v := range m.effective(e) {
			if s, _ := e.man.Setting(k); s != nil && s.Type == manifest.TypeSecret {
				in.SecretSet[k] = string(v) != `""`
				continue
			}
			in.Settings[k] = v
		}
	}
	return in
}

// status is a plugin's effective status: the user's choice, or the
// manifest's default.
func (m *Manager) status(e *entry) manifest.Status {
	if o, ok := m.cfg.Overrides[e.src.Name]; ok && o.Status != "" {
		return o.Status
	}
	if e.man == nil {
		return manifest.Disabled
	}
	return e.man.Status
}

// effective works out a plugin's setting values: the user's choice where it
// is valid, else the default, else the type's zero; secrets from the
// secrets file.
func (m *Manager) effective(e *entry) Settings {
	out := Settings{}
	if e.man == nil {
		return out
	}
	over := m.cfg.Overrides[e.src.Name].Settings
	for i := range e.man.Settings {
		s := &e.man.Settings[i]
		if s.Type == manifest.TypeSecret {
			v := ""
			if m.deps.Secrets != nil {
				v, _ = m.deps.Secrets.PluginSecret(e.src.Name, s.Key)
			}
			b, _ := json.Marshal(v)
			out[s.Key] = b
			continue
		}
		if v, ok := over[s.Key]; ok && s.Check(v) == nil {
			out[s.Key] = v
		} else if len(s.Default) > 0 {
			out[s.Key] = s.Default
		} else {
			out[s.Key] = s.Zero()
		}
	}
	return out
}

// reconcile starts and stops plugins to match their effective status, and
// tells running ones about changed settings. reasons says why a plugin
// that starts is starting, by name ("enable" when absent). The caller holds
// the lock.
func (m *Manager) reconcile(reasons map[string]string) {
	names := make([]string, 0, len(m.entries))
	for name := range m.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := m.entries[name]
		e.nsErr = ""
		want := m.status(e) == manifest.Enabled && !e.broken() && !e.faulted
		if want {
			if err := checkNamespace(e.man); err != nil {
				e.nsErr, want = err.Error(), false
				m.deps.log().Warn("plugin can't run", "plugin", name, "err", err)
			}
		}
		switch {
		case want && e.run == nil:
			reason := reasons[name]
			if reason == "" {
				reason = "enable"
			}
			m.start(e, reason)
		case !want && e.run != nil:
			if e.nsErr != "" {
				m.stop(e, "reserved namespace", false)
				break
			}
			m.stop(e, "disable", true)
		case e.run != nil:
			m.updateSettings(e)
		}
	}
	m.publish()
}

// start loads a plugin and calls its on_load (and, when the user just
// enabled it, on_status_change). A failure to load is one of its problems.
func (m *Manager) start(e *entry, reason string) {
	settings := m.effective(e)
	run, err := load(e.src, e.man, m.deps, settings, loadOptions{})
	if err != nil {
		e.loadErr = "loading: " + err.Error()
		m.deps.log().Warn("plugin failed to load", "plugin", e.src.Name, "err", err)
		return
	}
	name := e.src.Name
	run.onFault = func(err error) { m.fault(name, err) }
	e.run, e.last, e.settings = run, run, settings
	if len(e.man.FindingTypes) > 0 && m.deps.Findings != nil {
		types := make([]findings.Type, len(e.man.FindingTypes))
		for i, t := range e.man.FindingTypes {
			types[i] = findings.Type{ID: t.ID, Name: t.Name, Color: t.Color}
		}
		if err := m.deps.Findings.DeclareTypes(name, types); err != nil {
			run.logf(slog.LevelWarn, "declaring finding types: "+err.Error())
		}
	}
	run.lifecycle(manifest.PluginOnLoad, map[string]any{"hook": string(manifest.PluginOnLoad), "reason": reason, "settings": settingsMap(settings)})
	if reason == "enable" {
		run.lifecycle(manifest.PluginOnStatusChange, map[string]any{
			"hook": string(manifest.PluginOnStatusChange), "from": string(manifest.Disabled), "to": string(manifest.Enabled), "reason": "user",
		})
	}
	m.deps.log().Info("plugin started", "plugin", name, "reason", reason)
}

// stop calls a plugin's on_status_change (when the user disabled it) and
// on_unload, then stops it.
func (m *Manager) stop(e *entry, reason string, userDisabled bool) {
	run := e.run
	if run == nil {
		return
	}
	e.run = nil
	if userDisabled {
		run.lifecycle(manifest.PluginOnStatusChange, map[string]any{
			"hook": string(manifest.PluginOnStatusChange), "from": string(manifest.Enabled), "to": string(manifest.Disabled), "reason": "user",
		})
	}
	run.lifecycle(manifest.PluginOnUnload, map[string]any{"hook": string(manifest.PluginOnUnload), "reason": reason})
	run.close()
	if m.deps.Findings != nil {
		m.deps.Findings.WithdrawTypes(e.src.Name)
	}
	m.deps.log().Info("plugin stopped", "plugin", e.src.Name, "reason", reason)
}

// updateSettings hands a running plugin its settings when they changed, and
// calls its on_settings_change.
func (m *Manager) updateSettings(e *entry) {
	next := m.effective(e)
	changed := map[string]any{}
	for k, v := range next {
		if old := e.settings[k]; !bytes.Equal(old, v) {
			changed[k] = map[string]any{"old": rawAny(old), "new": rawAny(v)}
		}
	}
	if len(changed) == 0 {
		return
	}
	e.settings = next
	e.run.settings.Store(&next)
	e.run.lifecycle(manifest.PluginOnSettingsChange, map[string]any{
		"hook": string(manifest.PluginOnSettingsChange), "changed": changed, "settings": settingsMap(next),
	})
}

// fault stops a plugin that keeps failing, for the session.
func (m *Manager) fault(name string, err error) {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok || e.run == nil || m.closed {
		m.mu.Unlock()
		return
	}
	m.deps.log().Warn("plugin faulted: its hooks are stopped until it is reloaded", "plugin", name, "err", err)
	e.faulted = true
	run := e.run
	run.lifecycle(manifest.PluginOnStatusChange, map[string]any{
		"hook": string(manifest.PluginOnStatusChange), "from": string(manifest.Enabled), "to": string(StateFaulted), "reason": "fault",
	})
	m.stop(e, "fault", false)
	m.publish()
	m.mu.Unlock()
	m.notify()
}

// publish rebuilds the routing from the running plugins and installs or
// removes the intercept stage. The caller holds the lock.
func (m *Manager) publish() {
	var running []*Running
	for _, e := range m.entries {
		if e.run != nil {
			running = append(running, e.run)
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].src.Name < running[j].src.Name })
	m.disp.set(running)
	if m.deps.Intercept == nil {
		return
	}
	if m.disp.wants(manifest.InterceptOnRequestPending) || m.disp.wants(manifest.InterceptOnResponsePending) {
		m.deps.Intercept.SetStage(m.stage.run)
	} else {
		m.deps.Intercept.SetStage(nil)
	}
	var hold intercept.PluginHoldFunc
	if m.cfg.AllowHold {
		hold = m.disp.holdFunc()
	}
	m.deps.Intercept.SetPluginHold(hold)
}

// ---- Discovery ------------------------------------------------------------------

// discover reads every plugin's manifest and checks it, without running
// anything.
func (m *Manager) discover() map[string]*entry {
	found := map[string]*entry{}
	if m.deps.DataDir == "" {
		return found
	}
	dir := UserDir(m.deps.DataDir)
	dirs, _ := os.ReadDir(dir)
	for _, d := range dirs {
		// A dot-directory is a download in progress (or the copy an update
		// replaces), never a plugin: no plugin's name starts with a dot.
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, d.Name())
		root, err := os.OpenRoot(p)
		if err != nil {
			continue
		}
		e := inspect(Source{Name: d.Name(), FS: root.FS(), Path: p}, root)
		found[e.src.Name] = e
	}
	return found
}

// inspect reads and checks one plugin: its manifest, and that the
// functions its targets name are exported.
func inspect(src Source, root *os.Root) *entry {
	e := &entry{src: src, root: root, hash: hashFS(src.FS)}
	data, err := fs.ReadFile(src.FS, manifest.FileName)
	if err != nil {
		e.problems = append(e.problems, "reading "+manifest.FileName+": "+err.Error())
		return e
	}
	man, err := manifest.Parse(data, src.Name)
	e.man = man
	var errs manifest.Errors
	switch {
	case errors.As(err, &errs):
		for _, err := range errs {
			e.problems = append(e.problems, err.Error())
		}
	case err != nil:
		e.problems = append(e.problems, err.Error())
		return e
	}
	if _, err := fs.Stat(src.FS, manifest.EntryName); err != nil {
		e.problems = append(e.problems, manifest.EntryName+" is missing")
		return e
	}
	exports, err := jsrt.Exports(src.FS, manifest.EntryName, sdk.Modules())
	if err != nil {
		e.problems = append(e.problems, "bundling: "+err.Error())
		return e
	}
	have := map[string]bool{}
	for _, x := range exports {
		have[x] = true
	}
	for i, t := range man.Targets {
		if t.Function != "" && !have[t.Function] {
			e.problems = append(e.problems, fmt.Sprintf("targets[%d]: %s doesn't export %s", i, manifest.EntryName, t.Function))
		}
	}
	for i, a := range man.Actions {
		if a.Function != "" && !have[a.Function] {
			e.problems = append(e.problems, fmt.Sprintf("actions[%d] (%s): %s doesn't export %s", i, a.ID, manifest.EntryName, a.Function))
		}
	}
	return e
}

// hashFS fingerprints a plugin's files, so a rescan can tell what changed.
func hashFS(fsys fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		switch path.Ext(p) {
		case ".ts", ".js", ".mts", ".mjs", ".json":
		default:
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func closeRoots(found map[string]*entry) {
	for _, e := range found {
		if e.root != nil {
			e.root.Close()
		}
	}
}

func settingsMap(s Settings) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = rawAny(v)
	}
	return out
}

func rawAny(raw json.RawMessage) any {
	var v any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// Inspect reads and checks a plugin without running it: its manifest, and
// that the functions its targets name are exported. problems is empty for
// a plugin that would load.
func Inspect(src Source) (man *manifest.Manifest, problems []string) {
	e := inspect(src, nil)
	return e.man, e.problems
}
