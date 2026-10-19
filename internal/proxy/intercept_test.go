package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
)

func startInterceptingProxy(t *testing.T, hold intercept.UserHoldFunc) (*Server, *capture.Store, *intercept.Interceptor) {
	t.Helper()
	ic, err := intercept.New(intercept.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if hold != nil {
		ic.SetUserHold(hold)
	}
	ic.SetEnabled(true)
	store := capture.NewStore()
	srv := New(store, WithInterceptor(ic), WithLogger(slog.New(slog.DiscardHandler)))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, store, ic
}

func heldFlow(t *testing.T, store *capture.Store, state capture.FlowState) capture.Flow {
	t.Helper()
	var found capture.Flow
	waitFor(t, "held flow", func() bool {
		for _, f := range store.List() {
			if f.State == state {
				found = f
				return true
			}
		}
		return false
	})
	return found
}

func echoServer(t *testing.T) *httptest.Server {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s body=%s", r.Method, r.URL.Path, body)
	}))
	t.Cleanup(app.Close)
	return app
}

func roundTrip(t *testing.T, conn net.Conn, raw string) (*http.Response, string) {
	t.Helper()
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestHeldRequestIsEditedAndForwarded(t *testing.T) {
	app := echoServer(t)
	srv, store, ic := startInterceptingProxy(t, nil)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())

	type result struct {
		resp *http.Response
		body string
	}
	done := make(chan result)
	go func() {
		r, b := roundTrip(t, conn, "POST /orig HTTP/1.1\r\nHost: app\r\nContent-Length: 3\r\n\r\nabc")
		done <- result{r, b}
	}()

	f := heldFlow(t, store, capture.StateHeldRequest)
	text := intercept.EditableText(f.Request.Raw)
	text = strings.Replace(text, "/orig", "/edited", 1)
	text = strings.Replace(text, "abc", "<script>alert(1)</script>", 1)
	if err := ic.Forward(f.ID, &text); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.body != "POST /edited body=<script>alert(1)</script>" {
		t.Fatalf("server saw %q", r.body)
	}
	waitFor(t, "completed flow", completed(store, 1))
	got, _ := store.Get(f.ID)
	if got.State != capture.StateComplete || got.OriginalRequest == nil || got.Path != "/edited" ||
		!strings.Contains(string(got.Request.Raw), "Content-Length: 25") {
		t.Fatalf("flow after edit = %+v", got)
	}
}

func TestDroppedRequestClosesConnection(t *testing.T) {
	app := echoServer(t)
	srv, store, ic := startInterceptingProxy(t, nil)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())
	io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: app\r\n\r\n")

	f := heldFlow(t, store, capture.StateHeldRequest)
	ic.Drop(f.ID)

	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected connection to close after drop")
	}
	waitFor(t, "dropped state", func() bool {
		got, _ := store.Get(f.ID)
		return got.State == capture.StateDropped
	})
}

func TestHeldResponseIsEdited(t *testing.T) {
	app := echoServer(t)
	holdResponses := func(dir intercept.Direction, _ intercept.Subject) bool { return dir == intercept.Response }
	srv, store, ic := startInterceptingProxy(t, holdResponses)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())

	type result struct {
		resp *http.Response
		body string
	}
	done := make(chan result)
	go func() {
		r, b := roundTrip(t, conn, "GET /page HTTP/1.1\r\nHost: app\r\n\r\n")
		done <- result{r, b}
	}()

	f := heldFlow(t, store, capture.StateHeldResponse)
	text := strings.Replace(intercept.EditableText(f.Response.Raw), "200 OK", "500 Internal Server Error", 1)
	text = strings.Replace(text, "GET /page body=", `{"unexpected": null}`, 1)
	ic.Forward(f.ID, &text)

	r := <-done
	if r.resp.StatusCode != 500 || r.body != `{"unexpected": null}` {
		t.Fatalf("browser got %d %q", r.resp.StatusCode, r.body)
	}
	waitFor(t, "complete", completed(store, 1))
	if got, _ := store.Get(f.ID); got.StatusCode != 500 || got.OriginalResponse == nil {
		t.Fatalf("flow = %+v", got)
	}
}

