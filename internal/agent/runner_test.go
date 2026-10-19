package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// fakeProvider returns a scripted sequence of replies, one per Complete call,
// and records the history it was given each time.
type fakeProvider struct {
	replies []Reply
	calls   int
	seen    [][]Entry
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Complete(_ context.Context, _ string, history []Entry, _ []ToolSpec) (Reply, error) {
	f.seen = append(f.seen, append([]Entry(nil), history...))
	r := f.replies[f.calls]
	f.calls++
	return r, nil
}

type fakeProviders struct{ p Provider }

func (f fakeProviders) For(string) (Provider, error) { return f.p, nil }

// fakeTools records invocations and returns canned results.
type fakeTools struct {
	invoked []string
	turns   []Turn
}

func (t *fakeTools) Specs() []ToolSpec {
	return []ToolSpec{{Name: "get_flow", Schema: map[string]any{"type": "object"}}}
}
func (t *fakeTools) Invoke(_ context.Context, turn Turn, name string, _ json.RawMessage) (string, bool, error) {
	t.invoked = append(t.invoked, turn.Session+":"+name)
	t.turns = append(t.turns, turn)
	return `{"method":"GET","url":"http://a.test/x"}`, false, nil
}

func TestRunnerDrivesToolLoop(t *testing.T) {
	fp := &fakeProvider{replies: []Reply{
		{Text: "let me look", Calls: []ToolInvocation{{ID: "c1", Name: "get_flow", Input: json.RawMessage(`{"flow":"i.f1"}`)}}},
		{Text: "it's a GET to /x"},
	}}
	tools := &fakeTools{}
	r := NewLoopRunner(fakeProviders{fp}, tools)

	asked := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var msgs []Message
	convo, err := r.Run(context.Background(), Session{ID: "s1", Mode: ModeAuto,
		Messages: []Message{
			{Role: RoleUser, Text: "earlier", At: asked.Add(-time.Hour)},
			{Role: RoleAssistant, Text: "ok", At: asked.Add(-time.Minute)},
			{Role: RoleUser, Text: "look at flow 1", At: asked},
		},
		Convo: []Entry{{Role: EntryUser, Text: "look at flow 1"}}}, func(m Message) { msgs = append(msgs, m) })
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.invoked) != 1 || tools.invoked[0] != "s1:get_flow" {
		t.Fatalf("invoked = %v", tools.invoked)
	}
	// Tools learn when the message they answer was sent, so "this request"
	// resolves to what was focused then.
	if got := tools.turns[0]; got.Mode != ModeAuto || !got.AskedAt.Equal(asked) {
		t.Fatalf("turn = %+v, want asked at %v", got, asked)
	}
	// Display messages: assistant text, tool card, final assistant text.
	if len(msgs) != 3 || msgs[0].Role != RoleAssistant || msgs[1].Role != RoleTool || msgs[2].Text != "it's a GET to /x" {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[1].Tool == nil || msgs[1].Tool.Name != "get_flow" {
		t.Fatalf("tool card = %+v", msgs[1].Tool)
	}
	// Convo grew: user, assistant+call, user(result), assistant.
	if len(convo) != 4 || convo[2].Role != EntryUser || len(convo[2].Results) != 1 || convo[2].Results[0].ID != "c1" {
		t.Fatalf("convo = %+v", convo)
	}
	// The second Complete got the tool result.
	if len(fp.seen) != 2 || len(fp.seen[1]) != 3 {
		t.Fatalf("history progression = %v", fp.seen)
	}
}

// The focus is fetched on request (get_user_focus), never put in the
// prompt, which stays the same every turn.
func TestRunnerSystemPromptHasScopeAndPointsAtFocusTool(t *testing.T) {
	if !contains(systemPrompt, "same registrable domain") || !contains(systemPrompt, "get_user_focus") {
		t.Fatalf("system prompt = %q", systemPrompt)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
