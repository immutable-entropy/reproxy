package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"reproxy/internal/approval"
	"reproxy/internal/archive"
	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/focus"
	"reproxy/internal/instance"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

const testInstance = "abcd1234"

// fixture is a Registry over a single Local instance with a fake sender,
// and a findings store over a real (temporary) engagement database.
type fixture struct {
	entities *entities.Store
	store    *capture.Store
	ic       *intercept.Interceptor
	replays  *replay.Sessions
	focus    *focus.Tracker
	notes    *findings.Store
	files    *files.Store
	reg      *Registry
	broker   *approval.Broker
}

type oneRouter struct {
	id    string
	local *instance.Local
}

func (r oneRouter) LocalID() string { return r.id }
func (r oneRouter) Get(id string) (instance.Instance, bool) {
	if id == r.id {
		return r.local, true
	}
	return nil, false
}
func (r oneRouter) Instances(ctx context.Context) []instance.Info {
	i, _ := r.local.Info(ctx)
	return []instance.Info{i}
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
			return replay.Response{
				Raw:        append([]byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"), 0),
				StatusCode: 200, Status: "200 OK",
			}, nil
		}),
		focus:  &focus.Tracker{},
		broker: approval.NewBroker(0),
	}
	// The findings tools write into the engagement's capture.db, so the
	// fixture gives them a real one in a temporary directory.
	arch, err := archive.Open(filepath.Join(t.TempDir(), archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { arch.Close() })
	f.notes = findings.NewStore()
	if err := f.notes.SetTypes(testFindingTypes); err != nil {
		t.Fatal(err)
	}
	if err := f.notes.Use(arch.DB()); err != nil {
		t.Fatal(err)
	}
	f.entities = entities.NewStore()
	if err := f.entities.Use(arch.DB()); err != nil {
		t.Fatal(err)
	}

	// The file tools read the engagement's recovered files: their records
	// in the same database, their bytes in a temporary directory.
	f.files = files.NewStore()
	if err := f.files.Use(arch.DB(), filepath.Join(t.TempDir(), "recovered")); err != nil {
		t.Fatal(err)
	}

	local := instance.NewLocal(
		instance.Info{ID: testInstance, PID: 1}, f.store, ic, f.replays, f.focus,
		func() (string, error) { return "127.0.0.1:7193", nil })
	f.reg = New(oneRouter{id: testInstance, local: local}, f.broker, f.notes, f.entities)
	f.reg.SetFiles(f.files)
	return f
}

// testFindingTypes stands in for the user's catalog from Settings → Findings.
var testFindingTypes = []findings.Type{
	{ID: "credential", Name: "Credential", Color: "#d6453d"},
	{ID: "sensitive-data", Name: "Sensitive Data", Color: "#d08700"},
}

func (f *fixture) addFlow(method, host, path, body string) uint64 {
	raw := method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n" + body
	return f.store.Add(capture.Flow{Method: method, Host: host, Path: path, Target: host + ":80",
		Request: capture.Message{Raw: []byte(raw), Size: int64(len(raw))}})
}

// call runs a tool and decodes its JSON result into out.
func (f *fixture) call(t *testing.T, c Caller, name string, args map[string]any, out any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := f.reg.Call(context.Background(), c, name, raw)
	if err != nil {
		return err
	}
	if out != nil {
		b, _ := json.Marshal(res)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decoding %s result: %v", name, err)
		}
	}
	return nil
}

var auto = Caller{Policy: config.PolicyAuto}

func TestHandleRoundTrip(t *testing.T) {
	if got := flowHandle("ab12", 7); got != "ab12.f7" {
		t.Fatalf("flowHandle = %q", got)
	}
	if got := sessionHandle("ab12", 3); got != "ab12.r3" {
		t.Fatalf("sessionHandle = %q", got)
	}
	inst, id, err := parseFlow("ab12.f7")
	if err != nil || inst != "ab12" || id != 7 {
		t.Fatalf("parseFlow = %q, %d, %v", inst, id, err)
	}
	inst, sid, err := parseSession("ab12.r3")
	if err != nil || inst != "ab12" || sid != 3 {
		t.Fatalf("parseSession = %q, %d, %v", inst, sid, err)
	}
	for _, bad := range []string{"", "nodot", "ab12.x7", "ab12.f", "ab12.fxx", ".f7"} {
		if _, _, err := parseFlow(bad); err == nil {
			t.Errorf("parseFlow(%q) accepted", bad)
		}
	}
}