func TestDisablingReleasesHeldRequest(t *testing.T) {
	app := echoServer(t)
	srv, store, ic := startInterceptingProxy(t, nil)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())

	done := make(chan string)
	go func() {
		_, b := roundTrip(t, conn, "GET /held HTTP/1.1\r\nHost: app\r\n\r\n")
		done <- b
	}()
	heldFlow(t, store, capture.StateHeldRequest)
	ic.SetEnabled(false)
	if b := <-done; b != "GET /held body=" {
		t.Fatalf("body = %q", b)
	}
}

func TestStreamsResponsesWithoutWaitingForTheEnd(t *testing.T) {
	release := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	}))
	defer app.Close()
	defer close(release)

	srv, _ := startProxy(t)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())
	io.WriteString(conn, "GET /events HTTP/1.1\r\nHost: app\r\n\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event = %q, %v (response not streamed?)", line, err)
	}
}

func TestExpectContinue(t *testing.T) {
	app := echoServer(t)
	srv, store := startProxy(t)
	conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())
	br := bufio.NewReader(conn)

	io.WriteString(conn, "POST /up HTTP/1.1\r\nHost: app\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n")
	interim, err := http.ReadResponse(br, nil)
	if err != nil || interim.StatusCode != 100 {
		t.Fatalf("interim = %v, %v", interim, err)
	}
	io.WriteString(conn, "data")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "POST /up body=data" {
		t.Fatalf("body = %q", body)
	}
	waitFor(t, "complete", completed(store, 1))
	if raw := string(store.List()[0].Response.Raw); !strings.HasPrefix(raw, "HTTP/1.1 200") {
		t.Fatalf("recorded response = %q", raw)
	}
}

func TestUpgradeSwitchesToRawTunnel(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		io.Copy(c, br) // echo frames
	}()

	srv, store := startProxy(t)
	conn, _ := socksDial(t, srv.Addr(), l.Addr().String())
	br := bufio.NewReader(conn)
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: app\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade = %v, %v", resp, err)
	}
	frame := "\x81\x05hello not http"
	io.WriteString(conn, frame)
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != frame {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if n := store.Len(); n != 1 {
		t.Fatalf("recorded %d flows, want just the upgrade", n)
	}
}

func TestResponseDelimitedByClose(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		http.ReadRequest(bufio.NewReader(c))
		io.WriteString(c, "HTTP/1.0 200 OK\r\n\r\nuntil close")
		c.Close()
	}()

	srv, store := startProxy(t)
	conn, _ := socksDial(t, srv.Addr(), l.Addr().String())
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app\r\n\r\n")
	all, _ := io.ReadAll(conn)
	if !strings.HasSuffix(string(all), "until close") {
		t.Fatalf("browser got %q", all)
	}
	waitFor(t, "complete", completed(store, 1))
	if f := store.List()[0]; f.State != capture.StateComplete || !strings.HasSuffix(string(f.Response.Raw), "until close") {
		t.Fatalf("flow = %+v", f)
	}
}

// A store event can announce a held flow before the interceptor has
// registered it, so automation reacts to the interceptor's own events. A
// forward issued straight from a Held event must always succeed.
func TestForwardFromHeldEventSucceeds(t *testing.T) {
	app := echoServer(t)
	srv, _, ic := startInterceptingProxy(t, nil)
	errs := make(chan error, 8)
	unsub := ic.Subscribe(func(e intercept.HeldEvent) {
		if !e.Released {
			go func() { errs <- ic.Forward(e.FlowID, nil) }()
		}
	})
	defer unsub()

	for i := 0; i < 5; i++ {
		conn, _ := socksDial(t, srv.Addr(), app.Listener.Addr().String())
		_, body := roundTrip(t, conn, "GET /n HTTP/1.1\r\nHost: app\r\n\r\n")
		if body != "GET /n body=" {
			t.Fatalf("body %q", body)
		}
		if err := <-errs; err != nil {
			t.Fatalf("forward from held event: %v", err)
		}
		conn.Close()
	}
}
