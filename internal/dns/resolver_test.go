package dns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
)

// fixture is a Resolver over a fake lookup that counts its calls.
type fixture struct {
	r      *Resolver
	store  *capture.Store
	ic     *intercept.Interceptor
	calls  atomic.Int32
	remote atomic.Bool
	now    time.Time
	nowMu  sync.Mutex
}

var zones = map[string][]netip.Addr{
	"app.example.test": {netip.MustParseAddr("10.0.0.12"), netip.MustParseAddr("fd00::12")},
	"alt.example.test": {netip.MustParseAddr("10.0.0.99")},
}

func newFixture(t *testing.T, scope ScopeFunc) *fixture {
	t.Helper()
	fx := &fixture{store: capture.NewStore(), now: time.Unix(1_700_000_000, 0)}
	ic, err := intercept.New(intercept.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	fx.ic = ic
	fx.r = New(Options{
		Store:     fx.store,
		Intercept: ic,
		Scope:     scope,
		Remote:    fx.remote.Load,
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			fx.calls.Add(1)
			if a, ok := zones[host]; ok {
				return slices.Clone(a), nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		},
		Now: func() time.Time {
			fx.nowMu.Lock()
			defer fx.nowMu.Unlock()
			return fx.now
		},
	})
	return fx
}

func (fx *fixture) advance(d time.Duration) {
	fx.nowMu.Lock()
	fx.now = fx.now.Add(d)
	fx.nowMu.Unlock()
}

// holdDNS switches interception on, holding lookups in the given
// directions, and answers each hold with answer (nil forwards unchanged,
// "drop" drops).
func (fx *fixture) holdDNS(dirs map[intercept.Direction]bool, answer func(intercept.Direction, string) *string) {
	fx.ic.SetUserHold(func(dir intercept.Direction, s intercept.Subject) bool { return s.DNS && dirs[dir] })
	fx.ic.SetEnabled(true)
	fx.ic.Subscribe(func(e intercept.HeldEvent) {
		if e.Released {
			return
		}
		go func() {
			m, _ := fx.ic.Message(e.FlowID)
			text := answer(e.Dir, string(m.Raw))
			if text != nil && *text == "drop" {
				_ = fx.ic.Drop(e.FlowID)
				return
			}
			_ = fx.ic.Forward(e.FlowID, text)
		}()
	})
}

func ptr(s string) *string { return &s }

func TestResolveRecordsTheLookup(t *testing.T) {
	fx := newFixture(t, nil)
	ans, err := fx.r.Resolve(context.Background(), "App.Example.Test.", Who{ClientAddr: "127.0.0.1:5000"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.12", "fd00::12"}; !slices.Equal(ans.Hosts, want) {
		t.Fatalf("hosts = %v, want %v", ans.Hosts, want)
	}
	flows := fx.store.List()
	if len(flows) != 1 {
		t.Fatalf("%d flows, want 1", len(flows))
	}
	f := flows[0]
	if !f.IsDNS() || f.Method != "DNS" || f.Host != "app.example.test" || f.State != capture.StateComplete ||
		f.StatusText != "NOERROR" || f.Path != "10.0.0.12, fd00::12" || f.ClientAddr != "127.0.0.1:5000" {
		t.Fatalf("flow = %+v", f)
	}
	if f.URL() != "dns://app.example.test" {
		t.Errorf("URL = %q", f.URL())
	}
	if got := ParseAnswer(f.Response.Raw); len(got) != 2 {
		t.Errorf("answer text %q parses to %v", f.Response.Raw, got)
	}
}

func TestResolveCachesAndSharesLookups(t *testing.T) {
	fx := newFixture(t, nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fx.calls.Load(); n != 1 {
		t.Fatalf("%d lookups for 8 concurrent connections, want 1", n)
	}
	if n := fx.store.Len(); n != 1 {
		t.Fatalf("%d flows, want 1", n)
	}

	fx.advance(DefaultTTL + time.Second)
	if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); err != nil {
		t.Fatal(err)
	}
	if n := fx.calls.Load(); n != 2 {
		t.Fatalf("expired answer not looked up again: %d lookups", n)
	}

	fx.r.Flush()
	_, _ = fx.r.Resolve(context.Background(), "app.example.test", Who{})
	if n := fx.calls.Load(); n != 3 {
		t.Fatalf("flushed answer reused: %d lookups", n)
	}
}

func TestResolveFailureIsRecorded(t *testing.T) {
	fx := newFixture(t, nil)
	_, err := fx.r.Resolve(context.Background(), "missing.example.test", Who{})
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Fatalf("err = %v, want not found", err)
	}
	f := fx.store.List()[0]
	if f.StatusText != "NXDOMAIN" || f.Path != "no addresses" || f.State != capture.StateComplete {
		t.Fatalf("flow = %+v", f)
	}
}

func TestIPAddressesAreNotLookedUp(t *testing.T) {
	fx := newFixture(t, nil)
	for _, h := range []string{"10.1.2.3", "::1"} {
		ans, err := fx.r.Resolve(context.Background(), h, Who{})
		if err != nil || !slices.Equal(ans.Hosts, []string{h}) {
			t.Fatalf("Resolve(%s) = %v, %v", h, ans, err)
		}
	}
	if fx.calls.Load() != 0 || fx.store.Len() != 0 {
		t.Fatal("an IP address was looked up or recorded")
	}
}

func TestRemoteResolutionPassesTheNameOn(t *testing.T) {
	fx := newFixture(t, nil)
	fx.remote.Store(true)
	ans, err := fx.r.Resolve(context.Background(), "app.example.test", Who{Source: capture.SourceReplay})
	if err != nil || !slices.Equal(ans.Hosts, []string{"app.example.test"}) {
		t.Fatalf("Resolve = %v, %v", ans, err)
	}
	if fx.calls.Load() != 0 {
		t.Fatal("looked up locally although the upstream resolves")
	}
	f := fx.store.List()[0]
	if f.StatusText != "UPSTREAM" || f.Source != capture.SourceReplay || f.Path != "resolved by the upstream proxy" {
		t.Fatalf("flow = %+v", f)
	}
}

func TestScope(t *testing.T) {
	ipTarget := netip.MustParsePrefix("10.0.0.0/24")
	fx := newFixture(t, func(host string, ips []netip.Addr) (string, bool) {
		if host == "app.example.test" && len(ips) == 0 {
			return "", false // not by name…
		}
		for _, ip := range ips {
			if ipTarget.Contains(ip) {
				return "net", true // …but by address
			}
		}
		return "", false
	})
	if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.r.Resolve(context.Background(), "missing.example.test", Who{}); err == nil {
		t.Fatal("missing name resolved")
	}
	flows := fx.store.List()
	if len(flows) != 1 || flows[0].TargetID != "net" || flows[0].Host != "app.example.test" {
		t.Fatalf("flows = %+v, want just the lookup the address put in scope", flows)
	}
}

func TestHeldQueryCanBeRenamed(t *testing.T) {
	fx := newFixture(t, nil)
	fx.holdDNS(map[intercept.Direction]bool{intercept.Request: true}, func(_ intercept.Direction, text string) *string {
		return ptr("QUERY alt.example.test DNS\nType: A, AAAA\n\n")
	})
	ans, err := fx.r.Resolve(context.Background(), "app.example.test", Who{})
	if err != nil || !slices.Equal(ans.Hosts, []string{"10.0.0.99"}) {
		t.Fatalf("Resolve = %v, %v; want the renamed lookup's address", ans, err)
	}
	f := fx.store.List()[0]
	if f.Host != "alt.example.test" || f.OriginalRequest == nil || f.State != capture.StateComplete {
		t.Fatalf("flow = %+v", f)
	}
}

func TestHeldAnswerCanBeEditedAndIsCached(t *testing.T) {
	fx := newFixture(t, nil)
	var held atomic.Int32
	fx.holdDNS(map[intercept.Direction]bool{intercept.Response: true}, func(dir intercept.Direction, text string) *string {
		held.Add(1)
		if dir != intercept.Response {
			t.Errorf("held the %v", dir)
		}
		return ptr("DNS NOERROR\nA: 192.0.2.7\n\n")
	})
	for range 3 {
		ans, err := fx.r.Resolve(context.Background(), "app.example.test", Who{})
		if err != nil || !slices.Equal(ans.Hosts, []string{"192.0.2.7"}) {
			t.Fatalf("Resolve = %v, %v; want the edited address", ans, err)
		}
	}
	if n := held.Load(); n != 1 {
		t.Fatalf("held %d times, want once (the edit is cached)", n)
	}
	f := fx.store.List()[0]
	if f.Path != "192.0.2.7" || f.OriginalResponse == nil || !f.Edited() {
		t.Fatalf("flow = %+v", f)
	}
}

func TestHeldAnswerOverridesTheUpstream(t *testing.T) {
	fx := newFixture(t, nil)
	fx.remote.Store(true)
	fx.holdDNS(map[intercept.Direction]bool{intercept.Response: true}, func(_ intercept.Direction, text string) *string {
		return ptr(text + "A: 192.0.2.8\n")
	})
	ans, err := fx.r.Resolve(context.Background(), "app.example.test", Who{})
	if err != nil || !slices.Equal(ans.Hosts, []string{"192.0.2.8"}) {
		t.Fatalf("Resolve = %v, %v; want the address typed into the answer", ans, err)
	}
}

func TestDroppedLookupFailsAndIsNotCached(t *testing.T) {
	fx := newFixture(t, nil)
	fx.holdDNS(map[intercept.Direction]bool{intercept.Request: true}, func(intercept.Direction, string) *string {
		return ptr("drop")
	})
	for range 2 {
		if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); !errors.Is(err, ErrDropped) {
			t.Fatalf("err = %v, want ErrDropped", err)
		}
	}
	flows := fx.store.List()
	if len(flows) != 2 || flows[0].State != capture.StateDropped {
		t.Fatalf("flows = %+v, want two dropped lookups", flows)
	}
}

