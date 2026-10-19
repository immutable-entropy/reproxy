package target

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"reproxy/internal/atomicfile"
)

// Store holds the configured targets and keeps them in a targets.json file.
//
// Which file that is depends on the open engagement: Open points the store at
// another one, replacing the list with its contents, which is how opening an
// engagement brings its targets with it (see package engagement).
//
// Readers come in two shapes. The UI calls List/Get/Save/Remove, which lock.
// The proxy calls Set, which hands back the compiled snapshot through an
// atomic pointer: matching every request must not contend with the UI.
type Store struct {
	mu   sync.Mutex
	path string
	list []Target
	subs map[int]func()
	next int

	set atomic.Pointer[Set]
}

// file is the on-disk shape, a wrapper so fields can be added later.
type file struct {
	Targets []Target `json:"targets"`
}

// NewStore loads the targets at path. A missing file means none yet; a
// corrupt one is an error, so a typo in a hand-edited file is reported rather
// than silently dropping the user's scope. An empty path is an in-memory
// store, for tests.
func NewStore(path string) (*Store, error) {
	s := &Store{subs: make(map[int]func())}
	s.set.Store(&Set{})
	if err := s.Open(path); err != nil {
		return nil, err
	}
	return s, nil
}

// Open points the store at the targets file at path and replaces the list
// with what it holds, notifying subscribers. Nothing changes if the file
// can't be read or parsed.
func (s *Store) Open(path string) error {
	list, err := readFile(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	was := s.list
	s.path, s.list = path, list
	if err := s.recompile(); err != nil {
		s.list = was // keep the live set and the old list consistent
		s.recompile()
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	s.notify()
	return nil
}

// readFile loads and checks one targets file. An empty path (an in-memory
// store) and a missing file both mean no targets.
func readFile(path string) ([]Target, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading targets: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i, t := range f.Targets {
		f.Targets[i] = t.Normalized()
		if err := f.Targets[i].Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return f.Targets, nil
}

// List returns the targets in priority order. The slice is a copy.
func (s *Store) List() []Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Target(nil), s.list...)
}

// Get returns one target by ID.
func (s *Store) Get(id string) (Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.list {
		if t.ID == id {
			return t, true
		}
	}
	return Target{}, false
}

// Set returns the compiled targets. It is safe to call from any goroutine
// and never blocks; the returned Set is immutable.
func (s *Store) Set() *Set { return s.set.Load() }

// NextColor returns the colour a new target would be given: the first
// palette colour no current target holds.
func (s *Store) NextColor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PickColor(s.colorsLocked())
}

func (s *Store) colorsLocked() []string {
	used := make([]string, 0, len(s.list))
	for _, t := range s.list {
		used = append(used, t.Color)
	}
	return used
}

// Create adds a new target for value and returns it. An empty kind is
// detected from the value, and the colour is the next unused palette one.
func (s *Store) Create(kind Kind, value string) (Target, error) {
	s.mu.Lock()
	t := New(kind, value, PickColor(s.colorsLocked())).Normalized()
	if err := t.Validate(); err != nil {
		s.mu.Unlock()
		return Target{}, err
	}
	s.list = append(s.list, t)
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return Target{}, err
	}
	s.notify()
	return t, nil
}

// CreateAll adds a target for each value, as Create does, in one write and
// with one notification, so promoting many hosts at once doesn't rework
// everything that follows the targets once per host. Nothing is added if
// any value is invalid; the error names it.
func (s *Store) CreateAll(kind Kind, values []string) ([]Target, error) {
	s.mu.Lock()
	used := s.colorsLocked()
	added := make([]Target, 0, len(values))
	for _, v := range values {
		t := New(kind, v, PickColor(used)).Normalized()
		if err := t.Validate(); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("%s: %w", v, err)
		}
		used = append(used, t.Color)
		added = append(added, t)
	}
	if len(added) == 0 {
		s.mu.Unlock()
		return nil, nil
	}
	s.list = append(s.list, added...)
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.notify()
	return added, nil
}

