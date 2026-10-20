package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/upstream"
)

// echoApp answers every request with its method, Host and path, so a test
// can see what reached the server.
func echoApp(t *testing.T, name string) *httptest.Server {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name+" "+r.Method+" "+r.Host+" "+r.URL.RequestURI())
	}))
	t.Cleanup(app.Close)
	return app
}

// exchangeOn writes raw on conn and reads the response.
func exchangeOn(t *testing.T, conn net.Conn, br *bufio.Reader, raw string) (*http.Response, string) {
	t.Helper()
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: raw[:strings.IndexByte(raw, ' ')]})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// Every protocol a client can speak to the proxy reaches the target through
// the same relay, so each one's request is recorded. The clients are the
// upstream package's, an implementation of each protocol independent of
// the server's.
func TestEveryProtocolIsRecorded(t *testing.T) {
	app := echoApp(t, "app")
	srv, store, _ := startTLSProxy(t, nil) // HTTPS needs the CA

	for i, kind := range []upstream.Kind{upstream.SOCKS4, upstream.SOCKS4a, upstream.SOCKS5, upstream.HTTP, upstream.HTTPS} {
		t.Run(string(kind), func(t *testing.T) {
			// SOCKS4a passes the name; SOCKS4 has the dialer resolve it.
			target := strings.Replace(app.Listener.Addr().String(), "127.0.0.1", "localhost", 1)
			d := upstream.NewDialer([]upstream.Hop{{Kind: kind, Addr: srv.Addr()}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			path := "/" + string(kind)
			_, body := exchangeOn(t, conn, bufio.NewReader(conn), "GET "+path+" HTTP/1.1\r\nHost: app.local\r\n\r\n")
			if body != "app GET app.local "+path {
				t.Fatalf("body = %q", body)
			}
			waitFor(t, "the flow", completed(store, i+1))
			if f := store.List()[i]; f.Path != path || f.StatusCode != 200 || f.Host != "app.local" {
				t.Fatalf("flow = %s %s %d", f.Host, f.Path, f.StatusCode)
			}
		})
	}
}

// socks4Dial sends a SOCKS4 request by hand: to ip, or with host as a
// SOCKS4a name when host isn't empty.
func socks4Dial(t *testing.T, proxyAddr string, cmd byte, ip net.IP, port int, host string) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	req := []byte{4, cmd}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	req = append(req, ip.To4()...)
	req = append(req, "someone\x00"...)
	if host != "" {
		req = append(req, host+"\x00"...)
	}
	c.Write(req)
	var rep [8]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return c, 0
	}
	if rep[0] != 0 {
		t.Fatalf("reply version = %d", rep[0])
	}
	return c, rep[1]
}

