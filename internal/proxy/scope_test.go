package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"reproxy/internal/capture"
)

// TestScopeSkipsOutOfScopeTraffic checks the target scope rule: a request
// the scope function rejects is still relayed, and leaves nothing behind.
func TestScopeSkipsOutOfScopeTraffic(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "served %s", r.URL.Path)
	}))
	defer app.Close()

	store := capture.NewStore()
	srv := New(store,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithScope(func(host string, _ netip.Addr, path string) (string, bool) {
			if strings.HasPrefix(path, "/in-scope") {
				return "target-1", true
			}
			return "", false
		}))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	target := strings.Replace(app.Listener.Addr().String(), "127.0.0.1", "localhost", 1)
	conn, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("reply = %d", rep)
	}

	br := bufio.NewReader(conn)
	send := func(path string) string {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: app.local\r\n\r\n", path); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}

	// Out of scope first: the browser still gets its answer, and the tunnel
	// keeps working for the in-scope request that follows on it.
	if got := send("/private/secret"); got != "served /private/secret" {
		t.Errorf("out-of-scope body = %q", got)
	}
	if got := send("/in-scope/orders"); got != "served /in-scope/orders" {
		t.Errorf("in-scope body = %q", got)
	}
	if got := send("/private/again"); got != "served /private/again" {
		t.Errorf("second out-of-scope body = %q", got)
	}

	waitFor(t, "the in-scope flow", completed(store, 1))
	flows := store.List()
	if len(flows) != 1 {
		t.Fatalf("recorded %d flows, want only the in-scope one: %+v", len(flows), flows)
	}
	if f := flows[0]; f.Path != "/in-scope/orders" || f.TargetID != "target-1" {
		t.Errorf("flow = %+v", f)
	}
}

// TestNoScopeRecordsEverything is the default: without a scope function every
// request is recorded and no flow claims a target.
func TestNoScopeRecordsEverything(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()

	srv, store := startProxy(t)
	target := strings.Replace(app.Listener.Addr().String(), "127.0.0.1", "localhost", 1)
	conn, _ := socksDial(t, srv.Addr(), target)

	br := bufio.NewReader(conn)
	for _, path := range []string{"/a", "/b"} {
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: app.local\r\n\r\n", path)
		resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	waitFor(t, "2 completed flows", completed(store, 2))
	for _, f := range store.List() {
		if f.TargetID != "" {
			t.Errorf("flow %s carries target %q", f.Path, f.TargetID)
		}
	}
}
