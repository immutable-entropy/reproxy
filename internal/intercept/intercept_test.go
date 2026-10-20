package intercept

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyEdit(t *testing.T) {
	orig := []byte("POST /login HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\nhello")

	if got := ApplyEdit(orig, EditableText(orig), true); string(got) != string(orig) {
		t.Fatalf("unchanged edit altered message: %q", got)
	}

	edited := strings.Replace(EditableText(orig), "hello", "hello world", 1)
	if got := string(ApplyEdit(orig, edited, true)); got != "POST /login HTTP/1.1\r\nHost: x\r\nContent-Length: 11\r\n\r\nhello world" {
		t.Errorf("content-length not updated: %q", got)
	}
	if got := string(ApplyEdit(orig, edited, false)); !strings.Contains(got, "Content-Length: 5\r\n") {
		t.Errorf("content-length updated although disabled: %q", got)
	}

	// Header edit keeps a binary body intact.
	bin := []byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n\x00\x01\x02")
	headerEdit := strings.Replace(EditableText(bin), "200 OK", "500 Oops", 1)
	if got := string(ApplyEdit(bin, headerEdit, true)); got != "HTTP/1.1 500 Oops\r\nContent-Length: 3\r\n\r\n\x00\x01\x02" {
		t.Errorf("binary body not preserved: %q", got)
	}

	// CRLF bodies (multipart) keep CRLF after editing.
	mp := []byte("POST / HTTP/1.1\r\nContent-Length: 10\r\n\r\n--b\r\nx\r\n--")
	mpEdit := strings.Replace(EditableText(mp), "x", "yy", 1)
	if got := string(ApplyEdit(mp, mpEdit, true)); !strings.HasSuffix(got, "\r\n\r\n--b\r\nyy\r\n--") || !strings.Contains(got, "Content-Length: 11") {
		t.Errorf("multipart edit = %q", got)
	}
}

func TestHoldForwardDropAndDisable(t *testing.T) {
	i, _ := New(DefaultSettings())
	i.SetEnabled(true)
	if i.Holds(Request, Subject{})&HoldUser == 0 || i.Holds(Response, Subject{})&HoldUser != 0 {
		t.Fatal("default should hold requests only")
	}

	raw := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	result := make(chan Decision, 3)
	hold := func(id uint64) {
		d, err := i.Hold(context.Background(), id, Request, raw)
		if err != nil {
			t.Error(err)
		}
		result <- d
	}
	waitHeld := func(id uint64) {
		deadline := time.Now().Add(time.Second)
		for {
			if _, ok := i.IsHeld(id); ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("flow %d never held", id)
			}
			time.Sleep(time.Millisecond)
		}
	}

	go hold(1)
	waitHeld(1)
	edited := strings.Replace(EditableText(raw), "GET /", "GET /admin", 1)
	if err := i.Forward(1, &edited); err != nil {
		t.Fatal(err)
	}
	if d := <-result; d.Verdict != Forward || !d.Edited || !strings.HasPrefix(string(d.Raw), "GET /admin ") {
		t.Fatalf("forward decision = %+v", d)
	}
	if err := i.Forward(1, nil); err != ErrNotHeld {
		t.Fatalf("second forward err = %v", err)
	}

	go hold(2)
	waitHeld(2)
	i.Drop(2)
	if d := <-result; d.Verdict != Drop {
		t.Fatalf("drop decision = %+v", d)
	}

	go hold(3)
	waitHeld(3)
	i.SetEnabled(false)
	if d := <-result; d.Verdict != Forward || string(d.Raw) != string(raw) {
		t.Fatalf("disable should forward original, got %+v", d)
	}
}

func TestHoldEndsWithContext(t *testing.T) {
	i, _ := New(DefaultSettings())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := i.Hold(ctx, 1, Request, nil); err == nil {
		t.Fatal("expected context error")
	}
	if _, ok := i.IsHeld(1); ok {
		t.Fatal("cancelled hold left behind")
	}
}

