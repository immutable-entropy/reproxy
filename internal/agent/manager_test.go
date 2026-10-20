package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// scriptRunner emits a fixed set of messages, then optionally blocks until
// ctx is cancelled (to test Stop).
type scriptRunner struct {
	emit  []Message
	block bool
}

func (r scriptRunner) Run(ctx context.Context, s Session, emit func(Message)) ([]Entry, error) {
	for _, m := range r.emit {
		emit(m)
	}
	if r.block {
		<-ctx.Done()
		return s.Convo, ctx.Err()
	}
	return append(s.Convo, Entry{Role: EntryAssistant, Text: "ok"}), nil
}

func collect(m *Manager) (*[]Event, *sync.Mutex, func()) {
	var mu sync.Mutex
	var events []Event
	unsub := m.Subscribe(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	return &events, &mu, unsub
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSendRunsTurnAndPersists(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, scriptRunner{emit: []Message{{Role: RoleAssistant, Text: "hi there"}}})
	events, mu, _ := collect(m)

	sum := m.Create("p1", "")
	if err := m.Send(sum.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn idle", func() bool {
		s, _ := m.Get(sum.ID)
		return s.Status == StatusIdle && len(s.Messages) == 2
	})
	s, _ := m.Get(sum.ID)
	if s.Messages[0].Role != RoleUser || s.Messages[1].Text != "hi there" || s.Title != "hello" {
		t.Fatalf("transcript = %+v, title %q", s.Messages, s.Title)
	}

	// A fresh Manager on the same dir reloads the session, idle.
	m2 := NewManager(dir, nil)
	s2, ok := m2.Get(sum.ID)
	if !ok || len(s2.Messages) != 2 || s2.Status != StatusIdle {
		t.Fatalf("reloaded = %+v, %v", s2, ok)
	}

	mu.Lock()
	defer mu.Unlock()
	var kinds []EventKind
	for _, e := range *events {
		kinds = append(kinds, e.Kind)
	}
	// added, (send:)changed, message(user), message(assistant), (done:)changed
	if len(kinds) < 4 || kinds[0] != SessionAdded {
		t.Fatalf("events = %v", kinds)
	}
}

func TestStopCancelsTurnAndCloseRemoves(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, scriptRunner{emit: []Message{{Role: RoleAssistant, Text: "working"}}, block: true})
	sum := m.Create("p1", "task")
	m.Send(sum.ID, "go")
	waitFor(t, "running", func() bool { s, _ := m.Get(sum.ID); return s.Status == StatusRunning })

	m.Stop(sum.ID)
	waitFor(t, "stopped", func() bool { s, _ := m.Get(sum.ID); return s.Status == StatusIdle })
	s, _ := m.Get(sum.ID)
	if last := s.Messages[len(s.Messages)-1]; last.Role != RoleSystem || last.Text != "Stopped." {
		t.Fatalf("last message = %+v", last)
	}

	m.Close(sum.ID)
	if _, ok := m.Get(sum.ID); ok {
		t.Fatal("session still present after close")
	}
	if err := m.Send(sum.ID, "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("send after close = %v", err)
	}
}

func TestSendWithoutRunnerExplains(t *testing.T) {
	m := NewManager(t.TempDir(), nil)
	sum := m.Create("p1", "")
	m.Send(sum.ID, "hello")
	waitFor(t, "notice", func() bool { s, _ := m.Get(sum.ID); return len(s.Messages) == 2 })
	s, _ := m.Get(sum.ID)
	if s.Messages[1].Role != RoleSystem {
		t.Fatalf("expected system notice, got %+v", s.Messages[1])
	}
}

