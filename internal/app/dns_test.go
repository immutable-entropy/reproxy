package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/target"
)

// A browser asking the proxy for a name has the lookup made, and recorded,
// by reproxy.
func TestProxiedNamesAreLookedUpAndRecorded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())

	a, _ := newApp(t, freePort(t))
	a.Start()
	addr, err := a.ProxyStatus()
	if err != nil {
		t.Fatal(err)
	}
	c, err := upstreamDial(addr, net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "hello" {
		t.Fatalf("response body = %q", body)
	}

	var lookup capture.Flow
	waitFor(t, "the lookup to be recorded", func() bool {
		for _, f := range a.Store().List() {
			if f.IsDNS() && f.State == capture.StateComplete {
				lookup = f
				return true
			}
		}
		return false
	})
	if lookup.Host != "localhost" || lookup.StatusText != "NOERROR" || !strings.Contains(lookup.Path, "127.0.0.1") {
		t.Fatalf("lookup = %+v", lookup)
	}
}

// Lookups are held only for a target that opts in, as its requests and
// responses choice says, and never just because no targets are set up.
func TestDNSHoldsFollowTheTarget(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	q := intercept.Subject{DNS: true, Host: "app.example.test"}
	if a.interceptHold(intercept.Request, q) {
		t.Fatal("a lookup was held with no targets")
	}

	tg, err := a.targets.Create(target.KindDomain, "app.example.test")
	if err != nil {
		t.Fatal(err)
	}
	tg.Intercept = target.Intercept{Requests: true, Responses: false}
	if err := a.targets.Save(tg); err != nil {
		t.Fatal(err)
	}
	if a.interceptHold(intercept.Request, q) {
		t.Fatal("a lookup was held for a target without DNS holds")
	}
	if !a.interceptHold(intercept.Request, intercept.Subject{Host: "app.example.test", Path: "/"}) {
		t.Fatal("the target's HTTP requests aren't held")
	}

	tg.Intercept.DNS = true
	if err := a.targets.Save(tg); err != nil {
		t.Fatal(err)
	}
	if !a.interceptHold(intercept.Request, q) {
		t.Fatal("the query isn't held although the target holds requests and DNS")
	}
	if a.interceptHold(intercept.Response, q) {
		t.Fatal("the answer is held although the target doesn't hold responses")
	}
}

// Replays resolve through reproxy as well, listed as the replay's.
func TestReplayLookupsAreTheReplays(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	ctx := capture.WithSource(context.Background(), capture.SourceReplay, "")
	c, err := a.dialResolved(ctx, "tcp", "localhost:1") // refused; the lookup is what matters
	if err == nil {
		c.Close()
	}
	for _, f := range a.Store().List() {
		if f.IsDNS() && f.Source == capture.SourceReplay {
			return
		}
	}
	t.Fatalf("no replay lookup recorded: %+v", a.Store().List())
}
