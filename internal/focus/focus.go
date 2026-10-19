// Package focus records what the user is looking at in one reproxy process,
// so an agent can resolve "this request" in a chat message: the flow
// selected in the request list and the Replay session they last worked in,
// with its unsent edits.
//
// The UI updates a Tracker as the user moves around; agents read it only
// when they ask (the get_user_focus tool), never on every turn. Because the
// user goes on clicking while an agent works, "this request" means what was
// focused when they sent the message, not when the agent gets round to
// looking: the Tracker keeps a bounded history of its states, and At answers
// for any moment still in it.
//
// Typing in the Agents chat moves keyboard focus to the main window, so the
// last-active Replay session is kept (with when it was active) rather than
// cleared when its window loses focus; it is cleared only when that window
// closes.
package focus

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrTooOld is returned by At for a moment older than the history kept.
var ErrTooOld = errors.New("the focus at that time is no longer remembered")

// History bounds. A state is a few dozen bytes plus its Replay draft, and
// drafts change only when the user moves between Replay tabs or sections,
// so these keep many minutes of ordinary use; holding an arrow key in the
// request list is what spends the count.
const (
	maxStates      = 1000
	maxDraftsBytes = 4 << 20
)

// Snapshot is the user's focus at one moment. Zero IDs mean "none".
type Snapshot struct {
	// SelectedFlow is the flow selected in the main request list.
	SelectedFlow uint64
	SelectedAt   time.Time

	// ReplaySession is the Replay session whose window was last active.
	ReplaySession uint64
	ReplayAt      time.Time
	// ReplayDraft is that window's request editor text, which may hold
	// unsent edits.
	ReplayDraft string

	// Recent reports which of the two the user touched last: "flow",
	// "replay", or "" when neither is set.
	Recent string

	// Since is when the user's focus became this (zero before anything was
	// focused).
	Since time.Time
}

// Tracker holds the current Snapshot and the ones before it. It is safe for
// concurrent use.
type Tracker struct {
	mu sync.Mutex
	// states are the focus's states, oldest first; the last is current.
	states []Snapshot
	// drafts is the bytes of ReplayDraft held across states.
	drafts int
	// forgotten is the Since of the oldest state kept once older ones have
	// been dropped: anything before it is unknown.
	forgotten time.Time
}

// SelectFlow records the request-list selection (0 for none).
func (t *Tracker) SelectFlow(id uint64) {
	t.change(func(s *Snapshot, now time.Time) bool {
		s.SelectedFlow, s.SelectedAt = id, now
		return true
	})
}

// ActivateReplay records that a Replay window became active, with its
// current request text.
func (t *Tracker) ActivateReplay(sessionID uint64, draft string) {
	t.change(func(s *Snapshot, now time.Time) bool {
		s.ReplaySession, s.ReplayAt, s.ReplayDraft = sessionID, now, draft
		return true
	})
}

// SetReplayDraft updates the editor text of a Replay session, if it is the
// tracked one.
func (t *Tracker) SetReplayDraft(sessionID uint64, draft string) {
	t.change(func(s *Snapshot, _ time.Time) bool {
		if s.ReplaySession != sessionID || s.ReplayDraft == draft {
			return false
		}
		s.ReplayDraft = draft
		return true
	})
}

// CloseReplay forgets a Replay session whose window closed.
func (t *Tracker) CloseReplay(sessionID uint64) {
	t.change(func(s *Snapshot, _ time.Time) bool {
		if s.ReplaySession != sessionID {
			return false
		}
		s.ReplaySession, s.ReplayAt, s.ReplayDraft = 0, time.Time{}, ""
		return true
	})
}

// Snapshot returns the current focus.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.states) == 0 {
		return Snapshot{}
	}
	return t.states[len(t.states)-1].withRecent()
}

// At returns the focus as it was at the given moment; the zero time means
// now. It returns ErrTooOld for a moment before the oldest state kept.
func (t *Tracker) At(at time.Time) (Snapshot, error) {
	if at.IsZero() {
		return t.Snapshot(), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.forgotten.IsZero() && at.Before(t.forgotten) {
		return Snapshot{}, ErrTooOld
	}
	// The last state that began at or before the moment.
	i := sort.Search(len(t.states), func(i int) bool { return t.states[i].Since.After(at) })
	if i == 0 {
		return Snapshot{}, nil // nothing was focused yet
	}
	return t.states[i-1].withRecent(), nil
}

// change applies edit to a copy of the current state and, if edit reports
// a change, records the copy as the new current state.
func (t *Tracker) change(edit func(s *Snapshot, now time.Time) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now() // under the lock, so states stay in time order
	var s Snapshot
	if len(t.states) > 0 {
		s = t.states[len(t.states)-1]
	}
	if !edit(&s, now) {
		return
	}
	s.Since, s.Recent = now, ""
	t.states = append(t.states, s)
	t.drafts += len(s.ReplayDraft)
	// Drop the oldest states past either bound, always keeping the current.
	drop := 0
	for len(t.states)-drop > 1 && (len(t.states)-drop > maxStates || t.drafts > maxDraftsBytes) {
		t.drafts -= len(t.states[drop].ReplayDraft)
		drop++
	}
	if drop > 0 {
		t.forgotten = t.states[drop].Since
		t.states = append(t.states[:0], t.states[drop:]...)
	}
}

func (s Snapshot) withRecent() Snapshot {
	switch {
	case s.ReplaySession != 0 && (s.SelectedFlow == 0 || s.ReplayAt.After(s.SelectedAt)):
		s.Recent = "replay"
	case s.SelectedFlow != 0:
		s.Recent = "flow"
	}
	return s
}