func TestSocks4RejectsWhatItCannotServe(t *testing.T) {
	srv, _ := startProxy(t)
	app := echoApp(t, "app")
	_, portStr, _ := net.SplitHostPort(app.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	if _, rep := socks4Dial(t, srv.Addr(), 2, net.IPv4(127, 0, 0, 1), port, ""); rep != socks4Rejected {
		t.Errorf("BIND reply = %#x", rep)
	}
	if _, rep := socks4Dial(t, srv.Addr(), 1, net.IPv4(0, 0, 0, 0), port, ""); rep != socks4Rejected {
		t.Errorf("0.0.0.0 reply = %#x", rep)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, cp, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()
	cport, _ := strconv.Atoi(cp)
	if _, rep := socks4Dial(t, srv.Addr(), 1, net.IPv4(127, 0, 0, 1), cport, ""); rep != socks4Rejected {
		t.Errorf("refused reply = %#x", rep)
	}
	conn, rep := socks4Dial(t, srv.Addr(), 1, net.IPv4(0, 0, 0, 1), port, "localhost")
	if rep != socks4Granted {
		t.Fatalf("SOCKS4a reply = %#x", rep)
	}
	if _, body := exchangeOn(t, conn, bufio.NewReader(conn), "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"); body != "app GET x /a" {
		t.Fatalf("body = %q", body)
	}
}

// httpProxyConn opens a plain connection to the proxy for HTTP proxy
// requests.
func httpProxyConn(t *testing.T, proxyAddr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

// Plain requests to an HTTP proxy name their origin in the request line,
// and one connection carries requests for several. Each reaches its own
// server in origin form, with the proxy's headers taken out, and is
// recorded as it was sent.
func TestHTTPProxyForwardsAcrossOrigins(t *testing.T) {
	a, b := echoApp(t, "a"), echoApp(t, "b")
	srv, store := startProxy(t)
	conn, br := httpProxyConn(t, srv.Addr())

	aURL, bURL := a.URL, strings.Replace(b.URL, "127.0.0.1", "localhost", 1)
	bHost := strings.TrimPrefix(bURL, "http://")
	send := func(raw, want string) {
		t.Helper()
		resp, body := exchangeOn(t, conn, br, raw)
		if resp.StatusCode != 200 || body != want {
			t.Fatalf("%q: %d %q, want %q", raw, resp.StatusCode, body, want)
		}
	}
	send("GET "+aURL+"/one?x=1 HTTP/1.1\r\nHost: a.example\r\nProxy-Connection: keep-alive\r\n\r\n",
		"a GET "+strings.TrimPrefix(aURL, "http://")+" /one?x=1")
	send("GET "+aURL+"/two HTTP/1.1\r\nHost: "+strings.TrimPrefix(aURL, "http://")+"\r\n\r\n",
		"a GET "+strings.TrimPrefix(aURL, "http://")+" /two")
	// No Host header, and a Proxy-Authorization: the proxy supplies one and
	// keeps the other to itself.
	send("POST "+bURL+" HTTP/1.1\r\nProxy-Authorization: Basic eDp5\r\nContent-Length: 2\r\n\r\nhi",
		"b POST "+bHost+" /")
	send("GET "+aURL+"/three HTTP/1.1\r\nHost: x\r\n\r\n", "a GET "+strings.TrimPrefix(aURL, "http://")+" /three")

	waitFor(t, "four flows", completed(store, 4))
	flows := store.List()
	wantRaw := "GET /one?x=1 HTTP/1.1\r\nHost: " + strings.TrimPrefix(aURL, "http://") + "\r\n\r\n"
	if got := string(flows[0].Request.Raw); got != wantRaw {
		t.Errorf("first request recorded as %q, want %q", got, wantRaw)
	}
	wantRaw = "POST / HTTP/1.1\r\nContent-Length: 2\r\nHost: " + bHost + "\r\n\r\nhi"
	if got := string(flows[2].Request.Raw); got != wantRaw {
		t.Errorf("third request recorded as %q, want %q", got, wantRaw)
	}
	if f := flows[2]; f.Host != bHost || f.Path != "/" || f.Method != "POST" || f.Target != bHost || f.Secure {
		t.Errorf("third flow = %+v", f)
	}
}

// Requests sent back to back are answered in order even when the second is
// for another origin: the first's response comes back before the proxy
// moves the connection on.
func TestHTTPProxyPipelinedOriginSwitch(t *testing.T) {
	a, b := echoApp(t, "a"), echoApp(t, "b")
	srv, store := startProxy(t)
	conn, br := httpProxyConn(t, srv.Addr())
	io.WriteString(conn, "GET "+a.URL+"/1 HTTP/1.1\r\n\r\nGET "+b.URL+"/2 HTTP/1.1\r\n\r\nGET "+a.URL+"/3 HTTP/1.1\r\n\r\n")
	for _, want := range []string{"a", "b", "a"} {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.HasPrefix(string(body), want+" ") {
			t.Fatalf("got %q, want a response from %s", body, want)
		}
	}
	waitFor(t, "three flows", completed(store, 3))
}

// An https:// URL in a plain request is sent to its server over TLS.
func TestHTTPProxyForwardsHTTPSURL(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secure "+r.URL.Path)
	}))
	defer app.Close()
	srv, store := startProxy(t)
	conn, br := httpProxyConn(t, srv.Addr())
	if _, body := exchangeOn(t, conn, br, "GET "+app.URL+"/s HTTP/1.1\r\n\r\n"); body != "secure /s" {
		t.Fatalf("body = %q", body)
	}
	waitFor(t, "the flow", completed(store, 1))
	if f := store.List()[0]; !f.Secure || f.Path != "/s" {
		t.Fatalf("flow = %+v", f)
	}
}

