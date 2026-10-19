package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// anthropicProvider talks to the Anthropic Messages API
// (POST {baseURL}/v1/messages). Assistant content (text, thinking and
// tool_use blocks) is round-tripped verbatim through Entry.Raw, so thinking
// blocks are echoed back unchanged and preserved-thinking checks pass.
type anthropicProvider struct {
	model     string
	baseURL   string
	key       string
	maxTokens int
	http      *http.Client
}

func (p *anthropicProvider) Name() string { return "Anthropic" }

const anthropicVersion = "2023-06-01"

type anthropicReq struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicResp struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Error      *anthropicError  `json:"error"`
}

type anthropicBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (p *anthropicProvider) Complete(ctx context.Context, system string, history []Entry, tools []ToolSpec) (Reply, error) {
	req := anthropicReq{
		Model:     p.model,
		MaxTokens: p.maxTokens,
		System:    system,
		Messages:  p.messages(history),
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}

	var resp anthropicResp
	headers := map[string]string{
		"x-api-key":         p.key,
		"anthropic-version": anthropicVersion,
	}
	if err := postJSON(ctx, p.http, strings.TrimRight(p.baseURL, "/")+"/v1/messages", headers, req, &resp); err != nil {
		return Reply{}, err
	}
	if resp.Error != nil {
		return Reply{}, errorf(resp.Error.Message)
	}

	var reply Reply
	var texts []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			reply.Calls = append(reply.Calls, ToolInvocation{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	reply.Text = strings.Join(texts, "\n")
	// Keep the exact assistant content to echo back next turn.
	if raw, err := json.Marshal(resp.Content); err == nil {
		reply.Raw = raw
	}
	return reply, nil
}

// messages builds the Anthropic message list from neutral history.
func (p *anthropicProvider) messages(history []Entry) []anthropicMessage {
	var out []anthropicMessage
	for _, e := range history {
		switch e.Role {
		case EntryAssistant:
			// Prefer the provider's own exact content (preserves thinking and
			// tool_use blocks); fall back to rebuilding from Text + Calls.
			if len(e.Raw) > 0 {
				out = append(out, anthropicMessage{Role: "assistant", Content: e.Raw})
				continue
			}
			var blocks []anthropicBlock
			if e.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: e.Text})
			}
			for _, c := range e.Calls {
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Input})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: mustJSON(blocks)})
		case EntryUser:
			if len(e.Results) > 0 {
				var blocks []map[string]any
				for _, r := range e.Results {
					blocks = append(blocks, map[string]any{
						"type":        "tool_result",
						"tool_use_id": r.ID,
						"content":     r.Content,
						"is_error":    r.IsError,
					})
				}
				out = append(out, anthropicMessage{Role: "user", Content: mustJSON(blocks)})
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: mustJSON([]anthropicBlock{{Type: "text", Text: e.Text}})})
		}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
