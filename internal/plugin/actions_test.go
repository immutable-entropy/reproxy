package plugin

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/replay"
)

const actor = `{
	"name": "actor", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
	"actions": [
		{"id": "retry", "label": "Send it again", "function": "retry", "match": {"methods": ["GET"], "hosts": ["app.test"]}},
		{"id": "boom", "label": "Fail", "description": "Always throws", "function": "boom"}
	]
}`

const actorTS = `
import { http, type ActionHandler } from "reproxy";
export const retry: ActionHandler = async ({ action, flow }) => {
	const r = await http.send({ url: flow.url + "?again=1" });
	return { message: action + ": " + flow.method + " " + flow.path + " answered " + r.status };
};
export function boom() { throw new Error("nope"); }
`

// actionEnv runs the actor plugin, and records what its sends dial: the
// upstream chain (deps.Dial) for a direct send.
func actionEnv(t *testing.T) (*Deps, *Manager, chan string) {
	t.Helper()
	deps, m := env(t, pluginFS("actor", actor, actorTS))
	dialled := make(chan string, 10)
	deps.Dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled <- addr
		return nil, errors.New("test: not connected")
	}
	deps.Send = func(ctx context.Context, r replay.Request) (replay.Response, error) {
		if r.Dial == nil {
			t.Error("a plugin's send was given no dial function: it would connect directly")
		} else {
			_, _ = r.Dial(ctx, "tcp", r.Addr)
		}
		raw := []byte("HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n")
		return replay.Response{Raw: raw, Size: len(raw), StatusCode: 201, Status: "201 Created"}, nil
	}
	m.Rescan()
	m.Apply(Config{})
	return deps, m, dialled
}

func flowOf(t *testing.T, deps *Deps, method, host string) capture.Flow {
	t.Helper()
	id := deps.Store.Add(capture.Flow{Method: method, Host: host, Path: "/users/1", Secure: true,
		Request: capture.Message{Raw: []byte(method + " /users/1 HTTP/1.1\r\nHost: " + host + "\r\n\r\n")}, Started: time.Now()})
	deps.Store.Complete(id, capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n")}, 200, "200 OK", time.Now())
	f, _ := deps.Store.Get(id)
	return f
}

func actionIDs(as []Action) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Plugin+"/"+a.ID)
	}
	return out
}

func TestActionsAreOfferedWhereTheyApply(t *testing.T) {
	deps, m, _ := actionEnv(t)
	got := actionIDs(m.Actions(flowOf(t, deps, "GET", "app.test")))
	if strings.Join(got, ",") != "actor/retry,actor/boom" {
		t.Errorf("GET app.test: %v", got)
	}
	got = actionIDs(m.Actions(flowOf(t, deps, "POST", "app.test")))
	if strings.Join(got, ",") != "actor/boom" {
		t.Errorf("POST app.test: %v", got)
	}
	as := m.Actions(flowOf(t, deps, "GET", "other.test"))
	if len(as) != 1 || as[0].Label != "Fail" || as[0].Description != "Always throws" {
		t.Errorf("other.test: %+v", as)
	}

	m.Apply(Config{Overrides: map[string]Override{"actor": {Status: manifest.Disabled}}})
	if as := m.Actions(flowOf(t, deps, "GET", "app.test")); len(as) != 0 {
		t.Errorf("a disabled plugin offers %v", actionIDs(as))
	}
}

func TestRunActionSendsThroughTheUpstreamChain(t *testing.T) {
	deps, m, dialled := actionEnv(t)
	res, err := m.RunAction("actor", "retry", flowOf(t, deps, "GET", "app.test"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Message != "retry: GET /users/1 answered 201" {
		t.Errorf("message = %q", res.Message)
	}
	select {
	case addr := <-dialled:
		if addr != "app.test:443" {
			t.Errorf("dialled %q", addr)
		}
	default:
		t.Error("the action's request didn't go through the upstream dialer")
	}
}

func TestRunActionReportsWhatWentWrong(t *testing.T) {
	deps, m, _ := actionEnv(t)
	if _, err := m.RunAction("actor", "boom", flowOf(t, deps, "GET", "app.test")); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("a throwing action: %v", err)
	}
	if _, err := m.RunAction("actor", "retry", flowOf(t, deps, "POST", "app.test")); err == nil || !strings.Contains(err.Error(), "doesn't apply") {
		t.Errorf("an action its filter turns away: %v", err)
	}
	if _, err := m.RunAction("actor", "missing", flowOf(t, deps, "GET", "app.test")); err == nil {
		t.Error("no such action, no error")
	}
	if _, err := m.RunAction("nobody", "retry", flowOf(t, deps, "GET", "app.test")); err == nil {
		t.Error("no such plugin, no error")
	}
}

func TestPluginSendsRefuseToConnectDirectly(t *testing.T) {
	deps, m, _ := actionEnv(t)
	deps.Dial = nil
	_, err := m.RunAction("actor", "retry", flowOf(t, deps, "GET", "app.test"))
	if err == nil || !strings.Contains(err.Error(), "may not connect directly") {
		t.Errorf("with no upstream dialer: %v", err)
	}
}
