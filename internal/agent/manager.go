// Agent sessions: lifecycle, transcripts and persistence.
//
// The Manager owns the sessions; it runs a turn through a Runner, the seam
// the LLM/MCP runtime plugs into. With no Runner set, sessions still work
// as transcripts (create, switch, close, persist) and a sent message is
// answered with a notice that no runtime is configured, so the UI is usable
// before the runtime lands.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"reproxy/internal/atomicfile"
	"reproxy/internal/elide"
)

// EventKind identifies a change to the Manager.
type EventKind int

const (
	// SessionAdded: a new session (Summary set).
	SessionAdded EventKind = iota
	// SessionChanged: title, mode or status changed (Summary set).
	SessionChanged
	// SessionRemoved: closed (Summary.ID set).
	SessionRemoved
	// MessageAdded: a message was appended (SessionID and Message set).
	MessageAdded
)

// Event is a change broadcast to subscribers.
type Event struct {
	Kind      EventKind
	Summary   Summary
	SessionID string
	Message   Message
}

// Runner executes one turn of a session: it reads the transcript, talks to
// the model and its tools, and appends messages through the provided sink
// until the turn is done. It must return when ctx is cancelled.
type Runner interface {
	// Run advances a session by one user turn: it reads s.Convo as the prior
	// provider history (the latest user message already appended), drives the
	// model and its tools, emits display messages through emit, and returns
	// the updated provider history for the Manager to persist. It must return
	// when ctx is cancelled.
	Run(ctx context.Context, s Session, emit func(Message)) ([]Entry, error)
}

// ErrNoSession is returned for an unknown session ID.
var ErrNoSession = errors.New("agent: no such session")

// Manager owns the agent sessions for this process.
type Manager struct {
	dir    string // <datadir>/agents
	runner Runner

	mu       sync.Mutex
	sessions map[string]*Session
	cancels  map[string]context.CancelFunc

	subMu   sync.Mutex
	nextSub int
	subs    map[int]func(Event)
}

// NewManager returns a Manager storing transcripts under dir. Existing
// transcripts are loaded (as idle). runner may be nil until the runtime is
// wired.
func NewManager(dir string, runner Runner) *Manager {
	m := &Manager{
		dir:      dir,
		runner:   runner,
		sessions: make(map[string]*Session),
		cancels:  make(map[string]context.CancelFunc),
		subs:     make(map[int]func(Event)),
	}
	m.loadAll()
	return m
}

// SetRunner sets the turn runner (wired once the runtime exists).
func (m *Manager) SetRunner(r Runner) {
	m.mu.Lock()
	m.runner = r
	m.mu.Unlock()
}

// Subscribe registers fn for every change. The returned function
// unsubscribes.
func (m *Manager) Subscribe(fn func(Event)) (unsubscribe func()) {
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

func (m *Manager) emit(e Event) {
	m.subMu.Lock()
	fns := make([]func(Event), 0, len(m.subs))
	for _, fn := range m.subs {
		fns = append(fns, fn)
	}
	m.subMu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}

// Create starts a new session for profileID and returns its summary.
func (m *Manager) Create(profileID, title string) Summary {
	s := &Session{
		ID:        newSessionID(),
		ProfileID: profileID,
		Title:     title,
		Mode:      ModeAsk,
		Status:    StatusIdle,
		Created:   time.Now(),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	m.save(s)
	sum := s.summary()
	m.emit(Event{Kind: SessionAdded, Summary: sum})
	return sum
}

// List returns session summaries, oldest first.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	out := make([]Summary, 0, len(m.sessions))
	created := make(map[string]time.Time)
	for _, s := range m.sessions {
		out = append(out, s.summary())
		created[s.ID] = s.Created
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return created[out[i].ID].Before(created[out[j].ID]) })
	return out
}

// Get returns a copy of a session, transcript included.
func (m *Manager) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}
	c := *s
	c.Messages = append([]Message(nil), s.Messages...)
	return c, true
}

// SetMode changes a session's approval mode.
func (m *Manager) SetMode(id string, mode Mode) error {
	return m.change(id, func(s *Session) { s.Mode = mode })
}

// Rename changes a session's title.
func (m *Manager) Rename(id, title string) error {
	return m.change(id, func(s *Session) { s.Title = title })
}

// SetManaged marks a session as owned by a background service (see
// Session.Managed), or clears the mark. Close still removes it — the refusal
// to close a managed session is the UI's — so the service can take its own
// session down when its configuration turns off.
func (m *Manager) SetManaged(id string, managed bool) error {
	return m.change(id, func(s *Session) { s.Managed = managed })
}