func TestHeldEventsAndSnapshot(t *testing.T) {
	i, _ := New(DefaultSettings())
	i.SetEnabled(true)
	var mu sync.Mutex
	var events []HeldEvent
	unsub := i.Subscribe(func(e HeldEvent) { mu.Lock(); events = append(events, e); mu.Unlock() })
	defer unsub()

	done := make(chan Decision, 2)
	go func() { d, _ := i.Hold(context.Background(), 7, Request, []byte("GET /7 HTTP/1.1\r\n\r\n")); done <- d }()
	go func() {
		d, _ := i.Hold(context.Background(), 3, Response, []byte("HTTP/1.1 200 OK\r\n\r\n"))
		done <- d
	}()
	for len(i.Held()) < 2 {
		runtime.Gosched()
	}
	h := i.Held()
	if h[0].FlowID != 3 || h[0].Dir != Response || h[1].FlowID != 7 {
		t.Fatalf("Held = %+v", h)
	}
	if m, ok := i.Message(7); !ok || string(m.Raw) != "GET /7 HTTP/1.1\r\n\r\n" {
		t.Fatalf("Message(7) = %+v, %v", m, ok)
	}
	i.Drop(7)
	i.SetEnabled(false) // releases 3
	<-done
	<-done

	mu.Lock()
	defer mu.Unlock()
	held, released := 0, 0
	for _, e := range events {
		if e.Released {
			released++
		} else {
			held++
		}
	}
	if held != 2 || released != 2 {
		t.Fatalf("events = %+v", events)
	}
}

// A compressed, chunked response is readable in the view form, and goes back
// out byte for byte when nothing in it was changed.
func TestEditableViewDecodesAndRoundTrips(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte(`{"ok":true,"items":[1,2]}`))
	zw.Close()

	var body bytes.Buffer
	fmt.Fprintf(&body, "%x\r\n%s\r\n0\r\n\r\n", gzipped.Len(), gzipped.Bytes())
	orig := append([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"+
		"Content-Encoding: gzip\r\nTransfer-Encoding: chunked\r\n\r\n"), body.Bytes()...)

	view, note := EditableView(orig)
	if !strings.Contains(view, "\n  \"ok\": true,") {
		t.Errorf("body not decoded and laid out:\n%s", view)
	}
	if note != "dechunked · decoded from gzip · formatted as JSON" {
		t.Errorf("note = %q", note)
	}
	if got := ApplyEdit(orig, view, true); !bytes.Equal(got, orig) {
		t.Errorf("unchanged view altered the message:\n%q\nwant\n%q", got, orig)
	}
}

// Editing a decoded body re-applies the coding and framing the headers
// describe, so what goes out matches what they say.
func TestApplyEditRecompressesEditedBody(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte("hello"))
	zw.Close()
	orig := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: "+
		strconv.Itoa(gzipped.Len())+"\r\n\r\n"), gzipped.Bytes()...)

	view, _ := EditableView(orig)
	sent := ApplyEdit(orig, strings.Replace(view, "hello", "goodbye", 1), true)

	// The sent message decodes back to the edited text...
	again, _ := EditableView(sent)
	_, bodyText, _ := strings.Cut(again, "\n\n")
	if bodyText != "goodbye" {
		t.Errorf("edited body = %q, want %q", bodyText, "goodbye")
	}
	// ...and its Content-Length counts the compressed bytes actually sent.
	head, wire, _ := bytes.Cut(sent, []byte("\r\n\r\n"))
	if !bytes.Contains(head, []byte("Content-Length: "+strconv.Itoa(len(wire)))) {
		t.Errorf("content-length does not match the %d bytes sent: %q", len(wire), head)
	}
}

// Dropping the Content-Encoding header while editing sends the body as it
// reads, even when the body itself was not touched.
func TestApplyEditFollowsRemovedContentEncoding(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte("hello"))
	zw.Close()
	orig := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: "+
		strconv.Itoa(gzipped.Len())+"\r\n\r\n"), gzipped.Bytes()...)

	view, _ := EditableView(orig)
	sent := ApplyEdit(orig, strings.Replace(view, "Content-Encoding: gzip\n", "", 1), true)
	if got := string(sent); got != "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello" {
		t.Errorf("sent = %q", got)
	}
}

// A body that only the view form reformatted is still sent exactly as it
// arrived: formatting is a way of reading it, not a change to it.
func TestApplyEditKeepsUnformattedBytes(t *testing.T) {
	orig := []byte("POST / HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 9\r\n\r\n{\"a\":[1]}")
	view, note := EditableView(orig)
	if note != "formatted as JSON" {
		t.Fatalf("note = %q", note)
	}
	if got := ApplyEdit(orig, view, true); !bytes.Equal(got, orig) {
		t.Errorf("sent = %q, want the original bytes", got)
	}
}

// The raw form stays available, and stays raw: a compressed body is not
// editable as text there.
func TestEditableTextLeavesBodyOnTheWire(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte("hello"))
	zw.Close()
	orig := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"), gzipped.Bytes()...)

	if got := EditableText(orig); !strings.Contains(got, "bytes of binary data") {
		t.Errorf("raw form = %q", got)
	}
}

