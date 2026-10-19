package replay

import (
	"context"
	"errors"
	"sync"
	"testing"

	"reproxy/internal/capture"
)

func fakeSender(calls *[]Request, mu *sync.Mutex) Sender {
	return func(_ context.Context, r Request) (Response, error) {
		mu.Lock()
		*calls = append(*calls, r)
		mu.Unlock()
		if string(r.Raw) == "fail" {
			return Response{}, errors.New("boom")
		}
		return Response{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n"), StatusCode: 200, Status: "200 OK"}, nil
	}
}

func TestSessionsSendRecordsHistoryAndEvents(t *testing.T) {
	var calls []Request
	var mu sync.Mutex
	s := NewSessions(fakeSender(&calls, &mu))

	var events []SessionEvent
	unsub := s.Subscribe(func(e SessionEvent) { events = append(events, e) })
	defer unsub()

	sess := s.Create(AgentOwner("a1"), Template{Addr: "app:80", Host: "app", Raw: []byte("GET / HTTP/1.1\r\n\r\n")})
	if sess.ID != 1 || sess.Owner != "agent:a1" {
		t.Fatalf("created %+v", sess)
	}
	if _, err := s.Send(context.Background(), sess.ID, []byte("ok"), OwnerUser); err != nil {
		t.Fatal(err)
	}
	e, err := s.Send(context.Background(), sess.ID, []byte("fail"), AgentOwner("a1"))
	if err == nil || e.Err != "boom" || e.Seq != 2 {
		t.Fatalf("failed send = %+v, %v", e, err)
	}
	if calls[0].Addr != "app:80" {
		t.Fatalf("sent to %q", calls[0].Addr)
	}

	got, _ := s.Get(sess.ID)
	if len(got.History) != 2 || got.History[0].Response.StatusCode != 200 || got.History[0].By != OwnerUser {
		t.Fatalf("history = %+v", got.History)
	}
	if l := s.List(); len(l) != 1 || l[0].History != nil || s.Sends(sess.ID) != 2 {
		t.Fatalf("list = %+v", l)
	}
	s.Close(sess.ID)
	if _, err := s.Send(context.Background(), sess.ID, []byte("ok"), OwnerUser); !errors.Is(err, ErrNoSession) {
		t.Fatalf("send after close: %v", err)
	}
	kinds := []SessionEventKind{SessionCreated, SessionSent, SessionSent, SessionClosed}
	if len(events) != len(kinds) {
		t.Fatalf("events = %+v", events)
	}
	for i, k := range kinds {
		if events[i].Kind != k {
			t.Fatalf("event %d = %v, want %v", i, events[i].Kind, k)
		}
	}
}

func TestSessionsHistoryIsBounded(t *testing.T) {
	var calls []Request
	var mu sync.Mutex
	s := NewSessions(fakeSender(&calls, &mu))
	sess := s.Create(OwnerUser, Template{Addr: "app:80"})
	for i := 0; i < maxHistory+5; i++ {
		s.Send(context.Background(), sess.ID, []byte("ok"), OwnerUser)
	}
	got, _ := s.Get(sess.ID)
	if len(got.History) != maxHistory || got.History[0].Seq != 6 || s.Sends(sess.ID) != maxHistory+5 {
		t.Fatalf("len %d first seq %d sends %d", len(got.History), got.History[0].Seq, s.Sends(sess.ID))
	}
}

// Sends show up in the capture store as flows, marked with who sent them,
// and described from the bytes actually sent.
func TestSessionsRecordSendsInTheStore(t *testing.T) {
	var calls []Request
	var mu sync.Mutex
	s := NewSessions(fakeSender(&calls, &mu))
	store := capture.NewStore()
	s.RecordTo(store)

	sess := s.Create(OwnerUser, Template{Addr: "app:443", Secure: true, Host: "app", TargetID: "t1"})
	s.Send(context.Background(), sess.ID, []byte("POST /edited?x=1 HTTP/1.1\r\nHost: api.app\r\n\r\nbody"), OwnerUser)
	s.Send(context.Background(), sess.ID, []byte("fail"), AgentOwner("a1"))

	flows := store.List()
	if len(flows) != 2 {
		t.Fatalf("store has %d flows", len(flows))
	}
	f := flows[0]
	if f.Source != capture.SourceReplay || f.State != capture.StateComplete || f.Method != "POST" ||
		f.Path != "/edited?x=1" || f.Host != "api.app" || !f.Secure || f.TargetID != "t1" ||
		f.StatusCode != 200 || f.StatusText != "200 OK" {
		t.Errorf("user send recorded as %+v", f)
	}
	if f := flows[1]; f.Source != capture.SourceAgent || f.State != capture.StateFailed || f.Error != "boom" || f.Host != "app" {
		t.Errorf("agent send recorded as %+v", f)
	}
}

func TestRequestHeadReadsAbsoluteForm(t *testing.T) {
	m, p, h := requestHead([]byte("GET http://other.test:8080/a/b?c HTTP/1.1\r\nHost: ignored\r\n\r\n"))
	if m != "GET" || p != "/a/b?c" || h != "other.test:8080" {
		t.Fatalf("requestHead = %q %q %q", m, p, h)
	}
}
