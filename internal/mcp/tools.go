package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/elide"
	"reproxy/internal/focus"
	"reproxy/internal/instance"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
	"reproxy/internal/scope"
)

// maxText caps the message text a single get_flow / get_replay_session call
// returns, so a huge body can't flood the model's context; callers page with
// an offset.
const maxText = 16 * 1024

// tools returns the one list both the MCP server and in-app agents use, in a
// stable order (read-only first, then the tools that change traffic).
func tools() []*Tool {
	return []*Tool{
		listInstances,
		getUserFocus,
		listFlows,
		getFlow,
		searchFlows,
		interceptStatus,
		listHeld,
		waitForHeld,
		listReplaySessions,
		getReplaySession,
		listFindingTypes,
		listFindings,
		listEntities,
		getEntity,
		listFiles,
		getFile,

		createFinding,
		setFindingConfidence,
		setFindingConcern,
		addURL,
		setEntityKV,
		exportSourceMap,
		listPluginActions,
		runPluginAction,
		setIntercept,
		forwardHeld,
		dropHeld,
		replayCreate,
		replaySend,
		sendRequest,
	}
}

// Schema helpers.

func object(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "additionalProperties": false}
	if props == nil {
		props = map[string]any{}
	}
	m["properties"] = props
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strField(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intField(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolField(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

const instanceDesc = "The instance to act on; omit for this process's own instance (see list_instances)."

func unmarshal(args json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// Shared output shapes.

type flowOut struct {
	Handle string `json:"handle"`
	instance.FlowSummary
}

func toFlowOut(instanceID string, f instance.FlowSummary) flowOut {
	return flowOut{Handle: flowHandle(instanceID, f.ID), FlowSummary: f}
}

type heldOut struct {
	Handle string `json:"handle"`
	instance.Held
}

// ---- Read-only tools ----

var listInstances = &Tool{
	Name:        "list_instances",
	Description: "List the running reproxy processes you can work with. Each has an id used to build flow and replay handles and to target other tools.",
	Schema:      object(nil),
	run: func(ctx context.Context, r *Registry, _ Caller, _ json.RawMessage) (any, error) {
		return map[string]any{
			"local":     r.router.LocalID(),
			"instances": r.router.Instances(ctx),
		}, nil
	},
}

var getUserFocus = &Tool{
	Name: "get_user_focus",
	Description: "Report what the user had focused in an instance (the flow selected in the request list, " +
		"and the Replay session they last worked in with its unsent edits), to resolve \"this request\", " +
		"\"this response\" or \"what I'm looking at\". Call it whenever the user refers to something " +
		"without naming it. In the reProxy chat it reports the focus at the moment the user sent their " +
		"message, even if they have clicked elsewhere since (changed_since says so); pass now:true for " +
		"what they are looking at right now, or at for another moment. `recent` says which of the two " +
		"they touched last, which is usually the one they mean.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
		"at":       strField("An RFC 3339 time to report the focus at, instead of the time of the user's message. Only recent moments are remembered."),
		"now":      map[string]any{"type": "boolean", "description": "Report the focus right now instead of at the time of the user's message."},
	}),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance string `json:"instance"`
			At       string `json:"at"`
			Now      bool   `json:"now"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		var at time.Time
		switch {
		case a.At != "":
			if at, err = time.Parse(time.RFC3339, a.At); err != nil {
				return nil, fmt.Errorf("at: %w", err)
			}
		case !a.Now:
			at = c.AskedAt
		}
		snap, err := inst.Focus(ctx, at)
		if errors.Is(err, focus.ErrTooOld) {
			return nil, fmt.Errorf("%w; ask the user which request they mean", err)
		}
		if err != nil {
			return nil, err
		}
		asOf := at
		if asOf.IsZero() {
			asOf = time.Now()
		}
		out := map[string]any{"as_of": asOf.UTC().Format(time.RFC3339Nano), "recent": snap.Recent}
		if snap.SelectedFlow != 0 {
			out["selected_flow"] = flowHandle(id, snap.SelectedFlow)
			if f, err := inst.GetFlow(ctx, snap.SelectedFlow); err == nil {
				out["selected_flow_request"] = f.Method + " " + f.URL()
			}
		}
		if snap.ReplaySession != 0 {
			out["replay_session"] = sessionHandle(id, snap.ReplaySession)
			if s, err := inst.ReplayGet(ctx, snap.ReplaySession); err == nil {
				out["replay_request"] = s.Template.Method + " " + s.Template.URL
			}
			out["replay_draft"] = snap.ReplayDraft
		}
		if !at.IsZero() {
			if now, err := inst.Focus(ctx, time.Time{}); err == nil {
				out["changed_since"] = now.SelectedFlow != snap.SelectedFlow ||
					now.ReplaySession != snap.ReplaySession || now.ReplayDraft != snap.ReplayDraft
			}
		}
		return out, nil
	},
}

// flowQueryArgs is the shared argument shape of list_flows and search_flows.
type flowQueryArgs struct {
	Instance string `json:"instance"`
	Filter   string `json:"filter"`
	Query    string `json:"query"`
	Regex    bool   `json:"regex"`
	In       string `json:"in"`
	After    uint64 `json:"after"`
	Limit    int    `json:"limit"`
	Newest   bool   `json:"newest"`
	Entity   string `json:"entity"`
}

func (r *Registry) runQuery(ctx context.Context, a flowQueryArgs, q instance.FlowQuery) (any, error) {
	inst, id, err := r.instanceByID(a.Instance)
	if err != nil {
		return nil, err
	}
	page, err := inst.ListFlows(ctx, q)
	if err != nil {
		return nil, err
	}
	out := struct {
		Instance string    `json:"instance"`
		Flows    []flowOut `json:"flows"`
		Matched  int       `json:"matched"`
		Total    int       `json:"total"`
		More     bool      `json:"more"`
	}{Instance: id, Matched: page.Matched, Total: page.Total, More: page.More}
	for _, f := range page.Flows {
		out.Flows = append(out.Flows, toFlowOut(id, f))
	}
	return out, nil
}

var listFlows = &Tool{
	Name:        "list_flows",
	Description: "List captured flows (request summaries), newest or oldest first, optionally filtered by a substring or regular expression over the method, URL and status. Page with after and limit.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
		"filter":   strField("Keep only flows matching this text."),
		"regex":    boolField("Treat filter as a regular expression."),
		"after":    intField("Skip flows up to this flow ID (for paging); with newest, skip flows at or above it."),
		"limit":    intField("Maximum flows to return (default 50, max 200)."),
		"newest":   boolField("Page from the newest flow backwards."),
		"entity":   strField("Keep only the flows made to this URL, or to any URL under this host or domain, by its key (from list_entities). Local instance only."),
	}),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a flowQueryArgs
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		q := instance.FlowQuery{
			Filter: a.Filter, Regex: a.Regex, In: instance.InSummary,
			After: a.After, Limit: a.Limit, Newest: a.Newest,
		}
		if a.Entity != "" {
			store, err := r.entityStore()
			if err != nil {
				return nil, err
			}
			if _, ok := store.Get(a.Entity); !ok {
				return nil, fmt.Errorf("no entity %q in the Explorer", a.Entity)
			}
			q.IDs = store.FlowIDs(a.Entity)
			if len(q.IDs) == 0 {
				return map[string]any{"instance": r.router.LocalID(), "flows": []any{}, "total": 0, "matched": 0, "more": false}, nil
			}
		}
		return r.runQuery(ctx, a, q)
	},
}

var searchFlows = &Tool{
	Name:        "search_flows",
	Description: "Search flows for text anywhere in the request or response (headers and body), not just the summary. Set in to narrow to headers or body. Page with after and limit.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
		"query":    strField("The text (or regular expression, with regex) to search for."),
		"regex":    boolField("Treat query as a regular expression."),
		"in":       strField("Where to search: \"all\" (default), \"headers\" or \"body\"."),
		"after":    intField("Skip flows up to this flow ID (for paging); with newest, skip flows at or above it."),
		"limit":    intField("Maximum flows to return (default 50, max 200)."),
		"newest":   boolField("Page from the newest flow backwards."),
	}, "query"),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a flowQueryArgs
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		in := a.In
		switch in {
		case instance.InHeaders, instance.InBody, instance.InAll:
		case "":
			in = instance.InAll
		default:
			return nil, fmt.Errorf("in must be \"all\", \"headers\" or \"body\", not %q", a.In)
		}
		return r.runQuery(ctx, a, instance.FlowQuery{
			Filter: a.Query, Regex: a.Regex, In: in,
			After: a.After, Limit: a.Limit, Newest: a.Newest,
		})
	},
}

var getFlow = &Tool{
	Name:        "get_flow",
	Description: "Read one flow's request or response as text. Binary bodies are summarised, not shown. The text is capped; page a long message with offset.",
	Schema: object(map[string]any{
		"flow":   strField("A flow handle from list_flows or search_flows."),
		"part":   strField("Which side to read: \"request\" (default) or \"response\"."),
		"offset": intField("Byte offset into the text to start from (for paging)."),
	}, "flow"),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow   string `json:"flow"`
			Part   string `json:"part"`
			Offset int    `json:"offset"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, flowID, err := parseFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		f, err := inst.GetFlow(ctx, flowID)
		if err != nil {
			return nil, err
		}
		part := a.Part
		if part == "" {
			part = "request"
		}
		var msg capture.Message
		switch part {
		case "request":
			msg = f.Request
		case "response":
			msg = f.Response
		default:
			return nil, fmt.Errorf("part must be \"request\" or \"response\", not %q", a.Part)
		}

		text := capture.DisplayText(msg)
		chunk, next := page(text, a.Offset)
		return struct {
			Handle     string               `json:"handle"`
			Summary    instance.FlowSummary `json:"summary"`
			Part       string               `json:"part"`
			Offset     int                  `json:"offset"`
			Text       string               `json:"text"`
			More       bool                 `json:"more"`
			NextOffset int                  `json:"next_offset,omitempty"`
		}{
			Handle:     a.Flow,
			Summary:    instance.Summarize(f),
			Part:       part,
			Offset:     a.Offset,
			Text:       chunk,
			More:       next > 0,
			NextOffset: next,
		}, nil
	},
}

// page returns the slice of text starting at offset, capped at maxText, and
// the offset to continue from (0 when there is no more).
//
// Offsets are in bytes, since that is what the agent passes back to resume.
// The cut is backed off to the nearest rune boundary so a page never ends
// in a split rune; the bytes it gives up start the next page, so nothing is
// lost.
func page(text string, offset int) (chunk string, next int) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(text) {
		return "", 0
	}
	rest := text[offset:]
	if len(rest) <= maxText {
		return rest, 0
	}
	chunk = elide.Prefix(rest, maxText)
	if chunk == "" { // a rune longer than the whole budget
		return rest, 0
	}
	return chunk, offset + len(chunk)
}