func TestListAndGetFlow(t *testing.T) {
	f := newFixture(t)
	f.addFlow("GET", "a.test", "/one", "")
	f.addFlow("POST", "a.test", "/login", "secret=hunter2")

	var flows struct {
		Instance string `json:"instance"`
		Flows    []struct {
			Handle string `json:"handle"`
			URL    string `json:"url"`
		} `json:"flows"`
		Total int `json:"total"`
	}
	if err := f.call(t, auto, "list_flows", nil, &flows); err != nil {
		t.Fatal(err)
	}
	if flows.Total != 2 || len(flows.Flows) != 2 || flows.Flows[0].Handle != "abcd1234.f1" {
		t.Fatalf("list_flows = %+v", flows)
	}

	// search_flows finds the body text that a summary filter would miss.
	var found struct {
		Flows []struct {
			Handle string `json:"handle"`
		} `json:"flows"`
	}
	if err := f.call(t, auto, "search_flows", map[string]any{"query": "hunter2"}, &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Flows) != 1 || found.Flows[0].Handle != "abcd1234.f2" {
		t.Fatalf("search_flows = %+v", found)
	}

	var detail struct {
		Part string `json:"part"`
		Text string `json:"text"`
	}
	if err := f.call(t, auto, "get_flow", map[string]any{"flow": "abcd1234.f2"}, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Part != "request" || !strings.Contains(detail.Text, "secret=hunter2") {
		t.Fatalf("get_flow = %+v", detail)
	}
}

func TestGetFlowPaging(t *testing.T) {
	f := newFixture(t)
	body := strings.Repeat("x", maxText+500)
	id := f.addFlow("POST", "a.test", "/big", body)
	h := flowHandle(testInstance, id)

	var p1 struct {
		Text       string `json:"text"`
		More       bool   `json:"more"`
		NextOffset int    `json:"next_offset"`
	}
	if err := f.call(t, auto, "get_flow", map[string]any{"flow": h}, &p1); err != nil {
		t.Fatal(err)
	}
	if !p1.More || len(p1.Text) != maxText || p1.NextOffset != maxText {
		t.Fatalf("page 1: more=%v len=%d next=%d", p1.More, len(p1.Text), p1.NextOffset)
	}
	var p2 struct {
		More bool `json:"more"`
	}
	if err := f.call(t, auto, "get_flow", map[string]any{"flow": h, "offset": p1.NextOffset}, &p2); err != nil {
		t.Fatal(err)
	}
	if p2.More {
		t.Fatalf("page 2 still has more")
	}
}

func TestSetInterceptAndHeld(t *testing.T) {
	f := newFixture(t)
	if err := f.call(t, auto, "set_intercept", map[string]any{"enabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	if !f.ic.Enabled() {
		t.Fatal("interception not enabled")
	}

	id := f.addFlow("GET", "a.test", "/held", "")
	fl, _ := f.store.Get(id)
	decided := make(chan intercept.Decision, 1)
	go func() {
		d, _ := f.ic.Hold(context.Background(), id, intercept.Request, fl.Request.Raw)
		decided <- d
	}()

	var held struct {
		OK   bool `json:"ok"`
		Held struct {
			Handle    string `json:"handle"`
			Direction string `json:"direction"`
		} `json:"held"`
	}
	if err := f.call(t, auto, "wait_for_held", map[string]any{"timeout_ms": 5000}, &held); err != nil {
		t.Fatal(err)
	}
	if !held.OK || held.Held.Handle != flowHandle(testInstance, id) || held.Held.Direction != "request" {
		t.Fatalf("wait_for_held = %+v", held)
	}

	edited := intercept.EditableText(fl.Request.Raw)
	edited = strings.Replace(edited, "/held", "/edited", 1)
	if err := f.call(t, auto, "forward_held", map[string]any{"flow": held.Held.Handle, "edited": edited}, nil); err != nil {
		t.Fatal(err)
	}
	if d := <-decided; !d.Edited || !strings.Contains(string(d.Raw), "/edited") {
		t.Fatalf("decision = %+v", d)
	}
	// It is no longer held.
	if err := f.call(t, auto, "drop_held", map[string]any{"flow": held.Held.Handle}, nil); err == nil {
		t.Fatal("drop_held after forward should fail")
	}
}

func TestReplayAndSendRequest(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/r", "")
	fh := flowHandle(testInstance, id)

	var created struct {
		Session string `json:"session"`
	}
	if err := f.call(t, auto, "replay_create", map[string]any{"flow": fh}, &created); err != nil {
		t.Fatal(err)
	}
	if created.Session != sessionHandle(testInstance, 1) {
		t.Fatalf("replay_create = %+v", created)
	}

	req := "GET /r HTTP/1.1\nHost: a.test\n\n"
	var sent struct {
		Seq      int    `json:"seq"`
		Response string `json:"response"`
	}
	if err := f.call(t, auto, "replay_send", map[string]any{"session": created.Session, "request": req}, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Seq != 1 || !strings.Contains(sent.Response, "200 OK") {
		t.Fatalf("replay_send = %+v", sent)
	}

	// send_request creates-and-sends in one step.
	var one struct {
		Session  string `json:"session"`
		Response string `json:"response"`
	}
	if err := f.call(t, auto, "send_request", map[string]any{"flow": fh, "request": req}, &one); err != nil {
		t.Fatal(err)
	}
	if one.Session != sessionHandle(testInstance, 2) || !strings.Contains(one.Response, "200 OK") {
		t.Fatalf("send_request = %+v", one)
	}
}

func TestScopeRefusals(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/r", "")
	fh := flowHandle(testInstance, id)

	// replay_create to a different domain.
	if err := f.call(t, auto, "replay_create", map[string]any{"flow": fh, "target": "evil.test:443"}, nil); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("out-of-scope target = %v", err)
	}
	// A sibling host on the same registrable domain is allowed.
	if err := f.call(t, auto, "replay_create", map[string]any{"flow": fh, "target": "www.a.test:443"}, nil); err != nil {
		t.Fatalf("in-scope target refused: %v", err)
	}

	// send_request with the Host header moved out of scope.
	evil := "GET /r HTTP/1.1\nHost: evil.test\n\n"
	if err := f.call(t, auto, "send_request", map[string]any{"flow": fh, "request": evil}, nil); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("out-of-scope Host = %v", err)
	}
}

func TestApprovalPolicies(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/r", "")
	fh := flowHandle(testInstance, id)

	// read_only refuses every mutating tool before it acts.
	ro := Caller{Policy: config.PolicyReadOnly}
	for _, name := range []string{"set_intercept", "forward_held", "drop_held", "replay_create", "replay_send", "send_request"} {
		res, err := f.reg.Call(context.Background(), ro, name, json.RawMessage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s under read_only = %v (res %v)", name, err, res)
		}
	}
	// A read-only tool still works for a read-only caller.
	if _, err := f.reg.Call(context.Background(), ro, "list_flows", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("list_flows under read_only = %v", err)
	}

	// ask mode blocks until the user decides.
	ask := Caller{Session: "s1", Policy: config.PolicyAsk}
	done := make(chan error, 1)
	go func() {
		done <- f.call(t, ask, "replay_create", map[string]any{"flow": fh}, nil)
	}()
	select {
	case err := <-done:
		t.Fatalf("replay_create returned before approval: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	reqs := waitPending(t, f.broker, 1)
	if reqs[0].Tool != "replay_create" || reqs[0].Session != "s1" || reqs[0].Summary == "" {
		t.Fatalf("pending request = %+v", reqs[0])
	}
	if err := f.broker.Decide(reqs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("approved replay_create failed: %v", err)
	}

	// A denial surfaces as an error.
	go func() { done <- f.call(t, ask, "set_intercept", map[string]any{"enabled": true}, nil) }()
	reqs = waitPending(t, f.broker, 1)
	f.broker.Decide(reqs[0].ID, false)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied set_intercept = %v", err)
	}
}

func waitPending(t *testing.T, b *approval.Broker, n int) []approval.Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if p := b.Pending(); len(p) == n {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d pending, have %d", n, len(b.Pending()))
		}
		time.Sleep(time.Millisecond)
	}
}

