package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// openAIProvider talks to any OpenAI-compatible chat-completions endpoint
// (POST {baseURL}/chat/completions): Ollama, DeepSeek, Qwen (DashScope),
// Kimi (Moonshot) and the like. baseURL already includes the version prefix
// the vendor uses (e.g. ".../v1"), from the vendor catalogue.
type openAIProvider struct {
	name      string
	model     string
	baseURL   string
	key       string
	maxTokens int
	http      *http.Client
}

func (p *openAIProvider) Name() string { return p.name }

type openAIReq struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded string
	} `json:"function"`
}

type openAIResp struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *openAIProvider) Complete(ctx context.Context, system string, history []Entry, tools []ToolSpec) (Reply, error) {
	req := openAIReq{Model: p.model, MaxTokens: p.maxTokens}
	if system != "" {
		req.Messages = append(req.Messages, openAIMessage{Role: "system", Content: system})
	}
	req.Messages = append(req.Messages, p.messages(history)...)
	for _, t := range tools {
		req.Tools = append(req.Tools, openAITool{
			Type:     "function",
			Function: openAIToolFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema},
		})
	}

	headers := map[string]string{}
	if p.key != "" {
		headers["Authorization"] = "Bearer " + p.key
	}
	var resp openAIResp
	if err := postJSON(ctx, p.http, strings.TrimRight(p.baseURL, "/")+"/chat/completions", headers, req, &resp); err != nil {
		return Reply{}, err
	}
	if resp.Error != nil {
		return Reply{}, errorf(resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return Reply{}, errorf("the model returned no choices")
	}

	msg := resp.Choices[0].Message
	reply := Reply{Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		reply.Calls = append(reply.Calls, ToolInvocation{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(orEmptyObject(tc.Function.Arguments)),
		})
	}
	return reply, nil
}

func (p *openAIProvider) messages(history []Entry) []openAIMessage {
	var out []openAIMessage
	for _, e := range history {
		switch e.Role {
		case EntryAssistant:
			m := openAIMessage{Role: "assistant", Content: e.Text}
			for _, c := range e.Calls {
				var call openAIToolCall
				call.ID, call.Type = c.ID, "function"
				call.Function.Name = c.Name
				call.Function.Arguments = string(c.Input)
				m.ToolCalls = append(m.ToolCalls, call)
			}
			out = append(out, m)
		case EntryUser:
			if len(e.Results) > 0 {
				for _, r := range e.Results {
					out = append(out, openAIMessage{Role: "tool", ToolCallID: r.ID, Content: r.Content})
				}
				continue
			}
			out = append(out, openAIMessage{Role: "user", Content: e.Text})
		}
	}
	return out
}

func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
