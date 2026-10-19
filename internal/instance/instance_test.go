package instance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/focus"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

type fixture struct {
	store   *capture.Store
	ic      *intercept.Interceptor
	replays *replay.Sessions
	focus   *focus.Tracker
	local   *Local
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ic, err := intercept.New(intercept.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		store: capture.NewStore(),
		ic:    ic,
		replays: replay.NewSessions(func(_ context.Context, r replay.Request) (replay.Response, error) {
			return replay.Response{Raw: append([]byte("HTTP/1.1 200 OK\r\n\r\necho:"), r.Raw...), StatusCode: 200, Status: "200 OK"}, nil
		}),
		focus: &focus.Tracker{},
	}
	f.local = NewLocal(Info{ID: "abc", PID: 1}, f.store, ic, f.replays, f.focus,
		func() (string, error) { return "127.0.0.1:7193", nil })
	return f
}

func (f *fixture) addFlow(method, host, path, body string) uint64 {
	raw := method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n" + body
	return f.store.Add(capture.Flow{Method: method, Host: host, Path: path, Target: host + ":80",
		Request: capture.Message{Raw: []byte(raw), Size: int64(len(raw))}})
}

// remote serves the fixture over HTTP and returns a Remote for it.
func (f *fixture) remote(t *testing.T) *Remote {
	t.Helper()
	srv := httptest.NewServer(Handler(f.local, func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer tok"
	}))
	t.Cleanup(srv.Close)
	return NewRemote(strings.TrimPrefix(srv.URL, "http://"), func() (string, error) { return "tok", nil })
}

// Both implementations must behave the same, so every behaviour test runs
// against Local and against Remote over HTTP.
func implementations(t *testing.T) map[string]func(*fixture) Instance {
	return map[string]func(*fixture) Instance{
		"local":  func(f *fixture) Instance { return f.local },
		"remote": func(f *fixture) Instance { return f.remote(t) },
	}
}

func TestListAndSearchFlows(t *testing.T) {
	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.addFlow("GET", "a.test", "/one", "")
			f.addFlow("POST", "a.test", "/login", "user=admin&password=x")
			f.addFlow("GET", "b.test", "/two", "")
			inst := mk(f)
			ctx := context.Background()

			p, err := inst.ListFlows(ctx, FlowQuery{Limit: 2})
			if err != nil || len(p.Flows) != 2 || !p.More || p.Total != 3 || p.Flows[0].URL != "http://a.test/one" {
				t.Fatalf("page = %+v, %v", p, err)
			}
			p, _ = inst.ListFlows(ctx, FlowQuery{After: p.Flows[1].ID})
			if len(p.Flows) != 1 || p.Flows[0].Method != "GET" || p.More {
				t.Fatalf("page 2 = %+v", p)
			}
			p, _ = inst.ListFlows(ctx, FlowQuery{Filter: "password=", In: InBody})
			if p.Matched != 1 || p.Flows[0].Method != "POST" {
				t.Fatalf("body search = %+v", p)
			}
			p, _ = inst.ListFlows(ctx, FlowQuery{Newest: true, Limit: 1})
			if p.Flows[0].URL != "http://b.test/two" {
				t.Fatalf("newest = %+v", p)
			}
			if _, err := inst.ListFlows(ctx, FlowQuery{Filter: "(", Regex: true}); err == nil {
				t.Fatal("bad regex accepted")
			}
			if _, err := inst.GetFlow(ctx, 99); !errors.Is(err, ErrNoFlow) {
				t.Fatalf("GetFlow(99) = %v", err)
			}
			fl, err := inst.GetFlow(ctx, 2)
			if err != nil || !strings.Contains(string(fl.Request.Raw), "password=x") {
				t.Fatalf("GetFlow(2) = %+v, %v", fl, err)
			}
			info, _ := inst.Info(ctx)
			if info.ID != "abc" || info.ProxyAddr != "127.0.0.1:7193" {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestInterceptOperations(t *testing.T) {
	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			inst := mk(f)
			ctx := context.Background()
			if err := inst.SetIntercept(ctx, true); err != nil || !f.ic.Enabled() {
				t.Fatalf("SetIntercept: %v", err)
			}
			if _, ok, _ := inst.WaitHeld(ctx, 20*time.Millisecond); ok {
				t.Fatal("WaitHeld with nothing held returned ok")
			}

			id := f.addFlow("GET", "a.test", "/held", "")
			fl, _ := f.store.Get(id)
			decided := make(chan intercept.Decision, 1)
			go func() {
				d, _ := f.ic.Hold(ctx, id, intercept.Request, fl.Request.Raw)
				decided <- d
			}()
			h, ok, err := inst.WaitHeld(ctx, 5*time.Second)
			if err != nil || !ok || h.FlowID != id || h.Direction != "request" || h.URL != "http://a.test/held" {
				t.Fatalf("WaitHeld = %+v, %v, %v", h, ok, err)
			}
			st, _ := inst.Intercept(ctx)
			if !st.Enabled || len(st.Held) != 1 {
				t.Fatalf("state = %+v", st)
			}
			edited := intercept.EditableText(fl.Request.Raw)
			edited = strings.Replace(edited, "/held", "/edited", 1)
			if err := inst.Forward(ctx, id, &edited); err != nil {
				t.Fatal(err)
			}
			if d := <-decided; !d.Edited || !strings.Contains(string(d.Raw), "/edited") {
				t.Fatalf("decision = %+v", d)
			}
			if err := inst.Drop(ctx, id); !errors.Is(err, ErrNotHeld) {
				t.Fatalf("Drop after forward = %v", err)
			}
		})
	}
}

