package approval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitPending(t *testing.T, b *Broker, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if p := b.Pending(); len(p) == n {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d pending, have %d", n, len(b.Pending()))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestApproveDenyAndFirstDecisionWins(t *testing.T) {
	b := NewBroker(0)
	var mu sync.Mutex
	var events []Event
	b.Subscribe(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })

	res := make(chan bool)
	go func() { ok, _ := b.Ask(context.Background(), Request{Session: "s1", Tool: "replay_send"}); res <- ok }()
	p := waitPending(t, b, 1)
	if err := b.Decide(p[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if !<-res {
		t.Fatal("approval not delivered")
	}
	if err := b.Decide(p[0].ID, false); !errors.Is(err, ErrUnknown) {
		t.Fatalf("second decision = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || !events[1].Resolved || !events[1].Approved {
		t.Fatalf("events = %+v", events)
	}
}

func TestTimeoutContextAndDenyAll(t *testing.T) {
	b := NewBroker(20 * time.Millisecond)
	if ok, err := b.Ask(context.Background(), Request{Tool: "x"}); ok || err != nil {
		t.Fatalf("timeout = %v, %v", ok, err)
	}

	b = NewBroker(0)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error)
	go func() { _, err := b.Ask(ctx, Request{Tool: "x"}); errc <- err }()
	waitPending(t, b, 1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) || len(b.Pending()) != 0 {
		t.Fatalf("cancel = %v, pending %d", err, len(b.Pending()))
	}

	res := make(chan bool, 2)
	go func() { ok, _ := b.Ask(context.Background(), Request{Session: "s1"}); res <- ok }()
	go func() { ok, _ := b.Ask(context.Background(), Request{Session: "s2"}); res <- ok }()
	waitPending(t, b, 2)
	b.DenyAll("s1")
	if <-res {
		t.Fatal("DenyAll approved")
	}
	if p := waitPending(t, b, 1); p[0].Session != "s2" {
		t.Fatalf("remaining = %+v", p)
	}
}