// Go's own client, set to use the proxy, works for both schemes: plain
// requests for http, CONNECT (decrypted with the CA) for https.
func TestHTTPProxyWithGoClient(t *testing.T) {
	plain := echoApp(t, "plain")
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secure "+r.URL.Path)
	}))
	defer secure.Close()
	srv, store, pool := startTLSProxy(t, nil)

	proxyURL, _ := url.Parse("http://user:s3cret@" + srv.Addr())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	defer client.CloseIdleConnections()
	for _, u := range []string{plain.URL + "/p", secure.URL + "/s", plain.URL + "/p2"} {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", u, resp.StatusCode)
		}
	}
	waitFor(t, "three flows", completed(store, 3))
	flows := store.List()
	if !flows[1].Secure || flows[1].Path != "/s" || flows[0].Secure || flows[2].Path != "/p2" {
		t.Fatalf("flows = %s %v, %s %v, %s", flows[0].Path, flows[0].Secure, flows[1].Path, flows[1].Secure, flows[2].Path)
	}
	for _, f := range flows {
		if strings.Contains(string(f.Request.Raw), "Proxy-Authorization") {
			t.Errorf("proxy credentials reached the server: %q", f.Request.Raw)
		}
	}
}

// An HTTP proxy's Proxy-Authorization labels a plugin's connection as
// SOCKS5 credentials do.
func TestHTTPProxyCredentialsLabelOrigin(t *testing.T) {
	app := echoApp(t, "app")
	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)),
		WithOrigin(func(user, pass string) (capture.Source, string, bool) {
			return capture.SourcePlugin, user, pass == "s3cret"
		}))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	auth := base64.StdEncoding.EncodeToString([]byte("mapper:s3cret"))
	conn, br := httpProxyConn(t, srv.Addr())
	exchangeOn(t, conn, br, "GET "+app.URL+"/ HTTP/1.1\r\nProxy-Authorization: Basic "+auth+"\r\n\r\n")
	waitFor(t, "the flow", completed(store, 1))
	if f := store.List()[0]; f.Source != capture.SourcePlugin || f.Origin != "mapper" {
		t.Fatalf("flow = %q/%q", f.Source, f.Origin)
	}
}

func TestHTTPProxyErrors(t *testing.T) {
	srv, _ := startProxy(t)
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()

	for _, tc := range []struct {
		raw  string
		code int
	}{
		{"GET / HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest},                 // the proxy opened as a page
		{"GET ftp://x/ HTTP/1.1\r\n\r\n", http.StatusBadRequest},                     // not http(s)
		{"CONNECT nowhere HTTP/1.1\r\nHost: nowhere\r\n\r\n", http.StatusBadRequest}, // no port
		{"CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n\r\n", http.StatusBadGateway},
		{"GET http://" + addr + "/ HTTP/1.1\r\n\r\n", http.StatusBadGateway},
	} {
		conn, br := httpProxyConn(t, srv.Addr())
		resp, _ := exchangeOn(t, conn, br, tc.raw)
		if resp.StatusCode != tc.code {
			t.Errorf("%q: status %d, want %d", tc.raw, resp.StatusCode, tc.code)
		}
	}
}

func TestRewriteHead(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"GET http://h:81/p?q HTTP/1.1\r\nHost: h:81\r\nProxy-Connection: keep-alive\r\nAccept: */*\r\n\r\n",
			"GET /p?q HTTP/1.1\r\nHost: h:81\r\nAccept: */*\r\n\r\n"},
		{"GET http://user@h HTTP/1.1\r\nhost: other\r\n\r\n", "GET / HTTP/1.1\r\nHost: h\r\n\r\n"},
		{"GET http://h?q=1#frag HTTP/1.0\r\n\r\n", "GET /?q=1 HTTP/1.0\r\nHost: h\r\n\r\n"},
		{"GET http://[::1]:8080/x HTTP/1.1\r\n\r\n", "GET /x HTTP/1.1\r\nHost: [::1]:8080\r\n\r\n"},
	} {
		target, _ := requestTarget([]byte(tc.in))
		a, err := parseAbsolute(target)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got := string(rewriteHead([]byte(tc.in), a)); got != tc.want {
			t.Errorf("rewriteHead(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if a, _ := parseAbsolute("https://H.example/x"); a.addr != "H.example:443" || a.key() != "https://h.example:443" {
		t.Errorf("https default port: %+v", a)
	}
	// A request in origin form only loses the proxy's headers.
	f := &forwardState{key: "http://h:80"}
	if got, same := f.rewrite([]byte("GET /x HTTP/1.1\r\nHost: h\r\nProxy-Authorization: x\r\n\r\n")); !same || string(got) != "GET /x HTTP/1.1\r\nHost: h\r\n\r\n" {
		t.Errorf("origin-form rewrite = %q, %v", got, same)
	}
	if _, same := f.rewrite([]byte("GET http://other/ HTTP/1.1\r\n\r\n")); same {
		t.Error("a request for another origin was taken as this one's")
	}
}
