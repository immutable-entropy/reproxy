// Package terminal runs the shells behind the Terminal tab: it starts a
// pseudo-terminal, keeps a virtual screen of what the shell has written, and
// hands the UI a plain snapshot of that screen to draw.
//
// It is core only in the sense that it has to be Go: a plugin can't fork a
// process. Nothing in it touches the proxy's goroutine, the capture store or
// the interceptor — a terminal is a convenience beside the traffic, not part
// of it — and it is Qt-free like every other package but internal/ui, so the
// screen it exposes is ordinary Go values (Screen, Cell, Key) rather than
// the emulator's own types. Later, when reproxy has a command line of its
// own, this is where it will be run.
//
// A Session owns one shell. Everything it holds is behind one mutex: the
// goroutine draining the pty writes the emulator, the UI thread reads it for
// a snapshot and writes keys to it, and neither waits on the other for long.
package terminal

import (
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/x/vt"
)

const (
	// DefaultCols and DefaultRows size a session before the view has been
	// laid out and can say how big it really is.
	DefaultCols = 80
	DefaultRows = 24
	// scrollbackLines is how much of what scrolled off the top is kept for
	// the user to scroll back to.
	scrollbackLines = 5000
	// readChunk is the pty read buffer. Shell output arrives in bursts, so
	// it is big enough that a burst is a few reads rather than hundreds.
	readChunk = 32 << 10
)

// EventKind is what happened to a session.
type EventKind int

const (
	// Added is a new session.
	Added EventKind = iota
	// Output is new output on the screen: the view should repaint. These
	// arrive as fast as the shell writes, so a view must coalesce them.
	Output
	// Titled is a new title (the shell's OSC 0/2, usually the command
	// running or the working directory).
	Titled
	// Exited is the shell ending. The session is removed from the manager
	// just after, with Closed.
	Exited
	// Closed is a session that has gone from the list.
	Closed
)

// Event is one change to a session.
type Event struct {
	Kind EventKind
	ID   string
}

// Options configure a Manager.
type Options struct {
	// Shell is the command to run; empty uses the user's default (see
	// DefaultShell).
	Shell string
	// Dir is where a new session starts, worked out at the time it starts
	// so an engagement opened later is picked up. Empty (or a nil func)
	// starts in the user's home directory.
	Dir func() string
	// Env is added to the environment the shells inherit, as "K=V". TERM
	// and COLORTERM are set for them already.
	Env []string
	// Log receives what went wrong out of sight of the user.
	Log *slog.Logger
}

// Manager owns this process's terminal sessions.
type Manager struct {
	o Options

	mu       sync.Mutex
	sessions []*Session
	seq      int
	nextSub  int
	subs     map[int]func(Event)
}

// NewManager returns a Manager with no sessions.
func NewManager(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Manager{o: o, subs: map[int]func(Event){}}
}

// Subscribe calls fn for every change. fn runs on whichever goroutine made
// the change — the UI must marshal it to its own thread.
func (m *Manager) Subscribe(fn func(Event)) (unsubscribe func()) {
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

func (m *Manager) emit(e Event) {
	m.mu.Lock()
	subs := make([]func(Event), 0, len(m.subs))
	for _, fn := range m.subs {
		subs = append(subs, fn)
	}
	m.mu.Unlock()
	for _, fn := range subs {
		fn(e)
	}
}

// New starts a shell and returns its session. The error is the shell's: a
// terminal that couldn't be started is not listed.
func (m *Manager) New() (*Session, error) {
	shell := m.o.Shell
	if shell == "" {
		shell = DefaultShell()
	}
	dir := ""
	if m.o.Dir != nil {
		dir = m.o.Dir()
	}
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}

	m.mu.Lock()
	m.seq++
	id := strconv.Itoa(m.seq)
	m.mu.Unlock()

	s := &Session{
		id:    id,
		name:  shellName(shell),
		mgr:   m,
		emu:   vt.NewEmulator(DefaultCols, DefaultRows),
		cols:  DefaultCols,
		rows:  DefaultRows,
		log:   m.o.Log,
		ready: make(chan struct{}),
	}
	s.emu.SetScrollbackSize(scrollbackLines)
	s.emu.SetCallbacks(vt.Callbacks{Title: s.onTitle})
	if err := s.start(shell, dir, m.o.Env); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions = append(m.sessions, s)
	m.mu.Unlock()
	m.emit(Event{Kind: Added, ID: id})
	return s, nil
}

