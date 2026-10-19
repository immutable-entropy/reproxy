package replay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSendHTTP(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.Path, body)
	}))
	defer app.Close()

	raw := "POST /replay HTTP/1.1\r\nHost: app.local\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello"
	resp, err := Send(context.Background(), Request{Addr: strings.TrimPrefix(app.URL, "http://"), Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasSuffix(string(resp.Raw), "POST /replay body=hello") {
		t.Fatalf("resp = %d %q", resp.StatusCode, resp.Raw)
	}
	if !strings.HasPrefix(string(resp.Raw), "HTTP/1.1 200 OK\r\n") {
		t.Errorf("raw response missing status line: %q", resp.Raw)
	}
	if resp.Size != len(resp.Raw) {
		t.Errorf("size %d != len %d", resp.Size, len(resp.Raw))
	}
}

func TestSendHTTPSNoVerify(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure ok")
	}))
	defer app.Close()

	raw := "GET / HTTP/1.1\r\nHost: stage.local\r\nConnection: close\r\n\r\n"
	resp, err := Send(context.Background(), Request{
		Addr:   strings.TrimPrefix(app.URL, "https://"),
		Secure: true,
		Raw:    []byte(raw),
	})
	if err != nil {
		t.Fatalf("replay over TLS (self-signed) failed: %v", err)
	}
	if resp.StatusCode != 200 || !strings.HasSuffix(string(resp.Raw), "secure ok") {
		t.Fatalf("resp = %d %q", resp.StatusCode, resp.Raw)
	}
}

func TestSendKeepAliveCapturesExactlyOneResponse(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "one")
	}))
	defer app.Close()

	// No Connection: close — the server keeps the connection open, so the
	// captured response must stop at the end of the body, not hang or
	// over-read.
	raw := "GET / HTTP/1.1\r\nHost: app.local\r\n\r\n"
	resp, err := Send(context.Background(), Request{Addr: strings.TrimPrefix(app.URL, "http://"), Raw: []byte(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(resp.Raw), "one") || strings.Count(string(resp.Raw), "HTTP/1.1") != 1 {
		t.Fatalf("captured = %q", resp.Raw)
	}
}

func TestSendTimesOut(t *testing.T) {
	ln := newHangingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Send(ctx, Request{Addr: ln, Raw: []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestSendConnectionRefused(t *testing.T) {
	_, err := Send(context.Background(), Request{Addr: "127.0.0.1:1", Raw: []byte("GET / HTTP/1.1\r\n\r\n")})
	if err == nil {
		t.Fatal("expected a connection error")
	}
}

// newHangingServer returns the address of a server that accepts a connection
// and then never replies.
func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

func newHangingServer(t *testing.T) string {
	t.Helper()
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold open, never respond
		}
	}()
	return ln.Addr().String()
}

// TestSendUsesDial checks that a replay goes out through the dialer it is
// given, which is how it reaches a target only visible behind the
// configured upstream proxy chain.
func TestSendUsesDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hi"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var mu sync.Mutex
	var dialed []string
	resp, err := Send(context.Background(), Request{
		Addr: addr,
		Raw:  []byte("GET / HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n"),
		Dial: func(ctx context.Context, network, a string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, a)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, a)
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != addr {
		t.Errorf("Dial saw %v, want [%s]", dialed, addr)
	}
}

func TestSendRefusesAResponsePastTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 10000))
	}))
	defer srv.Close()
	raw := []byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	if _, err := Send(context.Background(), Request{Addr: srv.Listener.Addr().String(), Raw: raw, Limit: 1000}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Send(context.Background(), Request{Addr: srv.Listener.Addr().String(), Raw: raw, Limit: 100000}); err != nil {
		t.Fatal(err)
	}
}