var interceptStatus = &Tool{
	Name:        "intercept_status",
	Description: "Report whether interception is on in an instance and list any messages it is currently holding.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
	}),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance string `json:"instance"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		st, err := inst.Intercept(ctx)
		if err != nil {
			return nil, err
		}
		return interceptOut(id, st), nil
	},
}

func interceptOut(instanceID string, st instance.InterceptState) any {
	out := struct {
		Instance string    `json:"instance"`
		Enabled  bool      `json:"enabled"`
		Held     []heldOut `json:"held"`
	}{Instance: instanceID, Enabled: st.Enabled, Held: []heldOut{}}
	for _, h := range st.Held {
		out.Held = append(out.Held, heldOut{Handle: flowHandle(instanceID, h.FlowID), Held: h})
	}
	return out
}

var listHeld = &Tool{
	Name:        "list_held",
	Description: "List the messages interception is holding in an instance right now, with handles for forward_held and drop_held.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
	}),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance string `json:"instance"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		st, err := inst.Intercept(ctx)
		if err != nil {
			return nil, err
		}
		out := struct {
			Instance string    `json:"instance"`
			Held     []heldOut `json:"held"`
		}{Instance: id, Held: []heldOut{}}
		for _, h := range st.Held {
			out.Held = append(out.Held, heldOut{Handle: flowHandle(id, h.FlowID), Held: h})
		}
		return out, nil
	},
}

