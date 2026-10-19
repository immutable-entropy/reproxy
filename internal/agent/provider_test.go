package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each provider must translate a two-turn conversation (user message → tool
// call → tool result → final text) to and from its vendor's wire format.

func toolSpecs() []ToolSpec {
	return []ToolSpec{{Name: "get_flow", Description: "read a flow", Schema: map[string]any{
		"type": "object", "properties": map[string]any{"flow": map[string]any{"type": "string"}},
	}}}
}

func historyWithToolRoundTrip() []Entry {
	return []Entry{
		{Role: EntryUser, Text: "look at flow 1"},
		{Role: EntryAssistant, Text: "checking", Calls: []ToolInvocation{{ID: "call1", Name: "get_flow", Input: json.RawMessage(`{"flow":"i.f1"}`)}}},
		{Role: EntryUser, Results: []ToolOutcome{{ID: "call1", Content: "GET /x 200"}}},
	}
}

func TestAnthropicProvider(t *testing.T) {
	var gotReq anthropicReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("bad request: %s %s key=%q", r.Method, r.URL.Path, r.Header.Get("x-api-key"))
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		io.WriteString(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"flow 1 is a GET"}]}`)
	}))
	defer srv.Close()

	p, err := NewProvider("anthropic", "claude-opus-5-5", srv.URL, "k", 1000)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := p.Complete(context.Background(), "be helpful", historyWithToolRoundTrip(), toolSpecs())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "flow 1 is a GET" || len(reply.Calls) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
	if gotReq.System != "be helpful" || gotReq.Model != "claude-opus-5-5" || len(gotReq.Tools) != 1 {
		t.Fatalf("request = %+v", gotReq)
	}
	// The tool_use and tool_result must have been sent, paired by ID.
	if len(gotReq.Messages) != 3 {
		t.Fatalf("messages = %d", len(gotReq.Messages))
	}
	if !strings.Contains(string(gotReq.Messages[1].Content), "call1") ||
		!strings.Contains(string(gotReq.Messages[2].Content), "tool_result") {
		t.Fatalf("content: %s | %s", gotReq.Messages[1].Content, gotReq.Messages[2].Content)
	}
}

func TestAnthropicProviderReturnsToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stop_reason":"tool_use","content":[
			{"type":"text","text":"let me check"},
			{"type":"tool_use","id":"tu_1","name":"get_flow","input":{"flow":"i.f1"}}]}`)
	}))
	defer srv.Close()
	p, _ := NewProvider("anthropic", "m", srv.URL, "k", 0)
	reply, err := p.Complete(context.Background(), "", []Entry{{Role: EntryUser, Text: "hi"}}, toolSpecs())
	if err != nil || len(reply.Calls) != 1 || reply.Calls[0].Name != "get_flow" || reply.Calls[0].ID != "tu_1" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	if len(reply.Raw) == 0 { // raw content kept for echo-back
		t.Fatal("raw content not captured")
	}
}

func TestOpenAICompatProvider(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("bad request: %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"",
			"tool_calls":[{"id":"c9","type":"function","function":{"name":"get_flow","arguments":"{\"flow\":\"i.f1\"}"}}]}}]}`)
	}))
	defer srv.Close()

	p, _ := NewProvider("deepseek", "deepseek-chat", srv.URL, "sk", 0)
	reply, err := p.Complete(context.Background(), "sys", historyWithToolRoundTrip(), toolSpecs())
	if err != nil || len(reply.Calls) != 1 || reply.Calls[0].ID != "c9" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	msgs := body["messages"].([]any)
	// system + user + assistant(with tool_calls) + tool result
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	if last := msgs[3].(map[string]any); last["role"] != "tool" || last["tool_call_id"] != "call1" {
		t.Fatalf("tool result msg = %v", last)
	}
}

func TestGeminiProvider(t *testing.T) {
	var body geminiReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":generateContent") || r.URL.Query().Get("key") != "gk" {
			t.Errorf("bad request: %s key=%q", r.URL.Path, r.URL.Query().Get("key"))
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]}}]}`)
	}))
	defer srv.Close()

	p, _ := NewProvider("gemini", "gemini-2.5-pro", srv.URL, "gk", 0)
	reply, err := p.Complete(context.Background(), "sys", historyWithToolRoundTrip(), toolSpecs())
	if err != nil || reply.Text != "done" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	if body.SystemInstruction == nil || len(body.Tools) != 1 {
		t.Fatalf("request = %+v", body)
	}
	// functionCall then functionResponse must be present.
	if len(body.Contents) != 3 || body.Contents[1].Parts[1].FunctionCall == nil ||
		body.Contents[2].Parts[0].FunctionResponse == nil {
		t.Fatalf("contents = %+v", body.Contents)
	}
}

func TestUnknownVendor(t *testing.T) {
	if _, err := NewProvider("nope", "m", "", "k", 0); err == nil {
		t.Fatal("expected error")
	}
}
