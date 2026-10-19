package focus

import (
	"strings"
	"testing"
	"time"
)

func TestRecentFollowsLastTouched(t *testing.T) {
	var tr Tracker
	if s := tr.Snapshot(); s.Recent != "" {
		t.Fatalf("empty: %+v", s)
	}
	tr.SelectFlow(4)
	if s := tr.Snapshot(); s.Recent != "flow" || s.SelectedFlow != 4 {
		t.Fatalf("after select: %+v", s)
	}
	time.Sleep(time.Millisecond)
	tr.ActivateReplay(2, "GET / HTTP/1.1")
	tr.SetReplayDraft(2, "GET /edited HTTP/1.1")
	tr.SetReplayDraft(9, "ignored")
	s := tr.Snapshot()
	if s.Recent != "replay" || s.ReplaySession != 2 || s.ReplayDraft != "GET /edited HTTP/1.1" {
		t.Fatalf("after replay: %+v", s)
	}
	time.Sleep(time.Millisecond)
	tr.SelectFlow(5)
	if s := tr.Snapshot(); s.Recent != "flow" || s.ReplaySession != 2 {
		t.Fatalf("reselect keeps replay: %+v", s)
	}
	tr.CloseReplay(2)
	if s := tr.Snapshot(); s.ReplaySession != 0 || s.ReplayDraft != "" {
		t.Fatalf("after close: %+v", s)
	}
}

func TestAtReportsTheFocusOfThatMoment(t *testing.T) {
	var tr Tracker
	before := time.Now()
	tr.SelectFlow(4)
	asked := time.Now() // the user sends "look at this request"
	tr.ActivateReplay(2, "GET /draft HTTP/1.1")
	tr.SelectFlow(9) // and clicks on while the agent works

	if s, err := tr.At(before); err != nil || s.Recent != "" || s.SelectedFlow != 0 {
		t.Fatalf("before anything: %+v, %v", s, err)
	}
	s, err := tr.At(asked)
	if err != nil || s.SelectedFlow != 4 || s.ReplaySession != 0 || s.Recent != "flow" {
		t.Fatalf("at the message: %+v, %v", s, err)
	}
	if s, _ := tr.At(time.Time{}); s.SelectedFlow != 9 || s.ReplaySession != 2 {
		t.Fatalf("now: %+v", s)
	}
	if s, _ := tr.At(time.Now().Add(time.Hour)); s.SelectedFlow != 9 {
		t.Fatalf("after the last change: %+v", s)
	}
}

func TestHistoryForgetsTheOldestFirst(t *testing.T) {
	var tr Tracker
	tr.SelectFlow(1)
	first := time.Now()
	for i := range maxStates + 10 {
		tr.SelectFlow(uint64(i + 2))
	}
	if _, err := tr.At(first); err != ErrTooOld {
		t.Fatalf("a state past the count bound: %v", err)
	}
	if s, err := tr.At(time.Time{}); err != nil || s.SelectedFlow != maxStates+11 {
		t.Fatalf("current: %+v, %v", s, err)
	}

	// Drafts are bounded by size: big ones push the old states out sooner.
	tr = Tracker{}
	tr.ActivateReplay(1, "small")
	first = time.Now()
	big := strings.Repeat("x", maxDraftsBytes/3)
	for i := range 4 {
		tr.ActivateReplay(uint64(i+2), big)
	}
	if _, err := tr.At(first); err != ErrTooOld {
		t.Fatalf("a state past the draft bound: %v", err)
	}
	if tr.drafts > maxDraftsBytes {
		t.Fatalf("holding %d draft bytes, over %d", tr.drafts, maxDraftsBytes)
	}
	// The current state is kept even when its draft alone is over the bound.
	tr.ActivateReplay(9, strings.Repeat("y", maxDraftsBytes+1))
	if s := tr.Snapshot(); s.ReplaySession != 9 {
		t.Fatalf("current dropped: %+v", s)
	}
}