var waitForHeld = &Tool{
	Name:        "wait_for_held",
	Description: "Return the oldest held message in an instance, waiting up to timeout_ms for one to be held if none is. ok is false when the wait times out.",
	Schema: object(map[string]any{
		"instance":   strField(instanceDesc),
		"timeout_ms": intField("How long to wait, in milliseconds (default 30000, max 120000)."),
	}),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance  string `json:"instance"`
			TimeoutMs int64  `json:"timeout_ms"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.TimeoutMs <= 0 {
			a.TimeoutMs = 30000
		}
		a.TimeoutMs = min(a.TimeoutMs, 120000)
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		h, ok, err := inst.WaitHeld(ctx, time.Duration(a.TimeoutMs)*time.Millisecond)
		if err != nil {
			return nil, err
		}
		out := struct {
			Instance string   `json:"instance"`
			OK       bool     `json:"ok"`
			Held     *heldOut `json:"held,omitempty"`
		}{Instance: id, OK: ok}
		if ok {
			out.Held = &heldOut{Handle: flowHandle(id, h.FlowID), Held: h}
		}
		return out, nil
	},
}

type templateOut struct {
	FlowHandle string `json:"flow_handle,omitempty"`
	Addr       string `json:"addr"`
	Secure     bool   `json:"secure"`
	Host       string `json:"host"`
	Method     string `json:"method"`
	URL        string `json:"url"`
}

func toTemplateOut(instanceID string, t replay.Template) templateOut {
	out := templateOut{Addr: t.Addr, Secure: t.Secure, Host: t.Host, Method: t.Method, URL: t.URL}
	if t.FlowID != 0 {
		out.FlowHandle = flowHandle(instanceID, t.FlowID)
	}
	return out
}

var listReplaySessions = &Tool{
	Name:        "list_replay_sessions",
	Description: "List the Replay sessions in an instance, both the user's and agents', with handles for get_replay_session and replay_send.",
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
	}),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance string `json:"instance"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		sessions, err := inst.ReplayList(ctx)
		if err != nil {
			return nil, err
		}
		type sessionSummary struct {
			Handle   string      `json:"handle"`
			Owner    string      `json:"owner"`
			Created  time.Time   `json:"created"`
			Template templateOut `json:"template"`
		}
		out := struct {
			Instance string           `json:"instance"`
			Sessions []sessionSummary `json:"sessions"`
		}{Instance: id, Sessions: []sessionSummary{}}
		for _, s := range sessions {
			out.Sessions = append(out.Sessions, sessionSummary{
				Handle:   sessionHandle(id, s.ID),
				Owner:    string(s.Owner),
				Created:  s.Created,
				Template: toTemplateOut(id, s.Template),
			})
		}
		return out, nil
	},
}

