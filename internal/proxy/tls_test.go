package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/tlsca"
)

// startTLSProxy starts a proxy with HTTPS interception enabled and returns a
// cert pool trusting its CA.
func startTLSProxy(t *testing.T, ic *intercept.Interceptor) (*Server, *capture.Store, *x509.CertPool) {
	t.Helper()
	ca, err := tlsca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := capture.NewStore()
	opts := []Option{WithCA(ca), WithLogger(slog.New(slog.DiscardHandler))}
	if ic != nil {
		opts = append(opts, WithInterceptor(ic))
	}
	srv := New(store, opts...)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	return srv, store, pool
}

// socksTLSDial opens a tunnel to target through the proxy and completes a TLS
// handshake over it, trusting pool and sending sni as the server name.
func socksTLSDial(t *testing.T, proxyAddr, target, sni string, pool *x509.CertPool) *tls.Conn {
	t.Helper()
	raw, rep := socksDial(t, proxyAddr, target)
	if rep != repSucceeded {
		t.Fatalf("SOCKS reply = %d", rep)
	}
	conn := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: sni})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake (CA not trusted?): %v", err)
	}
	return conn
}

func TestInterceptsHTTPSAndRecordsPlaintext(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s secret=%s", r.Method, r.URL.Path, body)
	}))
	defer app.Close()
	target := app.Listener.Addr().String()

	srv, store, pool := startTLSProxy(t, nil)
	conn := socksTLSDial(t, srv.Addr(), target, "stage.example.com", pool)

	io.WriteString(conn, "POST /login HTTP/1.1\r\nHost: stage.example.com\r\nContent-Length: 5\r\n\r\nhunter")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasSuffix(string(body), "secret=hunte") && !strings.Contains(string(body), "secret=") {
		t.Fatalf("response body = %q", body)
	}

	waitFor(t, "completed https flow", completed(store, 1))
	f := store.List()[0]
	if !f.Secure || f.Host != "stage.example.com" || f.URL() != "https://stage.example.com/login" {
		t.Fatalf("flow = %+v url=%s", f, f.URL())
	}
	if !strings.Contains(string(f.Request.Raw), "POST /login") {
		t.Errorf("request not recorded in plaintext: %q", f.Request.Raw)
	}
	if f.StatusCode != 200 {
		t.Errorf("status = %d", f.StatusCode)
	}
}

func TestHTTPSWithoutCAIsTunnelledNotRecorded(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()

	// A proxy with no CA: TLS should pass through to the real server, whose
	// own cert the client must trust.
	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	raw, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())
	pool := x509.NewCertPool()
	pool.AddCert(app.Certificate())
	conn := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "example.com"})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("passthrough handshake failed: %v", err)
	}
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	resp, _ := http.ReadResponse(bufio.NewReader(conn), nil)
	io.Copy(io.Discard, resp.Body)
	if store.Len() != 0 {
		t.Fatalf("recorded %d flows for passed-through TLS", store.Len())
	}
}

func TestInterceptsAndEditsHeldHTTPS(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer app.Close()

	ic, _ := intercept.New(intercept.DefaultSettings())
	ic.SetEnabled(true)
	srv, store, pool := startTLSProxy(t, ic)
	conn := socksTLSDial(t, srv.Addr(), app.Listener.Addr().String(), "stage.local", pool)

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		io.WriteString(conn, "GET /orig HTTP/1.1\r\nHost: stage.local\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		done <- result{body: string(b)}
	}()

	f := heldFlow(t, store, capture.StateHeldRequest)
	if !f.Secure {
		t.Error("held https flow not marked secure")
	}
	edited := strings.Replace(intercept.EditableText(f.Request.Raw), "/orig", "/edited", 1)
	ic.Forward(f.ID, &edited)

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.body != "path=/edited" {
		t.Fatalf("server saw %q, edit not applied over TLS", r.body)
	}
}
