package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reproxy/internal/agent"
	"reproxy/internal/archive"
	"reproxy/internal/config"
	"reproxy/internal/findings"
)

// reviewRunner stands in for the agent: it scores the findings waiting for
// review (up to limit of them, none when limit is 0) and then ends the turn
// with err.
type reviewRunner struct {
	store *findings.Store
	limit int
	err   error
}

func (r reviewRunner) Run(_ context.Context, s agent.Session, _ func(agent.Message)) ([]agent.Entry, error) {
	for i, id := range r.store.AwaitingReview() {
		if i >= r.limit {
			break
		}
		f, _ := r.store.Get(id)
		concern := 10
		if strings.HasPrefix(f.Value, "real") {
			concern = 80
		}
		conf := 90
		r.store.SetConfidence(id, &conf)
		r.store.SetConcern(id, &concern)
	}
	return s.Convo, r.err
}

func reviewerFixture(t *testing.T, run func(*findings.Store) agent.Runner) (*findings.Store, *agent.Manager) {
	t.Helper()
	a, err := archive.Open(filepath.Join(t.TempDir(), archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	store := findings.NewStore()
	if err := store.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	mgr := agent.NewManager(t.TempDir(), run(store))
	r := newFindingsReviewer(mgr, store, nil)
	t.Cleanup(r.stop)
	store.Subscribe(r.onFinding)
	mgr.Subscribe(r.onAgent)
	r.configure(config.Agents{Profiles: []config.AgentProfile{{ID: "p"}}})
	return store, mgr
}

func recordFinding(t *testing.T, s *findings.Store, value string) uint64 {
	t.Helper()
	f, _, err := s.Record(findings.Draft{Value: value, Notes: "n", Source: findings.SourcePlugin, Author: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	return f.ID
}

func waitSettled(t *testing.T, s *findings.Store) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.AwaitingReview()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still awaiting review: %v", s.AwaitingReview())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unread(s *findings.Store, id uint64) bool {
	f, _ := s.Get(id)
	return f.Unread()
}

// With a reviewer, a new finding is unread only once the review has found
// it notable; the rest are marked read.
func TestReviewerAnnouncesOnlyNotableFindings(t *testing.T) {
	s, _ := reviewerFixture(t, func(s *findings.Store) agent.Runner { return reviewRunner{store: s, limit: 100} })
	real, dull := recordFinding(t, s, "real-token"), recordFinding(t, s, "banner")
	waitSettled(t, s)
	if !unread(s, real) || unread(s, dull) {
		t.Fatalf("unread: notable %v, not notable %v", unread(s, real), unread(s, dull))
	}
}

// A turn that fails, or ends without scoring everything it set out to,
// releases what it didn't score unread; findings that arrive during a turn
// wait for the next one.
func TestReviewerReleasesWhatItCouldNotReview(t *testing.T) {
	s, _ := reviewerFixture(t, func(s *findings.Store) agent.Runner {
		return reviewRunner{store: s, limit: 0, err: errors.New("model unavailable")}
	})
	id := recordFinding(t, s, "real-token")
	waitSettled(t, s)
	if !unread(s, id) {
		t.Fatal("a finding the failed turn couldn't review isn't unread")
	}

	gate := make(chan struct{})
	s, mgr := reviewerFixture(t, func(s *findings.Store) agent.Runner {
		return gatedRunner{reviewRunner{store: s, limit: 1}, gate}
	})
	a := recordFinding(t, s, "banner") // starts turn 1, which waits at the gate
	waitFor(t, "the first turn to start", func() bool {
		for _, sum := range mgr.List() {
			if sum.Status == agent.StatusRunning {
				return true
			}
		}
		return false
	})
	b, c := recordFinding(t, s, "real-token"), recordFinding(t, s, "real-key")
	close(gate) // turn 1 scores a; turn 2 is for b and c, and scores only b
	waitSettled(t, s)
	if unread(s, a) || !unread(s, b) || !unread(s, c) {
		t.Fatalf("unread: a %v (want false), b %v, c %v (want true)", unread(s, a), unread(s, b), unread(s, c))
	}
	if f, _ := s.Get(c); f.Confidence != nil {
		t.Fatal("c was scored; the test meant it to be left over")
	}
}

// gatedRunner holds each turn until gate is closed.
type gatedRunner struct {
	reviewRunner
	gate chan struct{}
}

func (g gatedRunner) Run(ctx context.Context, s agent.Session, emit func(agent.Message)) ([]agent.Entry, error) {
	<-g.gate
	return g.reviewRunner.Run(ctx, s, emit)
}
