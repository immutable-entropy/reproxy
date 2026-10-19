package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"reproxy/internal/capture"
)

// recordingDialer stands in for an upstream proxy chain: it notes every
// address it was asked for and then connects there itself.
type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func (d *recordingDialer) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// TestServerUsesConfiguredDialer checks that upstream connections go
// through the dialer, which is what puts the upstream proxy chain in front
// of every target.
func TestServerUsesConfiguredDialer(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	d := &recordingDialer{}
	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)), WithDialer(d.dial))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("SOCKS reply = %d", rep)
	}
	conn.Close()

	if seen := d.seen(); len(seen) != 1 || seen[0] != target {
		t.Errorf("dialer saw %v, want [%s]", seen, target)
	}
}

// TestSetDialerSwapsLive covers a settings change: the next connection uses
// the new chain, and clearing it goes back to connecting directly.
func TestSetDialerSwapsLive(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	target := strings.TrimPrefix(origin.URL, "http://")

	srv, _ := startProxy(t)
	first, second := &recordingDialer{}, &recordingDialer{}

	srv.SetDialer(first.dial)
	c1, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("SOCKS reply = %d", rep)
	}
	c1.Close()

	srv.SetDialer(second.dial)
	c2, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("SOCKS reply = %d", rep)
	}
	c2.Close()

	srv.SetDialer(nil)
	c3, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("SOCKS reply = %d", rep)
	}
	c3.Close()

	if n := len(first.seen()); n != 1 {
		t.Errorf("first dialer used %d times, want 1", n)
	}
	if n := len(second.seen()); n != 1 {
		t.Errorf("second dialer used %d times, want 1", n)
	}
}
