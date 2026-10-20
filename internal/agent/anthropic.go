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
//
// Every request is marked for prompt caching. A turn is a loop of requests
// that each repeat the one before and add a little, so the repeated part —
// the tools, the system prompt and the conversation so far — is read from
// the cache at a fraction of the price instead of being paid for again.
// Two breakpoints do it: one after the system prompt (which caches the
// tools ahead of it too), and one on the newest message, whose cached
// prefix the next request finds by looking back from its own. A prompt too
// short to cache is simply not cached; nothing else changes.
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
	System    []anthropicText    `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

// anthropicText is a text block of the system prompt.
type anthropicText struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl marks where a cached prefix of the prompt ends.
type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
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
		Messages:  p.messages(history),
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	// The tools come first in the prompt, then the system prompt: a
	// breakpoint after the system prompt caches both, and with no system
	// prompt the last tool takes it.
	if system != "" {
		req.System = []anthropicText{{Type: "text", Text: system, CacheControl: ephemeral}}
	} else if n := len(req.Tools); n > 0 {
		req.Tools[n-1].CacheControl = ephemeral
	}
	if n := len(req.Messages); n > 0 {
		req.Messages[n-1].Content = cacheLastBlock(req.Messages[n-1].Content)
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

// cacheLastBlock puts a cache breakpoint on the last block of a message's
// content. Content it can't mark is returned as it was: a block the API
// doesn't let carry one (thinking), or content that isn't a list of blocks.
func cacheLastBlock(content json.RawMessage) json.RawMessage {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil || len(blocks) == 0 {
		return content
	}
	last := blocks[len(blocks)-1]
	var kind string
	json.Unmarshal(last["type"], &kind)
	if kind == "thinking" || kind == "redacted_thinking" {
		return content
	}
	last["cache_control"] = mustJSON(ephemeral)
	return mustJSON(blocks)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
