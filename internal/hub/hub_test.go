package hub

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/focus"
	"reproxy/internal/instance"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

type member struct {
	node  *Node
	store *capture.Store
	roles chan bool
}

func join(t *testing.T, dir string) *member {
	t.Helper()
	ic, _ := intercept.New(intercept.DefaultSettings())
	m := &member{store: capture.NewStore(), roles: make(chan bool, 4)}
	local := instance.NewLocal(instance.Info{ID: NewInstanceID(), Started: time.Now()},
		m.store, ic, replay.NewSessions(nil), &focus.Tracker{}, nil)
	n, err := Start(Options{
		DataDir:      dir,
		Local:        local,
		Token:        func() (string, error) { return "tok", nil },
		Logger:       slog.New(slog.DiscardHandler),
		OnRoleChange: func(h bool) { m.roles <- h },
		Poll:         20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.node = n
	t.Cleanup(func() { n.Close() })
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOneHubSeesEveryInstanceAndFailsOver(t *testing.T) {
	dir := t.TempDir()
	a := join(t, dir)
	time.Sleep(5 * time.Millisecond) // let a start first (ordering in Instances)
	b := join(t, dir)
	c := join(t, dir)

	if !a.node.IsHub() || b.node.IsHub() || c.node.IsHub() {
		t.Fatalf("hub roles: a=%v b=%v c=%v", a.node.IsHub(), b.node.IsHub(), c.node.IsHub())
	}
	if !<-a.roles {
		t.Fatal("a not told it became hub")
	}
	waitFor(t, "peers registered", func() bool { return len(a.node.Router().Peers()) == 2 })

	// The hub reaches a peer's traffic through the router.
	b.store.Add(capture.Flow{Method: "GET", Host: "b.test", Path: "/from-b"})
	ctx := context.Background()
	inst, ok := a.node.Router().Get(b.node.ID())
	if !ok {
		t.Fatal("b not routable")
	}
	page, err := inst.ListFlows(ctx, instance.FlowQuery{})
	if err != nil || len(page.Flows) != 1 || page.Flows[0].URL != "http://b.test/from-b" {
		t.Fatalf("b's flows via hub = %+v, %v", page, err)
	}
	if infos := a.node.Router().Instances(ctx); len(infos) != 3 || infos[0].ID != a.node.ID() {
		t.Fatalf("instances = %+v", infos)
	}

	// The hub leaves: exactly one of b and c takes over, and the other
	// joins it.
	a.node.Close()
	waitFor(t, "new hub", func() bool { return b.node.IsHub() || c.node.IsHub() })
	newHub, other := b, c
	if c.node.IsHub() {
		newHub, other = c, b
	}
	if other.node.IsHub() {
		t.Fatal("two hubs")
	}
	waitFor(t, "re-registration", func() bool {
		_, id, _ := other.node.Hub()
		return id == newHub.node.ID() && len(newHub.node.Router().Peers()) == 1
	})
	if _, ok := newHub.node.Router().Get(other.node.ID()); !ok {
		t.Fatal("new hub can't reach the remaining peer")
	}
	if _, ok := newHub.node.Router().Get(a.node.ID()); ok {
		t.Fatal("departed hub still routable")
	}
}

func TestHubRoutesNeedTokenAndHubRole(t *testing.T) {
	dir := t.TempDir()
	a := join(t, dir)
	b := join(t, dir)
	called := false
	b.node.HandleHub("GET /hub/ping", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		io.WriteString(w, "pong")
	}))
	a.node.HandleHub("GET /hub/ping", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pong")
	}))

	get := func(addr, tok string) int {
		req, _ := http.NewRequest("GET", "http://"+addr+"/hub/ping", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := get(a.node.Addr(), ""); s != http.StatusUnauthorized {
		t.Fatalf("no token: %d", s)
	}
	if s := get(a.node.Addr(), "tok"); s != http.StatusOK {
		t.Fatalf("hub with token: %d", s)
	}
	if s := get(b.node.Addr(), "tok"); s != http.StatusMisdirectedRequest || called {
		t.Fatalf("non-hub: %d, called=%v", s, called)
	}
}