func TestSwitchingHoldsOnBypassesTheCache(t *testing.T) {
	fx := newFixture(t, nil)
	if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); err != nil {
		t.Fatal(err)
	}
	var held atomic.Int32
	fx.holdDNS(map[intercept.Direction]bool{intercept.Response: true}, func(intercept.Direction, string) *string {
		held.Add(1)
		return nil
	})
	if _, err := fx.r.Resolve(context.Background(), "app.example.test", Who{}); err != nil {
		t.Fatal(err)
	}
	if held.Load() != 1 || fx.calls.Load() != 2 {
		t.Fatalf("held %d, looked up %d: an answer cached before holding was reused", held.Load(), fx.calls.Load())
	}
}

func TestDialerResolvesFirstAndTagsTheSource(t *testing.T) {
	fx := newFixture(t, nil)
	var dialled []string
	dial := fx.r.Dialer(func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		if addr == "[fd00::12]:443" {
			c, _ := net.Pipe()
			return c, nil
		}
		return nil, errors.New("refused")
	})
	ctx := capture.WithSource(context.Background(), capture.SourceAgent, "")
	c, err := dial(ctx, "tcp", "app.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if want := []string{"10.0.0.12:443", "[fd00::12]:443"}; !slices.Equal(dialled, want) {
		t.Fatalf("dialled %v, want %v", dialled, want)
	}
	if f := fx.store.List()[0]; f.Source != capture.SourceAgent {
		t.Fatalf("lookup source = %q, want agent", f.Source)
	}
}

func TestParseAnswerSkipsJunk(t *testing.T) {
	got := ParseAnswer([]byte("DNS NOERROR\r\nA: 10.0.0.1\r\nA: not-an-ip\r\nNote: x\r\naaaa: ::2\r\n\r\n"))
	want := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::2")}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseAnswer = %v, want %v", got, want)
	}
	if _, ok := ParseQuery([]byte("GET / HTTP/1.1\r\n\r\n")); ok {
		t.Fatal("an HTTP request parsed as a query")
	}
}