// List returns the sessions in the order they were started.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, len(m.sessions))
	copy(out, m.sessions)
	return out
}

// Get returns a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.id == id {
			return s, true
		}
	}
	return nil, false
}

// Close ends a session's shell and takes it off the list.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	var found *Session
	for i, s := range m.sessions {
		if s.id == id {
			found = s
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if found == nil {
		return
	}
	found.stop()
	m.emit(Event{Kind: Closed, ID: id})
}

// CloseAll ends every session, for when the window closes.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := m.sessions
	m.sessions = nil
	m.mu.Unlock()
	for _, s := range all {
		s.stop()
	}
}

// remove takes an exited session off the list, as Close does for the user's.
func (m *Manager) remove(id string) {
	m.mu.Lock()
	for i, s := range m.sessions {
		if s.id == id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	m.emit(Event{Kind: Closed, ID: id})
}

// DefaultShell is the shell a session runs: the user's own if the
// environment says what it is, else the first ordinary one on the system.
func DefaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if runtime.GOOS == "windows" {
		if sh := os.Getenv("COMSPEC"); sh != "" {
			return sh
		}
		return "powershell.exe"
	}
	for _, sh := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// shellName is what a shell is called in the Sessions list.
func shellName(shell string) string {
	name := shell
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".exe")
}

// shellArgs is how the shell is started. On macOS a GUI process inherits
// almost no environment, so the shell is run as a login shell to pick up the
// user's PATH; elsewhere the environment is already the user's.
func shellArgs(shell string) []string {
	if runtime.GOOS == "darwin" && shellName(shell) != "sh" {
		return []string{"-l"}
	}
	return nil
}

// environ is the environment a shell runs in: this process's, with the
// terminal described and the caller's additions last.
func environ(extra []string) []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "TERM="), strings.HasPrefix(kv, "COLORTERM="),
			strings.HasPrefix(kv, "TERM_PROGRAM="), strings.HasPrefix(kv, "LINES="),
			strings.HasPrefix(kv, "COLUMNS="):
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=reproxy")
	return append(out, extra...)
}

// Session is one shell and the screen it is writing to.
type Session struct {
	id   string
	name string
	mgr  *Manager
	log  *slog.Logger

	mu         sync.Mutex
	emu        *vt.Emulator
	cols, rows int
	title      string
	exited     bool

	pty     *os.File
	cmd     *exec.Cmd
	stopped atomic.Bool
	ready   chan struct{} // closed once the shell is running
	once    sync.Once
}

// ID identifies the session for the manager's events.
func (s *Session) ID() string { return s.id }

// Name is the shell's name, for the Sessions list.
func (s *Session) Name() string { return s.name }

// Title is what the shell has set as the window title, or "".
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// Exited reports whether the shell has ended.
func (s *Session) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

// Size is the screen the shell believes it has.
func (s *Session) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// ScrollbackLen is how many lines have scrolled off the top and are still
// kept.
func (s *Session) ScrollbackLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.ScrollbackLen()
}

// OnAltScreen reports whether a full-screen program (an editor, a pager) is
// on the screen: the scroll wheel belongs to it then, not to the scrollback.
func (s *Session) OnAltScreen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.IsAltScreen()
}

func (s *Session) onTitle(title string) {
	s.mu.Lock()
	changed := s.title != title
	s.title = title
	s.mu.Unlock()
	if changed {
		s.mgr.emit(Event{Kind: Titled, ID: s.id})
	}
}
