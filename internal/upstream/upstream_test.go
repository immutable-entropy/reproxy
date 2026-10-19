package upstream

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- test doubles ----------------------------------------------------------

// fakeProxy is a proxy of one kind that records every target it is asked
// for, then connects there and pipes bytes both ways. It is enough of each
// protocol to exercise the client in this package.
type fakeProxy struct {
	t    *testing.T
	kind Kind
	ln   net.Listener

	user, pass string
	// refuse, when set, makes the proxy reject the connect request.
	refuse bool
	// eager is written immediately after a successful HTTP CONNECT reply,
	// in the same write, so the client has to read past its own reply.
	eager string

	mu      sync.Mutex
	targets []string
	wg      sync.WaitGroup
}

func newFakeProxy(t *testing.T, kind Kind) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &fakeProxy{t: t, kind: kind, ln: ln}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func (p *fakeProxy) addr() string { return p.ln.Addr().String() }

func (p *fakeProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *fakeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer c.Close()
			p.handle(c)
		}()
	}
}

func (p *fakeProxy) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var target string
	var err error
	switch p.kind {
	case HTTP:
		target, err = p.handleHTTP(c)
	case SOCKS4, SOCKS4a:
		target, err = p.handleSOCKS4(c)
	case SOCKS5:
		target, err = p.handleSOCKS5(c)
	default:
		err = fmt.Errorf("fake proxy cannot speak %s", p.kind)
	}
	if err != nil || target == "" {
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, target)
	p.mu.Unlock()

	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	_ = c.SetDeadline(time.Time{})
	_ = up.SetDeadline(time.Time{})
	// Each direction closes the other end when it finishes, so closing the
	// client connection tears the whole chain down and the test's cleanup
	// doesn't wait on a half-open pipe.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(up, c)
		_ = up.Close()
	}()
	_, _ = io.Copy(c, up)
	_ = c.Close()
	<-done
}

func (p *fakeProxy) handleHTTP(c net.Conn) (string, error) {
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) != 3 || fields[0] != "CONNECT" {
		return "", fmt.Errorf("not a CONNECT: %q", line)
	}
	var auth string
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		if h == "\r\n" || h == "\n" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Proxy-Authorization") {
			auth = strings.TrimSpace(v)
		}
	}
	if p.user != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass))
		if auth != want {
			_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
			return "", errors.New("bad credentials")
		}
	}
	if p.refuse {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return "", errors.New("refused")
	}
	// The reply and any eager bytes go out in one write, so a client that
	// buffers ahead has to hand the extra bytes back.
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\nX-Proxy: fake\r\n\r\n"+p.eager)
	return fields[1], nil
}

func (p *fakeProxy) handleSOCKS4(c net.Conn) (string, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != 4 || hdr[1] != 1 {
		return "", fmt.Errorf("not a SOCKS4 CONNECT: %v", hdr)
	}
	port := int(hdr[2])<<8 | int(hdr[3])
	userID, err := readUntilNUL(c)
	if err != nil {
		return "", err
	}
	if userID != p.user {
		_, _ = c.Write([]byte{0, socks4IdentdFailed, 0, 0, 0, 0, 0, 0})
		return "", fmt.Errorf("user ID %q, want %q", userID, p.user)
	}
	host := net.IP(hdr[4:8]).String()
	// 0.0.0.x with x non-zero means the host name follows (SOCKS4a).
	if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 {
		if host, err = readUntilNUL(c); err != nil {
			return "", err
		}
	}
	if p.refuse {
		_, _ = c.Write([]byte{0, socks4Rejected, 0, 0, 0, 0, 0, 0})
		return "", errors.New("refused")
	}
	if _, err := c.Write([]byte{0, socks4Granted, 0, 0, 0, 0, 0, 0}); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (p *fakeProxy) handleSOCKS5(c net.Conn) (string, error) {
	var greeting [2]byte
	if _, err := io.ReadFull(c, greeting[:]); err != nil {
		return "", err
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	want := byte(methodNoAuth)
	if p.user != "" {
		want = methodUserPass
	}
	if !slices.Contains(methods, want) {
		_, _ = c.Write([]byte{socks5Version, methodNoAcceptable})
		return "", fmt.Errorf("client did not offer method 0x%02x", want)
	}
	if _, err := c.Write([]byte{socks5Version, want}); err != nil {
		return "", err
	}
	if want == methodUserPass {
		if err := p.readUserPass(c); err != nil {
			return "", err
		}
	}

	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != socks5Version || hdr[1] != socks5Connect {
		return "", fmt.Errorf("not a SOCKS5 CONNECT: %v", hdr)
	}
	var host string
	switch hdr[3] {
	case atypIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case atypIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", fmt.Errorf("address type %d", hdr[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	if p.refuse {
		_, _ = c.Write([]byte{socks5Version, 0x02, 0, atypIPv4, 0, 0, 0, 0, 0, 0})
		return "", errors.New("refused")
	}
	// Reply with a domain-typed bound address, the longest form, so the
	// client has to read it off correctly before the tunnel starts.
	reply := []byte{socks5Version, 0x00, 0x00, atypDomain, byte(len("fake.proxy"))}
	reply = append(reply, "fake.proxy"...)
	reply = append(reply, 0x1F, 0x90)
	if _, err := c.Write(reply); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1]))), nil
}