var getReplaySession = &Tool{
	Name:        "get_replay_session",
	Description: "Read one Replay session: its template and the history of sends, each with the request and response as text (capped).",
	Schema: object(map[string]any{
		"session": strField("A replay session handle from list_replay_sessions."),
	}, "session"),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Session string `json:"session"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, sessionID, err := parseSession(a.Session)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		s, err := inst.ReplayGet(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		type entryOut struct {
			Seq      int       `json:"seq"`
			At       time.Time `json:"at"`
			By       string    `json:"by"`
			Request  string    `json:"request"`
			Response string    `json:"response,omitempty"`
			Error    string    `json:"error,omitempty"`
		}
		out := struct {
			Handle   string      `json:"handle"`
			Owner    string      `json:"owner"`
			Created  time.Time   `json:"created"`
			Template templateOut `json:"template"`
			History  []entryOut  `json:"history"`
		}{
			Handle:   a.Session,
			Owner:    string(s.Owner),
			Created:  s.Created,
			Template: toTemplateOut(id, s.Template),
			History:  []entryOut{},
		}
		for _, e := range s.History {
			eo := entryOut{
				Seq:     e.Seq,
				At:      e.At,
				By:      string(e.By),
				Request: capped(capture.DisplayText(capture.Message{Raw: e.Request, Size: int64(len(e.Request))})),
				Error:   e.Err,
			}
			if e.Err == "" {
				eo.Response = capped(capture.DisplayText(capture.Message{Raw: e.Response.Raw, Size: int64(len(e.Response.Raw))}))
			}
			out.History = append(out.History, eo)
		}
		return out, nil
	},
}

// capped truncates text to maxText, marking where it was cut. maxText is
// a byte budget — it bounds what one call hands the model — and the cut is
// backed off to a rune boundary: a body is arbitrary captured traffic, and
// a split rune is text the model cannot parse.
func capped(text string) string {
	return elide.Bytes(text, maxText, "\n‹truncated›")
}

// ---- Tools that change traffic ----

var setIntercept = &Tool{
	Name:        "set_intercept",
	Description: "Turn interception on or off in an instance. With it on, matching messages are held for forward_held or drop_held.",
	Mutates:     true,
	Schema: object(map[string]any{
		"instance": strField(instanceDesc),
		"enabled":  boolField("True to turn interception on, false to turn it off."),
	}, "enabled"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Instance string `json:"instance"`
			Enabled  bool   `json:"enabled"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		inst, id, err := r.instanceByID(a.Instance)
		if err != nil {
			return nil, err
		}
		verb := "Turn interception off"
		if a.Enabled {
			verb = "Turn interception on"
		}
		if err := r.gate(ctx, c, "set_intercept", fmt.Sprintf("%s in instance %s", verb, id)); err != nil {
			return nil, err
		}
		if err := inst.SetIntercept(ctx, a.Enabled); err != nil {
			return nil, err
		}
		return map[string]any{"instance": id, "enabled": a.Enabled}, nil
	},
}

