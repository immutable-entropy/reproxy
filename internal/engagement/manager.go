package engagement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"reproxy/internal/target"
)

// StateFile remembers, in the data directory, which engagement was open and
// which were opened before, so the app reopens the active one next launch.
const StateFile = "engagement-state.json"

// recentLimit caps the remembered engagements, newest first.
const recentLimit = 10

// State is StateFile's contents: engagement directories, newest first.
type State struct {
	// Active is the engagement that was open when the app last closed, or
	// "" if none was.
	Active string `json:"active,omitempty"`
	// Recent is the engagements opened before, most recent first.
	Recent []string `json:"recent,omitempty"`
}

// Manager owns the open engagement. Opening one points the target store at
// that engagement's targets.json; closing it points the store back at the
// data directory's own targets, so reProxy still works with no engagement
// open.
//
// It is Qt-free: the UI calls it from the File menu and follows it through
// Subscribe.
type Manager struct {
	statePath string
	// scratch is the targets file used while no engagement is open.
	scratch string
	targets *target.Store

	mu sync.Mutex
	// stateErr is kept from the load in NewManager and reported by Restore:
	// an unreadable state file costs the recent list, not the launch.
	stateErr error
	open     *Engagement
	state    State
	subs     map[int]func()
	next     int
}

// NewManager returns a manager for the state file at statePath, with
// scratchTargets as the targets file to use while nothing is open. Nothing
// is opened yet: call Restore for that.
func NewManager(statePath, scratchTargets string, targets *target.Store) *Manager {
	m := &Manager{
		statePath: statePath,
		scratch:   scratchTargets,
		targets:   targets,
		subs:      make(map[int]func()),
	}
	m.state, m.stateErr = loadState(statePath)
	return m
}

// StatePath returns the state file that sits beside the settings file at
// configPath.
func StatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), StateFile)
}

// Current returns the open engagement, if there is one.
func (m *Manager) Current() (Engagement, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.open == nil {
		return Engagement{}, false
	}
	return *m.open, true
}

// Recent returns the engagement directories opened before, most recent
// first, including the one open now.
func (m *Manager) Recent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.state.Recent...)
}

// Restore reopens the engagement that was active when the app last closed.
// A missing or moved engagement is forgotten and reported: it must not stop
// the app from starting.
func (m *Manager) Restore() error {
	m.mu.Lock()
	active, err := m.state.Active, m.stateErr
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if active == "" {
		return nil
	}
	if _, err := m.Open(active); err != nil {
		m.Forget(active)
		return fmt.Errorf("reopening the last engagement: %w", err)
	}
	return nil
}

// Create lays down a new engagement and opens it.
func (m *Manager) Create(dir, name, description string, targets []target.Target) (Engagement, error) {
	e, err := Create(dir, name, description, targets)
	if err != nil {
		return Engagement{}, err
	}
	return m.adopt(e)
}

// Open opens the engagement in dir, replacing whatever was open. The target
// store follows: its list becomes that engagement's.
func (m *Manager) Open(dir string) (Engagement, error) {
	e, err := Open(dir)
	if err != nil {
		return Engagement{}, err
	}
	return m.adopt(e)
}

// adopt makes e the open engagement: the target store moves to its targets,
// the state file records it, and subscribers are told.
func (m *Manager) adopt(e Engagement) (Engagement, error) {
	if err := m.targets.Open(e.TargetsPath()); err != nil {
		return Engagement{}, err
	}
	e.Opened = time.Now()
	// A read-only or removable location shouldn't stop the engagement from
	// being used, so a failed timestamp write is not fatal.
	_ = e.save()

	m.mu.Lock()
	m.open = &e
	m.state.Active = e.Dir
	m.state.Recent = promote(m.state.Recent, e.Dir)
	err := m.saveStateLocked()
	m.mu.Unlock()

	m.notify()
	return e, err
}

// Close closes the open engagement, putting the target store back on the
// data directory's own targets. Closing when nothing is open does nothing.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.open == nil {
		m.mu.Unlock()
		return nil
	}
	m.open = nil
	m.state.Active = ""
	err := m.saveStateLocked()
	m.mu.Unlock()

	if terr := m.targets.Open(m.scratch); terr != nil && err == nil {
		err = terr
	}
	m.notify()
	return err
}

// Rename changes the open engagement's name and description.
func (m *Manager) Rename(name, description string) error {
	m.mu.Lock()
	if m.open == nil {
		m.mu.Unlock()
		return errors.New("no engagement is open")
	}
	e := *m.open
	e.Name, e.Description = strings.TrimSpace(name), strings.TrimSpace(description)
	if err := e.Save(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.open = &e
	m.mu.Unlock()
	m.notify()
	return nil
}

// Forget drops an engagement from the recent list — for one that has been
// moved or deleted behind our back.
func (m *Manager) Forget(dir string) {
	m.mu.Lock()
	kept := m.state.Recent[:0]
	for _, d := range m.state.Recent {
		if d != dir {
			kept = append(kept, d)
		}
	}
	m.state.Recent = kept
	if m.state.Active == dir {
		m.state.Active = ""
	}
	m.saveStateLocked()
	m.mu.Unlock()
	m.notify()
}

// Subscribe registers fn to be called after an engagement is opened, closed
// or renamed. The returned function removes it; fn runs on the goroutine
// that made the change.
func (m *Manager) Subscribe(fn func()) (unsubscribe func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	m.subs[id] = fn
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

func (m *Manager) notify() {
	m.mu.Lock()
	fns := make([]func(), 0, len(m.subs))
	for _, fn := range m.subs {
		fns = append(fns, fn)
	}
	m.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// saveStateLocked persists the active and recent engagements. The caller
// holds the lock.
func (m *Manager) saveStateLocked() error {
	if m.statePath == "" {
		return nil // in-memory manager, for tests
	}
	return target.WriteJSON(m.statePath, m.state)
}

// promote puts dir at the front of the recent list, without duplicates, and
// trims it to recentLimit.
func promote(recent []string, dir string) []string {
	out := make([]string, 0, len(recent)+1)
	out = append(out, dir)
	for _, d := range recent {
		if d != dir && len(out) < recentLimit {
			out = append(out, d)
		}
	}
	return out
}

func loadState(path string) (State, error) {
	if path == "" {
		return State{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}