// AddAll appends targets that came from somewhere else — an imported scope
// file — in one write and with one notification, so bringing in hundreds of
// hosts doesn't rework everything downstream of the targets once per host.
//
// Each one keeps the kind, value, rules, notes and enabled flag it arrives
// with, and is given a fresh ID, the next free colour and the current time.
// A host the list already holds is left alone rather than added twice: the
// count of those is returned, so the caller can say so. Nothing is written
// if any target is invalid, and the error names it.
func (s *Store) AddAll(more []Target) (added []Target, duplicates int, err error) {
	s.mu.Lock()
	have := make(map[string]bool, len(s.list))
	for _, t := range s.list {
		have[addressKey(t)] = true
	}
	used := s.colorsLocked()
	fresh := make([]Target, 0, len(more))
	for _, t := range more {
		t = Adopt(t, PickColor(used))
		if err := t.Validate(); err != nil {
			s.mu.Unlock()
			return nil, 0, err
		}
		key := addressKey(t)
		if have[key] {
			duplicates++
			continue
		}
		have[key] = true
		used = append(used, t.Color)
		fresh = append(fresh, t)
	}
	if len(fresh) == 0 {
		s.mu.Unlock()
		return nil, duplicates, nil
	}
	s.list = append(s.list, fresh...)
	err = s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	// notify takes the lock itself, so it goes after the unlock, as it does
	// in every other writer here.
	s.notify()
	return fresh, duplicates, nil
}

// addressKey identifies a target by the address it matches, so that the same
// host is not imported twice.
func addressKey(t Target) string {
	return string(t.Kind) + "\x00" + strings.ToLower(strings.TrimSpace(t.Value))
}

// Save stores t, replacing the target with the same ID or appending it.
// Nothing changes if t is invalid.
func (s *Store) Save(t Target) error {
	t = t.Normalized()
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	replaced := false
	for i := range s.list {
		if s.list[i].ID == t.ID {
			s.list[i], replaced = t, true
			break
		}
	}
	if !replaced {
		s.list = append(s.list, t)
	}
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// Remove deletes a target. Flows already collected under it keep their tag;
// they simply no longer resolve to a target.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	for i := range s.list {
		if s.list[i].ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			break
		}
	}
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// Move shifts a target by delta places, changing which target claims a host
// two of them cover. Moving past either end does nothing.
func (s *Store) Move(id string, delta int) error {
	s.mu.Lock()
	from := -1
	for i := range s.list {
		if s.list[i].ID == id {
			from = i
			break
		}
	}
	to := from + delta
	if from < 0 || to < 0 || to >= len(s.list) {
		s.mu.Unlock()
		return nil
	}
	t := s.list[from]
	s.list = append(s.list[:from], s.list[from+1:]...)
	s.list = append(s.list[:to], append([]Target{t}, s.list[to:]...)...)
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// Subscribe registers fn to be called after any change. The returned function
// removes it. fn runs on the goroutine that made the change.
func (s *Store) Subscribe(fn func()) (unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// commitLocked recompiles the match set and writes the file. The caller holds
// the lock. A write failure leaves the new targets in memory and in the match
// set — the user's edit stands for this session — and is reported.
func (s *Store) commitLocked() error {
	if err := s.recompile(); err != nil {
		return err
	}
	if s.path == "" {
		return nil // in-memory store, for tests
	}
	return WriteJSON(s.path, file{Targets: s.list})
}

// recompile publishes a fresh Set. The caller holds the lock.
func (s *Store) recompile() error {
	set, err := Compile(s.list)
	if err != nil {
		return err
	}
	s.set.Store(set)
	return nil
}

func (s *Store) notify() {
	s.mu.Lock()
	fns := make([]func(), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Path returns the targets file in use ("" for an in-memory store).
func (s *Store) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// WriteFile writes list to path as a targets file, without touching the
// store. The engagement wizard uses it to lay down an engagement's initial
// targets before anything is open.
func WriteFile(path string, list []Target) error {
	out := make([]Target, 0, len(list))
	for _, t := range list {
		t = t.Normalized()
		if err := t.Validate(); err != nil {
			return err
		}
		out = append(out, t)
	}
	return WriteJSON(path, file{Targets: out})
}

// WriteJSON writes v to path, for targets.json and the engagement files
// beside it. An engagement names the hosts the user is allowed to test, so
// it is theirs to read.
func WriteJSON(path string, v any) error {
	return atomicfile.WriteJSON(path, v, atomicfile.Private, 0o755)
}
