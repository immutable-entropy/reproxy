package agent

import (
	"context"
	"encoding/json"
)

// A Provider is one LLM backend. It performs a single model turn: given the
// conversation and the tools available, it returns the model's reply (text
// and any tool calls). The Runner executes the tool calls and calls Complete
// again with their results appended, until the model is done.
//
// Providers translate the neutral types below into their own wire format.
// They are hand-rolled over net/http so one interface spans every vendor and
// each is testable with an httptest fake.
type Provider interface {
	// Name is the vendor label, for errors and logs.
	Name() string
	// Complete runs one turn. system is the system prompt; history is the
	// conversation so far; tools is what the model may call.
	Complete(ctx context.Context, system string, history []Entry, tools []ToolSpec) (Reply, error)
}

// Role of a conversation entry.
const (
	EntryUser      = "user"
	EntryAssistant = "assistant"
)

// Entry is one conversation turn in provider-neutral form.
type Entry struct {
	Role string
	// Text is the user's message or the assistant's visible text.
	Text string
	// Calls are tool calls the assistant made (assistant entries).
	Calls []ToolInvocation
	// Results are tool results being returned to the model (user entries),
	// paired to Calls by ID.
	Results []ToolOutcome
	// Raw lets a provider round-trip its own exact assistant content (e.g.
	// Anthropic thinking + tool_use blocks, preserved verbatim so
	// preserved-thinking checks pass). Opaque to everyone else.
	Raw json.RawMessage
}

// ToolSpec is a tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolInvocation is one tool call the model made.
type ToolInvocation struct {
	ID    string          // provider's call ID, echoed in the result
	Name  string          // tool name
	Input json.RawMessage // arguments as JSON
}

// ToolOutcome is the result of running a tool call.
type ToolOutcome struct {
	ID      string
	Content string
	IsError bool
}

// Reply is what a Provider returns from one turn.
type Reply struct {
	// Text is the assistant's visible text, if any.
	Text string
	// Calls are tool calls to execute; empty means the model is done.
	Calls []ToolInvocation
	// Raw is the provider's exact assistant content for this turn, stored on
	// the assistant Entry and echoed back next turn. May be nil.
	Raw json.RawMessage
}

// NewProvider builds the provider for a configured agent. key is the API key
// (ignored by providers that need none). model and baseURL come from the
// profile; empty baseURL uses the vendor default.
func NewProvider(vendor, model, baseURL, key string, maxTokens int) (Provider, error) {
	v, ok := VendorByID(vendor)
	if !ok {
		return nil, errUnknownVendor(vendor)
	}
	if baseURL == "" {
		baseURL = v.DefaultBaseURL
	}
	if model == "" {
		model = v.DefaultModel
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	switch v.API {
	case APIAnthropic:
		return &anthropicProvider{model: model, baseURL: baseURL, key: key, maxTokens: maxTokens, http: defaultHTTP()}, nil
	case APIGemini:
		return &geminiProvider{model: model, baseURL: baseURL, key: key, maxTokens: maxTokens, http: defaultHTTP()}, nil
	case APIOpenAI:
		return &openAIProvider{name: v.Name, model: model, baseURL: baseURL, key: key, maxTokens: maxTokens, http: defaultHTTP()}, nil
	}
	return nil, errUnknownVendor(vendor)
}