// "this request" is what the user had focused when they sent the message,
// even after they click elsewhere; now:true and at override that.
func TestGetUserFocusResolvesTheMessageTime(t *testing.T) {
	f := newFixture(t)
	login := f.addFlow("POST", "a.test", "/login", "")
	other := f.addFlow("GET", "a.test", "/other", "")
	f.focus.SelectFlow(login)
	asked := time.Now()
	f.focus.SelectFlow(other) // the user moves on while the agent works

	type out struct {
		AsOf         string `json:"as_of"`
		Recent       string `json:"recent"`
		Flow         string `json:"selected_flow"`
		Request      string `json:"selected_flow_request"`
		ChangedSince *bool  `json:"changed_since"`
	}
	chat := Caller{Session: "s1", Policy: config.PolicyAsk, AskedAt: asked}

	var got out
	if err := f.call(t, chat, "get_user_focus", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Flow != flowHandle(testInstance, login) || got.Request != "POST http://a.test/login" ||
		got.Recent != "flow" || got.ChangedSince == nil || !*got.ChangedSince {
		t.Fatalf("at the message = %+v", got)
	}
	// as_of can be handed back as at and means the same moment.
	if back, err := time.Parse(time.RFC3339, got.AsOf); err != nil || !back.Equal(asked) {
		t.Fatalf("as_of %q doesn't round-trip to %v: %v", got.AsOf, asked, err)
	}

	got = out{}
	if err := f.call(t, chat, "get_user_focus", map[string]any{"now": true}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Flow != flowHandle(testInstance, other) || got.ChangedSince != nil {
		t.Fatalf("now = %+v", got)
	}

	// An external client has no message time: it gets the focus now, or at
	// a time it names.
	got = out{}
	if err := f.call(t, auto, "get_user_focus", nil, &got); err != nil || got.Flow != flowHandle(testInstance, other) {
		t.Fatalf("external = %+v, %v", got, err)
	}
	got = out{}
	at := asked.UTC().Format(time.RFC3339Nano)
	if err := f.call(t, auto, "get_user_focus", map[string]any{"at": at}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Flow != flowHandle(testInstance, login) {
		t.Fatalf("at %s = %+v", at, got)
	}

	for i := range 1001 {
		f.focus.SelectFlow(uint64(i))
	}
	err := f.call(t, chat, "get_user_focus", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ask the user") {
		t.Fatalf("forgotten focus = %v", err)
	}
}

// TestPageAndCappedKeepRunesWhole covers the bug these two shared with the
// rest of the tree: cutting arbitrary captured traffic at a byte offset
// splits a multi-byte rune, and an agent is handed text it cannot parse.
func TestPageAndCappedKeepRunesWhole(t *testing.T) {
	// A body of 3-byte runes, long enough that no page boundary lands on
	// one: maxText is not a multiple of 3.
	body := strings.Repeat("日", maxText) // 3*maxText bytes
	if maxText%3 == 0 {
		t.Fatalf("maxText %d is a multiple of 3; pick another rune for this test", maxText)
	}

	var pages int
	for offset := 0; ; pages++ {
		chunk, next := page(body, offset)
		if !utf8.ValidString(chunk) {
			t.Fatalf("page at offset %d is not valid UTF-8", offset)
		}
		if strings.ContainsRune(chunk, '�') {
			t.Fatalf("page at offset %d contains U+FFFD", offset)
		}
		if next == 0 {
			break
		}
		if next <= offset {
			t.Fatalf("page at offset %d did not advance (next=%d)", offset, next)
		}
		offset = next
	}
	if pages < 2 {
		t.Fatalf("expected the body to span several pages, got %d", pages)
	}

	// Nothing is lost to the backed-off boundary: the pages rejoin into
	// exactly the body.
	var rejoined strings.Builder
	for offset := 0; ; {
		chunk, next := page(body, offset)
		rejoined.WriteString(chunk)
		if next == 0 {
			break
		}
		offset = next
	}
	if rejoined.String() != body {
		t.Errorf("rejoined pages differ from the body (%d bytes vs %d)", rejoined.Len(), len(body))
	}

	if out := capped(body); !utf8.ValidString(out) || strings.ContainsRune(out, '�') {
		t.Errorf("capped produced invalid UTF-8")
	}
	if out := capped(body); !strings.HasSuffix(out, "‹truncated›") {
		t.Errorf("capped did not mark the cut (%d bytes in, %d out)", len(body), len(out))
	} else if len(out) > maxText+len("\n‹truncated›") {
		t.Errorf("capped returned %d bytes, over the %d-byte budget", len(out), maxText)
	}
}