// A managed session carries the flag in its summary and across a reload, so
// the UI can tell which sessions it must refuse to close. Close still removes
// it — the refusal lives in the UI — so the service that owns it can take it
// down when its config turns off.
func TestSetManagedFlagPersistsAndCloseStillWorks(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, nil)
	sum := m.Create("p1", "Findings review")
	if sum.Managed {
		t.Fatal("a new session is managed")
	}
	if err := m.SetManaged(sum.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := m.List(); len(got) != 1 || !got[0].Managed {
		t.Fatalf("summary didn't report managed: %+v", got)
	}

	// It survives a reload (persisted), so a managed session can't be reopened
	// as closeable after a restart.
	m2 := NewManager(dir, nil)
	if got := m2.List(); len(got) != 1 || !got[0].Managed {
		t.Fatalf("managed flag lost on reload: %+v", got)
	}

	// Close removes it regardless, so its owner can.
	m2.Close(sum.ID)
	if len(m2.List()) != 0 {
		t.Fatal("Close didn't remove the managed session")
	}
}

// Instructions persist with the session, and ClearContext empties what the
// next turn is given while keeping the transcript the user reads.
func TestInstructionsPersistAndClearContextKeepsTranscript(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, scriptRunner{emit: []Message{{Role: RoleAssistant, Text: "scored"}}})
	sum := m.Create("p1", "Findings review")
	if err := m.SetInstructions(sum.ID, "Score them."); err != nil {
		t.Fatal(err)
	}
	m.Send(sum.ID, "round 1")
	waitFor(t, "idle", func() bool { s, _ := m.Get(sum.ID); return s.Status == StatusIdle && len(s.Messages) == 2 })
	if s, _ := m.Get(sum.ID); len(s.Convo) != 2 {
		t.Fatalf("convo before clear = %+v", s.Convo)
	}

	if err := m.ClearContext(sum.ID); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(dir, nil) // and it stays cleared across a reload
	s, _ := m2.Get(sum.ID)
	if len(s.Convo) != 0 || len(s.Messages) != 2 || s.Instructions != "Score them." {
		t.Fatalf("after clear: convo %+v, %d messages, instructions %q", s.Convo, len(s.Messages), s.Instructions)
	}
	if err := m.ClearContext("nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("clear unknown = %v", err)
	}
}

// A running turn's context isn't pulled from under it.
func TestClearContextRefusesWhileRunning(t *testing.T) {
	m := NewManager(t.TempDir(), scriptRunner{block: true})
	sum := m.Create("p1", "x")
	m.Send(sum.ID, "go")
	waitFor(t, "running", func() bool { s, _ := m.Get(sum.ID); return s.Status == StatusRunning })
	if err := m.ClearContext(sum.ID); err == nil {
		t.Fatal("cleared a running session")
	}
	m.Stop(sum.ID)
	// The turn saves the session as it ends; let it, before the directory goes.
	waitFor(t, "stopped", func() bool { s, _ := m.Get(sum.ID); return s.Status == StatusIdle })
}

// TestTitleFromKeepsRunesWhole covers the one place the byte-slicing bug
// was persisted rather than only displayed: a session title is written to
// the sessions file, and json.Marshal silently substitutes U+FFFD for a
// split rune, so the corruption survived a restart.
func TestTitleFromKeepsRunesWhole(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("日", 100), // 3 bytes per rune
		strings.Repeat("é", 100), // 2
		strings.Repeat("🔒", 100), // 4
		"ascii is fine too, and long enough to be cut by the forty-rune limit",
	} {
		title := titleFrom(in)
		if !utf8.ValidString(title) {
			t.Errorf("titleFrom(%.8q…) is not valid UTF-8: % x", in, title)
		}
		if strings.ContainsRune(title, '�') {
			t.Errorf("titleFrom(%.8q…) contains U+FFFD: %q", in, title)
		}
		if n := utf8.RuneCountInString(title); n > 40 {
			t.Errorf("titleFrom(%.8q…) is %d runes, over the 40-rune limit", in, n)
		}
		// A round trip through the sessions file must not change it.
		data, err := json.Marshal(Session{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		var back Session
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if back.Title != title {
			t.Errorf("title changed through JSON: %q -> %q", title, back.Title)
		}
	}
	if got := titleFrom("  \n  "); got != untitled {
		t.Errorf("titleFrom(blank) = %q, want %q", got, untitled)
	}
}
