package capture

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"
)

func TestStoreLifecycleAndEvents(t *testing.T) {
	s := NewStore()
	var events []Event
	unsub := s.Subscribe(func(e Event) { events = append(events, e) })

	start := time.Now()
	id := s.Add(Flow{Method: "GET", Host: "localhost:3000", Path: "/", Started: start})
	s.Complete(id, Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n")}, 200, "200 OK", start.Add(time.Second))

	f, ok := s.Get(id)
	if !ok || f.State != StateComplete || f.StatusCode != 200 || f.Duration != time.Second {
		t.Fatalf("flow = %+v", f)
	}
	if len(events) != 2 || events[0].Kind != FlowAdded || events[1].Kind != FlowUpdated {
		t.Fatalf("events = %+v", events)
	}

	s.Clear()
	s.Complete(id, Message{}, 500, "500", time.Now()) // late update for a cleared flow
	if s.Len() != 0 || events[len(events)-1].Kind != FlowsCleared {
		t.Fatalf("after clear: len=%d last event=%v", s.Len(), events[len(events)-1].Kind)
	}
	if next := s.Add(Flow{}); next <= id {
		t.Fatalf("id reused after clear: %d", next)
	}

	unsub()
	s.Add(Flow{})
	if n := len(events); n != 4 {
		t.Fatalf("got %d events after unsubscribe, want 4", n)
	}
}

func TestDisplayText(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{"text body", Message{Raw: []byte("HTTP/1.1 200 OK\r\nA: b\r\n\r\nhi\r\nthere")}, "HTTP/1.1 200 OK\nA: b\n\nhi\nthere"},
		{"no body", Message{Raw: []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")}, "GET / HTTP/1.1\nHost: x\n\n"},
		{"binary body", Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n\x1f\x8b\x08\x00")}, "HTTP/1.1 200 OK\n\n‹4 B of binary data not shown›"},
		{"truncated", Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\nab"), Size: 4096, Truncated: true}, "HTTP/1.1 200 OK\n\nab\n\n‹truncated: showing 21 B of 4.0 KB›"},
	}
	for _, c := range cases {
		if got := DisplayText(c.msg); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestDisplayTextToleratesRuneCutByTruncation(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\n\r\ncafé")
	m := Message{Raw: raw[:len(raw)-1], Size: 100, Truncated: true}
	if got := DisplayText(m); strings.Contains(got, "binary") {
		t.Fatalf("truncated UTF-8 treated as binary: %q", got)
	}
}

// A compressed page reads as the page it is, with a note that the headers
// still describe the bytes that were on the wire.
func TestDisplayTextDecodesBody(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write([]byte("<html>hi</html>"))
	zw.Close()
	raw := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"), gzipped.Bytes()...)

	got := DisplayText(Message{Raw: raw, Size: int64(len(raw))})
	want := "HTTP/1.1 200 OK\nContent-Encoding: gzip\n\n<html>hi</html>\n\n" +
		"‹body decoded from gzip; the headers describe it as it was on the wire›"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// An update carries the state the flow had before it, so a subscriber can
// tell a transition (a held request forwarded) from a change in place.
func TestUpdateCarriesThePreviousState(t *testing.T) {
	s := NewStore()
	var got []Event
	s.Subscribe(func(e Event) { got = append(got, e) })
	id := s.Add(Flow{State: StateHeldRequest})
	s.Update(id, func(f *Flow) { f.State = StatePending })
	s.Complete(id, Message{}, 200, "200 OK", time.Now())
	if len(got) != 3 || got[1].Prev != StateHeldRequest || got[1].Flow.State != StatePending ||
		got[2].Prev != StatePending || got[2].Flow.State != StateComplete {
		t.Fatalf("events = %+v", got)
	}
}