var forwardHeld = &Tool{
	Name:        "forward_held",
	Description: "Forward a held message, optionally replacing it with edited text (the whole message: start line, headers, a blank line, then the body). Editing a request may not move it outside the original request's domain.",
	Mutates:     true,
	Schema: object(map[string]any{
		"flow":   strField("The held flow's handle (from list_held or wait_for_held)."),
		"edited": strField("Replacement message text; omit to forward it unchanged."),
	}, "flow"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow   string  `json:"flow"`
			Edited *string `json:"edited"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, flowID, err := parseFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		held, err := heldOf(ctx, inst, flowID)
		if err != nil {
			return nil, err
		}
		// Only a held request can be redirected out of scope by an edit; a
		// response goes back to the same client regardless.
		if a.Edited != nil && held.Direction == "request" {
			f, err := inst.GetFlow(ctx, flowID)
			if err != nil {
				return nil, err
			}
			if err := checkHostHeader(f.Host, hostHeader([]byte(*a.Edited))); err != nil {
				return nil, err
			}
		}
		summary := fmt.Sprintf("Forward the held %s %s %s", held.Direction, held.Method, held.URL)
		if a.Edited != nil {
			summary += " (edited)"
		}
		if err := r.gate(ctx, c, "forward_held", summary); err != nil {
			return nil, err
		}
		if err := inst.Forward(ctx, flowID, a.Edited); err != nil {
			return nil, err
		}
		return map[string]any{"forwarded": a.Flow, "edited": a.Edited != nil}, nil
	},
}

var dropHeld = &Tool{
	Name:        "drop_held",
	Description: "Drop a held message, discarding it and closing the browser's connection for that exchange.",
	Mutates:     true,
	Schema: object(map[string]any{
		"flow": strField("The held flow's handle (from list_held or wait_for_held)."),
	}, "flow"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow string `json:"flow"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, flowID, err := parseFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		held, err := heldOf(ctx, inst, flowID)
		if err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("Drop the held %s %s %s", held.Direction, held.Method, held.URL)
		if err := r.gate(ctx, c, "drop_held", summary); err != nil {
			return nil, err
		}
		if err := inst.Drop(ctx, flowID); err != nil {
			return nil, err
		}
		return map[string]any{"dropped": a.Flow}, nil
	},
}

