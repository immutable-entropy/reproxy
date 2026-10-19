package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
)

func startProxy(t *testing.T) (*Server, *capture.Store) {
	t.Helper()
	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, store
}

// socksDial opens a tunnel to target through the proxy using a domain-name
// address, the way browsers with "proxy DNS" do.
func socksDial(t *testing.T, proxyAddr, target string) (net.Conn, byte) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil || sel != [2]byte{5, 0} {
		t.Fatalf("method selection = %v, %v", sel, err)
	}

	host, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return c, rep[1]
}

// waitFor polls until cond holds; flows are recorded asynchronously.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func completed(store *capture.Store, n int) func() bool {
	return func() bool {
		flows := store.List()
		if len(flows) < n {
			return false
		}
		for _, f := range flows {
			if f.State == capture.StatePending {
				return false
			}
		}
		return true
	}
}

func TestRecordsKeepAliveExchanges(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/chunked":
			w.Write([]byte("part1-"))
			w.(http.Flusher).Flush()
			w.Write([]byte("part2"))
		default:
			fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.Path, body)
		}
	}))
	defer app.Close()

	srv, store := startProxy(t)
	target := strings.Replace(app.Listener.Addr().String(), "127.0.0.1", "localhost", 1)
	conn, rep := socksDial(t, srv.Addr(), target)
	if rep != repSucceeded {
		t.Fatalf("reply = %d", rep)
	}

	br := bufio.NewReader(conn)
	send := func(raw string) *http.Response {
		t.Helper()
		if _, err := io.WriteString(conn, raw); err != nil {
			t.Fatal(err)
		}
		method := raw[:strings.IndexByte(raw, ' ')]
		resp, err := http.ReadResponse(br, &http.Request{Method: method})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	getReq := "GET /hello?x=1 HTTP/1.1\r\nHost: app.local\r\n\r\n"
	postReq := "POST /submit HTTP/1.1\r\nHost: app.local\r\nContent-Length: 7\r\n\r\npayload"
	send(getReq)
	send(postReq)
	send("GET /chunked HTTP/1.1\r\nHost: app.local\r\n\r\n")
	send("HEAD /hello HTTP/1.1\r\nHost: app.local\r\n\r\n")

	waitFor(t, "4 completed flows", completed(store, 4))
	flows := store.List()

	if got := string(flows[0].Request.Raw); got != getReq {
		t.Errorf("GET raw request = %q", got)
	}
	if got := string(flows[1].Request.Raw); got != postReq {
		t.Errorf("POST raw request = %q", got)
	}
	if f := flows[0]; f.Method != "GET" || f.Host != "app.local" || f.Path != "/hello?x=1" || f.Target != target {
		t.Errorf("GET metadata = %+v", f)
	}
	if f := flows[1]; f.StatusCode != 200 || !strings.HasSuffix(string(f.Response.Raw), "POST /submit body=payload") {
		t.Errorf("POST response = %d %q", f.StatusCode, f.Response.Raw)
	}
	if raw := string(flows[2].Response.Raw); !strings.Contains(raw, "Transfer-Encoding: chunked") || !strings.HasSuffix(raw, "0\r\n\r\n") {
		t.Errorf("chunked response not captured verbatim: %q", raw)
	}
	if f := flows[3]; f.Method != "HEAD" || f.State != capture.StateComplete || !strings.HasSuffix(string(f.Response.Raw), "\r\n\r\n") {
		t.Errorf("HEAD flow = %+v", f)
	}
}

func TestPassesThroughNonHTTPWithoutRecording(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	srv, store := startProxy(t)
	conn, rep := socksDial(t, srv.Addr(), echo.Addr().String())
	if rep != repSucceeded {
		t.Fatalf("reply = %d", rep)
	}

	msg := []byte{0x16, 0x03, 0x01, 'h', 'i'} // looks like a TLS record
	conn.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != string(msg) {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if n := store.Len(); n != 0 {
		t.Fatalf("recorded %d flows for non-HTTP traffic", n)
	}
}

func TestReportsConnectionRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()

	srv, _ := startProxy(t)
	if _, rep := socksDial(t, srv.Addr(), addr); rep != repConnectionRefused {
		t.Fatalf("reply = %d, want %d", rep, repConnectionRefused)
	}
}

func TestFailsFlowWhenUpstreamClosesEarly(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(c).ReadString('\n')
		c.Close()
	}()

	srv, store := startProxy(t)
	conn, _ := socksDial(t, srv.Addr(), l.Addr().String())
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")

	waitFor(t, "failed flow", completed(store, 1))
	if f := store.List()[0]; f.State != capture.StateFailed || f.Error == "" {
		t.Fatalf("flow = %+v", f)
	}
}

func TestListenMovesPortAndKeepsOldOnFailure(t *testing.T) {
	srv, _ := startProxy(t)
	first := srv.Addr()

	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if err := srv.Listen(busy.Addr().String()); err == nil {
		t.Fatal("expected error binding a busy port")
	}
	if srv.Addr() != first {
		t.Fatalf("addr changed to %s after failed listen", srv.Addr())
	}

	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if srv.Addr() == first {
		t.Fatal("addr did not change")
	}
	if c, err := net.DialTimeout("tcp", first, time.Second); err == nil {
		c.Close()
		t.Fatal("old listener still accepting")
	}
}

// A real site behind a CDN answers with a brotli-compressed, chunked page.
// What the proxy records is those bytes, and what the views make of them is
// the page itself: this is the whole path from the wire to the editor.
func TestRecordedPageIsReadable(t *testing.T) {
	const page = `<!doctype html>` + "\n" + `<html><body><p>hello</p></body></html>`
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "br")
		bw := brotli.NewWriter(w)
		bw.Write([]byte(page))
		bw.Flush()
		w.(http.Flusher).Flush() // no Content-Length: Go then sends it chunked
		bw.Close()
	}))
	defer app.Close()

	srv, store := startProxy(t)
	conn, rep := socksDial(t, srv.Addr(), app.Listener.Addr().String())
	if rep != repSucceeded {
		t.Fatalf("reply = %d", rep)
	}
	if _, err := io.WriteString(conn, "GET /page HTTP/1.1\r\nHost: app.local\r\nAccept-Encoding: br\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	waitFor(t, "the flow to complete", completed(store, 1))
	raw := store.List()[0].Response.Raw
	if !bytes.Contains(raw, []byte("Content-Encoding: br")) {
		t.Fatalf("response was not recorded as it arrived: %q", raw)
	}

	// The editor's view shows the page...
	view, note := intercept.EditableView(raw)
	if !strings.Contains(view, page) {
		t.Errorf("view does not hold the page:\n%s", view)
	}
	if note != "dechunked · decoded from brotli" {
		t.Errorf("note = %q", note)
	}
	// ...an agent reading the flow gets it too...
	if text := capture.DisplayText(store.List()[0].Response); !strings.Contains(text, page) {
		t.Errorf("DisplayText does not hold the page:\n%s", text)
	}
	// ...and forwarding it unedited sends back exactly what arrived.
	if got := intercept.ApplyEdit(raw, view, true); !bytes.Equal(got, raw) {
		t.Errorf("unchanged view altered the response:\n%q", got)
	}
}