func TestReplayAndFocus(t *testing.T) {
	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			id := f.addFlow("GET", "a.test", "/r", "")
			inst := mk(f)
			ctx := context.Background()

			s, err := inst.ReplayCreate(ctx, replay.AgentOwner("s1"), id, "")
			if err != nil || s.Template.Addr != "a.test:80" || s.Template.Host != "a.test" {
				t.Fatalf("create = %+v, %v", s, err)
			}
			e, err := inst.ReplaySend(ctx, s.ID, []byte("GET /x HTTP/1.1\r\n\r\n"), replay.AgentOwner("s1"))
			if err != nil || e.Response.StatusCode != 200 || e.Seq != 1 {
				t.Fatalf("send = %+v, %v", e, err)
			}
			got, err := inst.ReplayGet(ctx, s.ID)
			if err != nil || len(got.History) != 1 || got.History[0].By != "agent:s1" {
				t.Fatalf("get = %+v, %v", got, err)
			}
			if l, _ := inst.ReplayList(ctx); len(l) != 1 {
				t.Fatalf("list = %+v", l)
			}
			if _, err := inst.ReplaySend(ctx, 42, nil, replay.OwnerUser); !errors.Is(err, ErrNoSession) {
				t.Fatalf("send to unknown session = %v", err)
			}
			f.focus.ActivateReplay(s.ID, "draft")
			fs, _ := inst.Focus(ctx, time.Time{})
			if fs.ReplaySession != s.ID || fs.ReplayDraft != "draft" || fs.Recent != "replay" {
				t.Fatalf("focus = %+v", fs)
			}
			// The focus at an earlier moment, as "this request" in a chat
			// message sent then means.
			asked := time.Now()
			f.focus.SelectFlow(id)
			if fs, err := inst.Focus(ctx, asked); err != nil || fs.SelectedFlow != 0 || fs.ReplaySession != s.ID {
				t.Fatalf("focus at %v = %+v, %v", asked, fs, err)
			}
			if fs, _ := inst.Focus(ctx, time.Time{}); fs.SelectedFlow != id {
				t.Fatalf("focus now = %+v", fs)
			}
			if _, err := inst.Focus(ctx, asked.Add(-time.Hour)); err != nil {
				t.Fatalf("focus before any change = %v", err) // nothing focused, not forgotten
			}
			for i := range 1001 {
				f.focus.SelectFlow(uint64(i))
			}
			if _, err := inst.Focus(ctx, asked); !errors.Is(err, focus.ErrTooOld) {
				t.Fatalf("focus long ago = %v, want ErrTooOld", err)
			}
		})
	}
}

func TestRemoteRequiresToken(t *testing.T) {
	f := newFixture(t)
	r := f.remote(t)
	r.token = func() (string, error) { return "wrong", nil }
	if _, err := r.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Info with wrong token = %v", err)
	}
}

// The command line reaches an instance the same way an agent does, so a
// command must behave identically whether it is run in this process or in
// another one over HTTP.
func TestRunCommand(t *testing.T) {
	reg := cli.NewRegistry()
	err := reg.Add(cli.Command{
		Namespace: "plugins", Action: "ls", Summary: "list them",
		Flags: []cli.Flag{{Name: "name", Type: cli.FlagString, Default: "all"}},
		Run: func(_ context.Context, in *cli.Input) (cli.Result, error) {
			if in.String("name") == "boom" {
				return cli.Result{}, errors.New("it went wrong")
			}
			return cli.Data("listing "+in.String("name"), map[string]string{"name": in.String("name")})
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.local.SetCommands(reg)
			inst := mk(f)
			ctx := context.Background()

			res, err := inst.RunCommand(ctx, cli.Request{Namespace: "plugins", Action: "ls", Argv: []string{"--name", "x"}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != "listing x" || string(res.JSON) != `{"name":"x"}` {
				t.Errorf("got %+v", res)
			}

			// A flag the command doesn't take is an answer, not an error,
			// and it carries the usage.
			res, err = inst.RunCommand(ctx, cli.Request{Namespace: "plugins", Action: "ls", Argv: []string{"--nope"}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != cli.UsageCode || !strings.Contains(res.Error, "--nope") || !strings.Contains(res.Output, "Usage:") {
				t.Errorf("got %+v", res)
			}

			// A command that fails is an error, with its message intact.
			if _, err := inst.RunCommand(ctx, cli.Request{Namespace: "plugins", Action: "ls", Argv: []string{"--name", "boom"}}); err == nil {
				t.Error("a failing command reported no error")
			} else if !strings.Contains(err.Error(), "it went wrong") {
				t.Errorf("error is %v", err)
			}
		})
	}
}

// A process with no command line refuses rather than half-answering.
func TestRunCommandWithoutRegistry(t *testing.T) {
	f := newFixture(t)
	if _, err := f.local.RunCommand(context.Background(), cli.Request{Namespace: "plugins", Action: "ls"}); err == nil {
		t.Error("an instance with no registry answered a command")
	}
}
