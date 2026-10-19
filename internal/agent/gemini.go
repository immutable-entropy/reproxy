package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// geminiProvider talks to the Google Gemini generateContent API
// (POST {baseURL}/v1beta/models/{model}:generateContent?key=...). Tool calls
// are Gemini "functionCall" parts; results are "functionResponse" parts.
type geminiProvider struct {
	model     string
	baseURL   string
	key       string
	maxTokens int
	http      *http.Client
}

func (p *geminiProvider) Name() string { return "Google Gemini" }

type geminiReq struct {
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	Tools             []geminiTool           `json:"tools,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiResp struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *geminiProvider) Complete(ctx context.Context, system string, history []Entry, tools []ToolSpec) (Reply, error) {
	req := geminiReq{GenerationConfig: geminiGenerationConfig{MaxOutputTokens: p.maxTokens}}
	if system != "" {
		req.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: system}}}
	}
	req.Contents = p.contents(history)
	if len(tools) > 0 {
		decls := make([]geminiFunctionDecl, 0, len(tools))
		for _, t := range tools {
			decls = append(decls, geminiFunctionDecl{Name: t.Name, Description: t.Description, Parameters: cleanSchema(t.Schema)})
		}
		req.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}

	// Gemini authenticates with the key as a query parameter.
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s",
		strings.TrimRight(p.baseURL, "/"), p.model, p.key)
	var resp geminiResp
	if err := postJSON(ctx, p.http, url, nil, req, &resp); err != nil {
		return Reply{}, err
	}
	if resp.Error != nil {
		return Reply{}, errorf(resp.Error.Message)
	}
	if len(resp.Candidates) == 0 {
		return Reply{}, errorf("the model returned no candidates")
	}

	var reply Reply
	var texts []string
	n := 0
	for _, part := range resp.Candidates[0].Content.Parts {
		switch {
		case part.FunctionCall != nil:
			n++
			reply.Calls = append(reply.Calls, ToolInvocation{
				// Gemini has no call ID; synthesise a stable one for pairing.
				ID:    fmt.Sprintf("%s-%d", part.FunctionCall.Name, n),
				Name:  part.FunctionCall.Name,
				Input: json.RawMessage(orEmptyObject(string(part.FunctionCall.Args))),
			})
		case part.Text != "":
			texts = append(texts, part.Text)
		}
	}
	reply.Text = strings.Join(texts, "\n")
	return reply, nil
}

func (p *geminiProvider) contents(history []Entry) []geminiContent {
	var out []geminiContent
	for _, e := range history {
		switch e.Role {
		case EntryAssistant:
			var parts []geminiPart
			if e.Text != "" {
				parts = append(parts, geminiPart{Text: e.Text})
			}
			for _, c := range e.Calls {
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: c.Name, Args: c.Input}})
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})
		case EntryUser:
			if len(e.Results) > 0 {
				var parts []geminiPart
				for _, r := range e.Results {
					parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{
						Name:     toolNameFromID(r.ID),
						Response: map[string]any{"result": r.Content},
					}})
				}
				out = append(out, geminiContent{Role: "user", Parts: parts})
				continue
			}
			out = append(out, geminiContent{Role: "user", Parts: []geminiPart{{Text: e.Text}}})
		}
	}
	return out
}

// toolNameFromID recovers the function name from a synthesised Gemini call ID
// ("<name>-<n>").
func toolNameFromID(id string) string {
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		return id[:i]
	}
	return id
}

// cleanSchema drops JSON Schema keywords Gemini's function declarations
// reject (it accepts a restricted subset). Unknown keys are passed through;
// only the few that commonly break are removed.
func cleanSchema(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		switch k {
		case "additionalProperties", "$schema", "$id", "$ref", "definitions", "$defs":
			continue
		}
		if m, ok := v.(map[string]any); ok {
			out[k] = cleanSchema(m)
			continue
		}
		if props, ok := v.(map[string]map[string]any); ok {
			np := make(map[string]any, len(props))
			for pk, pv := range props {
				np[pk] = cleanSchema(pv)
			}
			out[k] = np
			continue
		}
		out[k] = v
	}
	return out
}