// An image is binary however it was sent, and its placeholder still stands
// for the bytes to keep.
func TestEditableViewBinaryBodyKeepsPlaceholder(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte{0x89, 'P', 'N', 'G', 0x00})
	zw.Close()
	orig := append([]byte("HTTP/1.1 200 OK\r\nContent-Type: image/png\r\nContent-Encoding: gzip\r\n\r\n"), gzipped.Bytes()...)

	view, _ := EditableView(orig)
	if !strings.Contains(view, "5 bytes of binary data") {
		t.Errorf("view = %q", view)
	}
	if got := ApplyEdit(orig, view, true); !bytes.Equal(got, orig) {
		t.Errorf("sent = %q, want the original bytes", got)
	}
}

// A stage sees each message before the user: its edit is what the user is
// shown (the arrival kept as the original), and its verdict, when it gives
// one, settles the message without the user seeing it.
func TestStage(t *testing.T) {
	i, err := New(Settings{})
	if err != nil {
		t.Fatal(err)
	}
	i.SetEnabled(true)
	raw := []byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")
	tagged := []byte("GET / HTTP/1.1\r\nHost: a\r\nX-Plugin: 1\r\n\r\n")
	drop := Drop
	forward := Forward

	// Edit: held for the user with the edit; forwarding unchanged sends it.
	i.SetStage(func(_ context.Context, p Pending) StageResult { return StageResult{Raw: tagged} })
	done := make(chan Decision, 1)
	go func() { d, _ := i.Hold(context.Background(), 1, Request, raw); done <- d }()
	waitHeld(t, i, 1)
	if m, _ := i.Message(1); string(m.Raw) != string(tagged) {
		t.Fatalf("user shown %q", m.Raw)
	}
	i.Forward(1, nil)
	if d := <-done; !d.Edited || string(d.Raw) != string(tagged) {
		t.Fatalf("decision = %+v", d)
	}

	// Release: decided without a hold.
	var events []HeldEvent
	unsub := i.Subscribe(func(e HeldEvent) { events = append(events, e) })
	i.SetStage(func(_ context.Context, p Pending) StageResult { return StageResult{Release: &drop} })
	if d, _ := i.Hold(context.Background(), 2, Request, raw); d.Verdict != Drop {
		t.Fatalf("decision = %+v", d)
	}
	i.SetStage(func(_ context.Context, p Pending) StageResult { return StageResult{Raw: tagged, Release: &forward} })
	if d, _ := i.Hold(context.Background(), 3, Request, raw); d.Verdict != Forward || !d.Edited {
		t.Fatalf("decision = %+v", d)
	}
	unsub()
	if len(events) != 0 {
		t.Errorf("a released message reached the queue: %+v", events)
	}

	// Interception switched off while the stage ran: forwarded as staged.
	i.SetStage(func(_ context.Context, p Pending) StageResult { i.SetEnabled(false); return StageResult{Raw: tagged} })
	if d, _ := i.Hold(context.Background(), 4, Request, raw); d.Verdict != Forward || string(d.Raw) != string(tagged) {
		t.Fatalf("decision = %+v", d)
	}
}

func waitHeld(t *testing.T, i *Interceptor, id uint64) {
	t.Helper()
	for n := 0; n < 200; n++ {
		if _, ok := i.IsHeld(id); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("never held")
}

func TestWithMethod(t *testing.T) {
	for _, tc := range []struct{ line, method, want string }{
		{"GET /a?b=c HTTP/1.1", "POST", "POST /a?b=c HTTP/1.1"},
		{"GET http://h/x HTTP/1.1", "PROPFIND", "PROPFIND http://h/x HTTP/1.1"},
		{"GET", "PUT", "PUT"},
		{"GET /a HTTP/1.1", "", "GET /a HTTP/1.1"},
		{"GET /a HTTP/1.1", "BAD METHOD", "GET /a HTTP/1.1"},
	} {
		if got := WithMethod(tc.line, tc.method); got != tc.want {
			t.Errorf("WithMethod(%q, %q) = %q, want %q", tc.line, tc.method, got, tc.want)
		}
	}
	if got := RequestMethod("DELETE /x HTTP/1.1"); got != "DELETE" {
		t.Errorf("RequestMethod = %q", got)
	}
	if got := RequestMethod(""); got != "" {
		t.Errorf("RequestMethod of an empty line = %q", got)
	}
}