func (p *fakeProxy) readUserPass(c net.Conn) error {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return err
	}
	user := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return err
	}
	var pl [1]byte
	if _, err := io.ReadFull(c, pl[:]); err != nil {
		return err
	}
	pass := make([]byte, pl[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return err
	}
	if string(user) != p.user || string(pass) != p.pass {
		_, _ = c.Write([]byte{userPassVersion, 0x01})
		return fmt.Errorf("credentials %q/%q rejected", user, pass)
	}
	_, err := c.Write([]byte{userPassVersion, 0x00})
	return err
}

func readUntilNUL(r io.Reader) (string, error) {
	var out []byte
	var b [1]byte
	for len(out) < 256 {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		if b[0] == 0 {
			return string(out), nil
		}
		out = append(out, b[0])
	}
	return "", errors.New("unterminated string")
}

// echoServer answers every line with "echo: <line>", so a test can prove
// bytes flow both ways through the whole chain.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := io.WriteString(c, "echo: "+line); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// roundTrip sends one line through conn and returns the reply.
func roundTrip(t *testing.T, conn net.Conn, line string) string {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(got)
}

func dialThrough(t *testing.T, hops []Hop, target string) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewDialer(hops).DialContext(ctx, "tcp", target)
}

// ---- tests -----------------------------------------------------------------

func TestDialDirectWithoutHops(t *testing.T) {
	target := echoServer(t)
	for _, d := range []*Dialer{nil, NewDialer(nil), NewDialer([]Hop{})} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := d.DialContext(ctx, "tcp", target)
		cancel()
		if err != nil {
			t.Fatalf("direct dial: %v", err)
		}
		if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
			t.Errorf("round trip = %q", got)
		}
		conn.Close()
	}
}

func TestDialThroughEachKind(t *testing.T) {
	target := echoServer(t)
	for _, kind := range []Kind{HTTP, SOCKS4, SOCKS4a, SOCKS5} {
		t.Run(string(kind), func(t *testing.T) {
			p := newFakeProxy(t, kind)
			conn, err := dialThrough(t, []Hop{{Kind: kind, Addr: p.addr()}}, target)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
				t.Errorf("round trip = %q", got)
			}
			if seen := p.seen(); len(seen) != 1 || seen[0] != target {
				t.Errorf("proxy saw %v, want [%s]", seen, target)
			}
		})
	}
}

func TestDialWithCredentials(t *testing.T) {
	target := echoServer(t)
	for _, kind := range []Kind{HTTP, SOCKS5} {
		t.Run(string(kind), func(t *testing.T) {
			p := newFakeProxy(t, kind)
			p.user, p.pass = "tester", "s3cret"
			hop := Hop{Kind: kind, Addr: p.addr(), Username: "tester", Password: "s3cret"}
			conn, err := dialThrough(t, []Hop{hop}, target)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
				t.Errorf("round trip = %q", got)
			}

			hop.Password = "wrong"
			if _, err := dialThrough(t, []Hop{hop}, target); err == nil {
				t.Error("dial with the wrong password succeeded")
			}
		})
	}
}

