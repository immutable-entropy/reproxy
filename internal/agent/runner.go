package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"reproxy/internal/elide"
)

// ToolBox is the set of tools the agent can call. In production it wraps the
// MCP tool registry; a session's mode decides whether a mutating tool asks
// the user first (the registry's approval gate handles that, blocking until
// the user decides).
type ToolBox interface {
	// Specs lists the tools offered to the model.
	Specs() []ToolSpec
	// Invoke runs a tool for a turn and returns the result as text for the
	// model. isError marks a tool-level failure (the model sees it and can
	// adjust). A returned error is an infrastructure failure (or a
	// cancelled context) that should end the turn.
	Invoke(ctx context.Context, turn Turn, name string, args json.RawMessage) (content string, isError bool, err error)
}

// Turn is what a tool call knows about the turn it is made in.
type Turn struct {
	Session string
	Mode    Mode
	// AskedAt is when the user sent the message the turn answers, so a tool
	// resolving "this request" (get_user_focus) can report what was focused
	// then, not wherever the user has clicked since.
	AskedAt time.Time
}

// Providers builds the LLM client for a configured agent profile.
type Providers interface {
	For(profileID string) (Provider, error)
}

// maxSteps bounds the tool-call loop in a single turn, so a confused model
// can't loop forever.
const maxSteps = 24

// LoopRunner drives one provider through the tool loop. It implements
// manager.Runner.
type LoopRunner struct {
	providers Providers
	tools     ToolBox
}

// NewLoopRunner returns a LoopRunner.
func NewLoopRunner(providers Providers, tools ToolBox) *LoopRunner {
	return &LoopRunner{providers: providers, tools: tools}
}

// Run implements the agent loop for one user turn.
func (r *LoopRunner) Run(ctx context.Context, s Session, emit func(Message)) ([]Entry, error) {
	provider, err := r.providers.For(s.ProfileID)
	if err != nil {
		return nil, err
	}
	system := systemPrompt
	if s.Instructions != "" {
		system += "\n" + s.Instructions + "\n"
	}
	history := append([]Entry(nil), s.Convo...)
	turn := Turn{Session: s.ID, Mode: s.Mode, AskedAt: askedAt(s)}
	specs := r.tools.Specs()

	for step := 0; step < maxSteps; step++ {
		reply, err := provider.Complete(ctx, system, history, specs)
		if err != nil {
			return history, err
		}
		if reply.Text != "" {
			emit(Message{Role: RoleAssistant, Text: reply.Text})
		}
		history = append(history, Entry{Role: EntryAssistant, Text: reply.Text, Calls: reply.Calls, Raw: reply.Raw})

		if len(reply.Calls) == 0 {
			return history, nil // model is done
		}

		results := make([]ToolOutcome, 0, len(reply.Calls))
		for _, call := range reply.Calls {
			content, isErr, err := r.tools.Invoke(ctx, turn, call.Name, call.Input)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return history, err
				}
				// An approval denial or infra error is reported to the model
				// as a tool error so it can adjust rather than crash the turn.
				content, isErr = err.Error(), true
			}
			emit(Message{Role: RoleTool, Tool: &ToolCall{
				Name:          call.Name,
				Input:         compactJSON(call.Input),
				Output:        truncate(content, 2000),
				IsError:       isErr,
				ReplaySession: replayHandle(call.Name, content),
			}})
			results = append(results, ToolOutcome{ID: call.ID, Content: content, IsError: isErr})
		}
		history = append(history, Entry{Role: EntryUser, Results: results})

		if err := ctx.Err(); err != nil {
			return history, err
		}
	}
	emit(Message{Role: RoleSystem, Text: "Reached the step limit for this turn; stopping."})
	return history, nil
}

// systemPrompt tells the model what it is and the rules it works under. It
// is the same on every turn: what the user is looking at is fetched only
// when the model asks (get_user_focus), which keeps it out of turns that
// don't need it and leaves the prompt cacheable.
const systemPrompt = "You are an AI assistant built into reProxy, an intercepting HTTP proxy used for QA and " +
	"security testing of the team's own pre-production web apps. You help the user review, " +
	"intercept, edit, replay and fuzz the traffic flowing through the proxy, to find flaws " +
	"before the apps ship.\n\n" +
	"Work through the MCP tools. Instances, flows and replay sessions are named by opaque " +
	"handles the tools return — pass them back verbatim. Read traffic freely; when you send " +
	"or change traffic the tool may ask the user to approve it first.\n\n" +
	"Scope: you may only send requests to the same registrable domain each flow originally " +
	"targeted — this is enforced, and a send outside it is refused. To test a different host, " +
	"start from a flow captured on that host.\n\n" +
	"When the user refers to something without naming it (\"this request\", \"this response\", " +
	"\"what I'm looking at\"), call get_user_focus: it reports what they had selected, and the " +
	"Replay window they were working in with its unsent edits, at the moment they sent the " +
	"message. Don't guess from the flow list.\n\n" +
	"Be concise.\n"

// askedAt is when the user sent the message this turn answers: the last
// user message in the transcript.
func askedAt(s Session) time.Time {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == RoleUser {
			return s.Messages[i].At
		}
	}
	return time.Time{}
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return truncate(string(raw), 500)
	}
	return truncate(buf.String(), 500)
}

// truncate shortens a tool result for the model, saying how much was left
// out. max is a byte budget, cut back to a rune boundary: tool results
// carry captured traffic, and a split rune is text the model cannot read.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return elide.Bytes(s, max, fmt.Sprintf("… (%d bytes total)", len(s)))
}

// replayHandle pulls a replay-session handle out of a tool result, so the UI
// can offer "Open in Replay" on a card. The replay tools return JSON with a
// "session" field holding the handle.
func replayHandle(tool, content string) string {
	if !strings.HasPrefix(tool, "replay_") && tool != "send_request" {
		return ""
	}
	var m struct {
		Session string `json:"session"`
	}
	if json.Unmarshal([]byte(content), &m) == nil {
		return m.Session
	}
	return ""
}