func (m *Manager) change(id string, mutate func(*Session)) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		mutate(s)
	}
	var sum Summary
	if ok {
		sum = s.summary()
	}
	m.mu.Unlock()
	if !ok {
		return ErrNoSession
	}
	m.saveByID(id)
	m.emit(Event{Kind: SessionChanged, Summary: sum})
	return nil
}

// Close stops a running turn and removes a session, deleting its transcript.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	_, ok := m.sessions[id]
	delete(m.sessions, id)
	cancel := m.cancels[id]
	delete(m.cancels, id)
	m.mu.Unlock()
	if !ok {
		return
	}
	if cancel != nil {
		cancel()
	}
	os.Remove(m.path(id))
	m.emit(Event{Kind: SessionRemoved, Summary: Summary{ID: id}})
}

// Stop cancels a running turn without removing the session.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	cancel := m.cancels[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Send appends a user message and starts a turn. It is a no-op (bar the
// user message) if a turn is already running.
func (m *Manager) Send(id, text string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrNoSession
	}
	if s.Status == StatusRunning {
		m.mu.Unlock()
		return errors.New("a turn is already running")
	}
	m.appendLocked(s, Message{Role: RoleUser, Text: text, At: time.Now()})
	s.Convo = append(s.Convo, Entry{Role: EntryUser, Text: text})
	if s.Title == "" || s.Title == untitled {
		s.Title = titleFrom(text)
	}
	s.Status = StatusRunning
	runner := m.runner
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	snapshot := *s
	snapshot.Messages = append([]Message(nil), s.Messages...)
	m.mu.Unlock()

	m.saveByID(id)
	m.emit(Event{Kind: SessionChanged, Summary: snapshot.summary()})

	go m.runTurn(ctx, snapshot, runner)
	return nil
}

func (m *Manager) runTurn(ctx context.Context, s Session, runner Runner) {
	emit := func(msg Message) {
		if msg.At.IsZero() {
			msg.At = time.Now()
		}
		m.appendByID(s.ID, msg)
	}

	var err error
	var convo []Entry
	if runner == nil {
		emit(Message{Role: RoleSystem, Text: "No agent runtime is configured yet. " +
			"Add an agent in Settings → Agents; message handling arrives with the runtime."})
	} else {
		convo, err = runner.Run(ctx, s, emit)
		if err != nil && !errors.Is(err, context.Canceled) {
			emit(Message{Role: RoleSystem, Text: "Turn failed: " + err.Error()})
		}
		if errors.Is(err, context.Canceled) {
			emit(Message{Role: RoleSystem, Text: "Stopped."})
		}
	}

	m.mu.Lock()
	cur, ok := m.sessions[s.ID]
	if ok {
		if convo != nil {
			cur.Convo = convo
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			cur.Status = StatusError
		} else {
			cur.Status = StatusIdle
		}
		if c := m.cancels[s.ID]; c != nil {
			delete(m.cancels, s.ID)
		}
	}
	var sum Summary
	if ok {
		sum = cur.summary()
	}
	m.mu.Unlock()
	if ok {
		m.saveByID(s.ID)
		m.emit(Event{Kind: SessionChanged, Summary: sum})
	}
}

func (m *Manager) appendByID(id string, msg Message) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		m.appendLocked(s, msg)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	m.saveByID(id)
	m.emit(Event{Kind: MessageAdded, SessionID: id, Message: msg})
}

func (m *Manager) appendLocked(s *Session, msg Message) {
	s.Messages = append(s.Messages, msg)
}

// ---- persistence -----------------------------------------------------------

func (m *Manager) path(id string) string { return filepath.Join(m.dir, id+".json") }

func (m *Manager) saveByID(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	var c Session
	if ok {
		c = *s
		c.Messages = append([]Message(nil), s.Messages...)
	}
	m.mu.Unlock()
	if ok {
		m.save(&c)
	}
}

// save writes one chat session to its own file. A transcript holds
// whatever the user and the model said about the traffic, so it is
// private; a failure is ignored, since losing a transcript must not stop
// the chat.
func (m *Manager) save(s *Session) {
	if m.dir == "" {
		return
	}
	_ = atomicfile.WriteJSON(m.path(s.ID), s, atomicfile.Private, 0o755)
}

func (m *Manager) loadAll() {
	entries, err := os.ReadDir(m.dir)
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil || s.ID == "" {
			continue
		}
		if s.Status == StatusRunning {
			s.Status = StatusIdle // a turn can't survive the process
		}
		m.sessions[s.ID] = &s
	}
}

const untitled = "New chat"

func titleFrom(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" {
		return untitled
	}
	// A title is persisted to the session file, so it must be valid UTF-8:
	// cutting by bytes would leave a split rune for json.Marshal to replace
	// with U+FFFD, saving the corruption.
	return elide.Runes(text, 40)
}

func newSessionID() string {
	return fmt.Sprintf("s%d", time.Now().UnixNano())
}