// TestSOCKS4UserID checks that the username goes out as the USERID field,
// which is SOCKS4's only form of authentication.
func TestSOCKS4UserID(t *testing.T) {
	target := echoServer(t)
	p := newFakeProxy(t, SOCKS4)
	p.user = "tester"

	if _, err := dialThrough(t, []Hop{{Kind: SOCKS4, Addr: p.addr()}}, target); err == nil {
		t.Error("dial without a user ID succeeded")
	}
	conn, err := dialThrough(t, []Hop{{Kind: SOCKS4, Addr: p.addr(), Username: "tester"}}, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
		t.Errorf("round trip = %q", got)
	}
}

// TestSOCKS4aSendsHostName checks that a name is handed to the proxy
// instead of being resolved here — the difference between SOCKS4 and 4a.
func TestSOCKS4aSendsHostName(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	p := newFakeProxy(t, SOCKS4a)

	conn, err := dialThrough(t, []Hop{{Kind: SOCKS4a, Addr: p.addr()}}, net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
		t.Errorf("round trip = %q", got)
	}
	if seen := p.seen(); len(seen) != 1 || seen[0] != net.JoinHostPort("localhost", port) {
		t.Errorf("proxy saw %v, want the host name, not an address", seen)
	}
}

// TestSOCKS5SendsHostName is the same check for SOCKS5, whose domain
// address type is why a staging host that only resolves upstream works.
func TestSOCKS5SendsHostName(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	p := newFakeProxy(t, SOCKS5)

	conn, err := dialThrough(t, []Hop{{Kind: SOCKS5, Addr: p.addr()}}, net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if seen := p.seen(); len(seen) != 1 || seen[0] != net.JoinHostPort("localhost", port) {
		t.Errorf("proxy saw %v, want the host name, not an address", seen)
	}
}

// TestHTTPConnectKeepsReadAheadBytes covers the proxy that packs the start
// of the tunnel into the same write as its CONNECT reply: those bytes must
// come back out of the connection, not be lost in the handshake's buffer.
func TestHTTPConnectKeepsReadAheadBytes(t *testing.T) {
	target := echoServer(t)
	p := newFakeProxy(t, HTTP)
	p.eager = "220 ready\n"

	conn, err := dialThrough(t, []Hop{{Kind: HTTP, Addr: p.addr()}}, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if first != "220 ready\n" {
		t.Fatalf("first line = %q, want the bytes written with the CONNECT reply", first)
	}
	if _, err := io.WriteString(conn, "hi\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(line) != "echo: hi" {
		t.Errorf("after the read-ahead, got %q", line)
	}
}

// TestChainEncapsulation is the point of the feature: each proxy is
// reached through the one before it, and only the last one is told the
// real target.
func TestChainEncapsulation(t *testing.T) {
	target := echoServer(t)
	outer := newFakeProxy(t, SOCKS5)
	middle := newFakeProxy(t, HTTP)
	inner := newFakeProxy(t, SOCKS4a)

	hops := []Hop{
		{Kind: SOCKS5, Addr: outer.addr()},
		{Kind: HTTP, Addr: middle.addr()},
		{Kind: SOCKS4a, Addr: inner.addr()},
	}
	conn, err := dialThrough(t, hops, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
		t.Errorf("round trip = %q", got)
	}

	for _, c := range []struct {
		name string
		p    *fakeProxy
		want string
	}{
		{"outer", outer, middle.addr()},
		{"middle", middle, inner.addr()},
		{"inner", inner, target},
	} {
		if seen := c.p.seen(); len(seen) != 1 || seen[0] != c.want {
			t.Errorf("%s proxy saw %v, want [%s]", c.name, seen, c.want)
		}
	}
}

// TestChainReportsFailingHop names the hop that refused, so a chain can be
// debugged from the error alone.
func TestChainReportsFailingHop(t *testing.T) {
	target := echoServer(t)
	outer := newFakeProxy(t, SOCKS5)
	inner := newFakeProxy(t, HTTP)
	inner.refuse = true

	_, err := dialThrough(t, []Hop{
		{Kind: SOCKS5, Addr: outer.addr()},
		{Kind: HTTP, Addr: inner.addr()},
	}, target)
	if err == nil {
		t.Fatal("dial through a refusing proxy succeeded")
	}
	if !strings.Contains(err.Error(), "hop 2 of 2") || !strings.Contains(err.Error(), inner.addr()) {
		t.Errorf("error %q does not identify the failing hop", err)
	}
}

func TestDialUnsupportedNetwork(t *testing.T) {
	p := newFakeProxy(t, SOCKS5)
	d := NewDialer([]Hop{{Kind: SOCKS5, Addr: p.addr()}})
	if _, err := d.DialContext(context.Background(), "udp", "example.test:53"); err == nil {
		t.Error("UDP through a proxy chain was accepted")
	}
}

func TestDialContextCancelled(t *testing.T) {
	// A listener that accepts and then says nothing: the handshake can only
	// end by the context running out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			time.Sleep(2 * time.Second)
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := NewDialer([]Hop{{Kind: SOCKS5, Addr: ln.Addr().String()}}).
		DialContext(ctx, "tcp", "example.test:443"); err == nil {
		t.Fatal("dial through a silent proxy succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("dial took %s; the context deadline did not interrupt the handshake", elapsed)
	}
}

func TestChainActiveAndHops(t *testing.T) {
	c := Chain{
		Enabled:   true,
		VerifyTLS: true,
		Proxies: []Proxy{
			{ID: "a", Enabled: true, Kind: SOCKS5, Host: "one.test", Port: 1080, Username: "u"},
			{ID: "b", Enabled: false, Kind: HTTP, Host: "two.test", Port: 8080},
			{ID: "c", Enabled: true, Kind: HTTPS, Host: "three.test", Port: 8443},
		},
	}
	active := c.Active()
	if len(active) != 2 || active[0].ID != "a" || active[1].ID != "c" {
		t.Fatalf("Active() = %v, want the enabled proxies in order", active)
	}

	hops := c.Hops(func(id string) string { return "pw-" + id })
	if len(hops) != 2 {
		t.Fatalf("Hops() returned %d hops", len(hops))
	}
	if hops[0].Addr != "one.test:1080" || hops[0].Username != "u" || hops[0].Password != "pw-a" {
		t.Errorf("first hop = %+v", hops[0])
	}
	if !hops[1].VerifyTLS {
		t.Error("VerifyTLS did not reach the hops")
	}

	off := c
	off.Enabled = false
	if len(off.Active()) != 0 || off.Hops(nil) != nil {
		t.Error("a disabled chain still produced hops")
	}
}

func TestChainValidate(t *testing.T) {
	ok := Chain{Proxies: []Proxy{{Kind: SOCKS5, Host: "proxy.test", Port: 1080}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid chain rejected: %v", err)
	}

	for name, c := range map[string]Chain{
		"unknown kind": {Proxies: []Proxy{{Kind: "socks6", Host: "p.test", Port: 1080}}},
		"no host":      {Proxies: []Proxy{{Kind: HTTP, Host: "  ", Port: 8080}}},
		"host is URL":  {Proxies: []Proxy{{Kind: HTTP, Host: "http://p.test", Port: 8080}}},
		"port zero":    {Proxies: []Proxy{{Kind: HTTP, Host: "p.test", Port: 0}}},
		"port high":    {Proxies: []Proxy{{Kind: HTTP, Host: "p.test", Port: 70000}}},
		"NUL in user":  {Proxies: []Proxy{{Kind: SOCKS4, Host: "p.test", Port: 1080, Username: "a\x00b"}}},
		"long user":    {Proxies: []Proxy{{Kind: SOCKS5, Host: "p.test", Port: 1080, Username: strings.Repeat("u", 256)}}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A disabled proxy is checked too, so a typo surfaces where it was made.
	bad := Chain{Proxies: []Proxy{{Enabled: false, Kind: HTTP, Host: "p.test", Port: -1}}}
	if err := bad.Validate(); err == nil {
		t.Error("a disabled proxy with a bad port was accepted")
	}
}

func TestKindValid(t *testing.T) {
	for _, k := range Kinds {
		if !k.Valid() {
			t.Errorf("%s reported invalid", k)
		}
	}
	if Kind("socks").Valid() || Kind("").Valid() {
		t.Error("an unknown kind reported valid")
	}
}

func TestProxyAddr(t *testing.T) {
	if got := (Proxy{Host: "p.test", Port: 8080}).Addr(); got != "p.test:8080" {
		t.Errorf("Addr() = %q", got)
	}
	if got := (Proxy{Host: "::1", Port: 1080}).Addr(); got != "[::1]:1080" {
		t.Errorf("Addr() = %q, want the IPv6 form bracketed", got)
	}
}

func TestSOCKS4RejectsIPv6Target(t *testing.T) {
	p := newFakeProxy(t, SOCKS4)
	_, err := dialThrough(t, []Hop{{Kind: SOCKS4, Addr: p.addr()}}, "[::1]:443")
	if err == nil || !strings.Contains(err.Error(), "IPv6") {
		t.Errorf("error = %v, want one explaining SOCKS4 has no IPv6", err)
	}
}

// ---- HTTPS proxies ---------------------------------------------------------

// selfSignedCert returns a certificate for 127.0.0.1, standing in for the
// kind of certificate a local proxy presents for itself.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake proxy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newFakeTLSProxy is an HTTP CONNECT proxy reached over TLS, which is what
// the HTTPS kind means: the tunnel inside is unchanged.
func newFakeTLSProxy(t *testing.T) *fakeProxy {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	p := &fakeProxy{t: t, kind: HTTP, ln: ln}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

func TestDialThroughHTTPSProxy(t *testing.T) {
	target := echoServer(t)
	p := newFakeTLSProxy(t)

	// The proxy's certificate is self-signed, so verification has to be off.
	conn, err := dialThrough(t, []Hop{{Kind: HTTPS, Addr: p.addr()}}, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if got := roundTrip(t, conn, "hi"); got != "echo: hi" {
		t.Errorf("round trip = %q", got)
	}
	if seen := p.seen(); len(seen) != 1 || seen[0] != target {
		t.Errorf("proxy saw %v, want [%s]", seen, target)
	}

	// With verification on, the same proxy is refused rather than trusted.
	if _, err := dialThrough(t, []Hop{{Kind: HTTPS, Addr: p.addr(), VerifyTLS: true}}, target); err == nil {
		t.Error("a self-signed proxy certificate was accepted with VerifyTLS on")
	}
}

// TestPlainHTTPToTLSProxyFails guards the distinction between the two HTTP
// kinds: sending a bare CONNECT to a TLS listener must not look like it
// worked.
func TestPlainHTTPToTLSProxyFails(t *testing.T) {
	target := echoServer(t)
	p := newFakeTLSProxy(t)
	if _, err := dialThrough(t, []Hop{{Kind: HTTP, Addr: p.addr()}}, target); err == nil {
		t.Error("plain HTTP to a TLS proxy succeeded")
	}
}

func TestResolvesRemotely(t *testing.T) {
	for _, tc := range []struct {
		kinds []Kind
		want  bool
	}{
		{nil, false},
		{[]Kind{SOCKS5}, true},
		{[]Kind{HTTP}, true},
		{[]Kind{SOCKS4a}, true},
		{[]Kind{SOCKS5, SOCKS4}, false}, // the last hop can't carry names
		{[]Kind{SOCKS4, HTTPS}, true},
	} {
		var hops []Hop
		for _, k := range tc.kinds {
			hops = append(hops, Hop{Kind: k, Addr: "127.0.0.1:1"})
		}
		if got := NewDialer(hops).ResolvesRemotely(); got != tc.want {
			t.Errorf("%v: ResolvesRemotely = %v, want %v", tc.kinds, got, tc.want)
		}
	}
	var nilDialer *Dialer
	if nilDialer.ResolvesRemotely() {
		t.Error("a nil dialer resolves remotely")
	}
}