var replayCreate = &Tool{
	Name:        "replay_create",
	Description: "Start a Replay session from a captured flow, to resend and vary its request. An optional target (host:port) overrides where it connects, and must stay within the original request's domain.",
	Mutates:     true,
	Schema: object(map[string]any{
		"flow":   strField("The flow to base the session on (a handle from list_flows)."),
		"target": strField("Optional host:port to connect to instead of the original; must be in the same domain."),
	}, "flow"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow   string `json:"flow"`
			Target string `json:"target"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, flowID, err := parseFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		f, err := inst.GetFlow(ctx, flowID)
		if err != nil {
			return nil, err
		}
		if a.Target != "" {
			if err := scope.Check(f.Host, a.Target, ""); err != nil {
				return nil, err
			}
		}
		summary := fmt.Sprintf("Create a Replay session from %s %s", f.Method, f.URL())
		if a.Target != "" {
			summary += " targeting " + a.Target
		}
		if err := r.gate(ctx, c, "replay_create", summary); err != nil {
			return nil, err
		}
		s, err := inst.ReplayCreate(ctx, c.owner(), flowID, a.Target)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"session":  sessionHandle(id, s.ID),
			"template": toTemplateOut(id, s.Template),
		}, nil
	},
}

var replaySend = &Tool{
	Name:        "replay_send",
	Description: "Send a request in a Replay session. request is the full raw request text; it is normalised against the session's template before sending and may not move the Host header outside the original request's domain. The response is returned.",
	Mutates:     true,
	Schema: object(map[string]any{
		"session": strField("The replay session handle (from replay_create or list_replay_sessions)."),
		"request": strField("The full raw request to send (start line, headers, a blank line, then the body)."),
	}, "session", "request"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Session string `json:"session"`
			Request string `json:"request"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, sessionID, err := parseSession(a.Session)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		s, err := inst.ReplayGet(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		raw := intercept.ApplyEdit(s.Template.Raw, a.Request, true)
		if err := checkHostHeader(s.Template.Host, hostHeader(raw)); err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("Send a request to %s in Replay session %s", s.Template.Host, a.Session)
		if err := r.gate(ctx, c, "replay_send", summary); err != nil {
			return nil, err
		}
		e, err := inst.ReplaySend(ctx, sessionID, raw, c.owner())
		if err != nil {
			return nil, err
		}
		return sendResult(a.Session, e), nil
	},
}

var sendRequest = &Tool{
	Name:        "send_request",
	Description: "Create a Replay session from a flow and send one request in a single step. request is the full raw request text; an optional target overrides the connection. Both the target and the Host header must stay within the original request's domain.",
	Mutates:     true,
	Schema: object(map[string]any{
		"flow":    strField("The flow to base the request on (a handle from list_flows)."),
		"request": strField("The full raw request to send (start line, headers, a blank line, then the body)."),
		"target":  strField("Optional host:port to connect to instead of the original; must be in the same domain."),
	}, "flow", "request"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow    string `json:"flow"`
			Request string `json:"request"`
			Target  string `json:"target"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		id, flowID, err := parseFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		inst, _, err := r.instanceByID(id)
		if err != nil {
			return nil, err
		}
		f, err := inst.GetFlow(ctx, flowID)
		if err != nil {
			return nil, err
		}
		target := a.Target
		if target == "" {
			target = f.Target
		}
		raw := intercept.ApplyEdit(f.Request.Raw, a.Request, true)
		if err := scope.Check(f.Host, target, hostHeader(raw)); err != nil {
			return nil, err
		}
		summary := fmt.Sprintf("Send a %s request to %s (from %s)", f.Method, f.Host, f.URL())
		if err := r.gate(ctx, c, "send_request", summary); err != nil {
			return nil, err
		}
		s, err := inst.ReplayCreate(ctx, c.owner(), flowID, a.Target)
		if err != nil {
			return nil, err
		}
		e, err := inst.ReplaySend(ctx, s.ID, raw, c.owner())
		if err != nil {
			return nil, err
		}
		return sendResult(sessionHandle(id, s.ID), e), nil
	},
}

func sendResult(session string, e replay.Entry) any {
	out := struct {
		Session  string `json:"session"`
		Seq      int    `json:"seq"`
		Request  string `json:"request"`
		Response string `json:"response,omitempty"`
		Error    string `json:"error,omitempty"`
	}{
		Session: session,
		Seq:     e.Seq,
		Request: capped(capture.DisplayText(capture.Message{Raw: e.Request, Size: int64(len(e.Request))})),
		Error:   e.Err,
	}
	if e.Err == "" {
		out.Response = capped(capture.DisplayText(capture.Message{Raw: e.Response.Raw, Size: int64(len(e.Response.Raw))}))
	}
	return out
}

// checkHostHeader verifies an outgoing Host header stays within origin's
// domain, leaving the connection target (already fixed) out of it.
func checkHostHeader(origin, host string) error {
	return scope.Check(origin, origin, host)
}

// hostHeader returns the value of the Host header in a raw request, or "".
func hostHeader(raw []byte) string {
	s := string(raw)
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		s = s[:i]
	} else if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Host") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// heldOf returns the message an instance is currently holding for flowID, or
// ErrNotHeld if none is.
func heldOf(ctx context.Context, inst instance.Instance, flowID uint64) (instance.Held, error) {
	st, err := inst.Intercept(ctx)
	if err != nil {
		return instance.Held{}, err
	}
	for _, h := range st.Held {
		if h.FlowID == flowID {
			return h, nil
		}
	}
	return instance.Held{}, instance.ErrNotHeld
}
