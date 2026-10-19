package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"reproxy/internal/capture"
)

// The proxy resolves the name a client asked for through its resolver and
// connects to what that returned, trying each address in turn.
func TestDialsWhatTheResolverReturns(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())

	var asked []string
	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)),
		WithResolver(func(_ context.Context, host, clientAddr string, source capture.Source, _ string) ([]string, error) {
			asked = append(asked, host)
			if clientAddr == "" {
				t.Error("no client address given to the resolver")
			}
			if host == "missing.test" {
				return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			}
			// The first address refuses; the proxy must move on.
			return []string{"127.0.0.1", "127.0.0.1"}, nil
		}),
		WithDialer(func() DialFunc {
			tried := 0
			return func(ctx context.Context, network, addr string) (net.Conn, error) {
				tried++
				if tried == 1 {
					return nil, errors.New("refused")
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
		}()))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	c, rep := socksDial(t, srv.Addr(), net.JoinHostPort("app.test", port))
	if rep != repSucceeded {
		t.Fatalf("reply = %d, want success", rep)
	}
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: app.test\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "hello" {
		t.Fatalf("response body = %q", body)
	}

	if _, rep := socksDial(t, srv.Addr(), "missing.test:80"); rep != repHostUnreachable {
		t.Fatalf("reply for a name that doesn't resolve = %d, want host unreachable", rep)
	}
	if len(asked) != 2 || asked[0] != "app.test" || asked[1] != "missing.test" {
		t.Fatalf("resolver asked %v", asked)
	}
}
