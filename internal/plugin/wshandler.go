package plugin

import (
	"context"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/websocket"
)

// wsHandler is the proxy.WSHandler the manager installs: it decides whether a
// WebSocket connection should be editable (so the proxy strips its
// compression), runs the websocket.on_message_pending hooks on each message
// of an editable connection, and emits websocket.on_open / on_close.
//
// websocket.on_message is delivered separately, through the Dispatcher's
// OnWSMessage subscriber on the capture store, so every recorded message
// reaches observers whether or not the connection is editable.
type wsHandler struct {
	d     *Dispatcher
	store *capture.Store
}

// Edits reports whether some enabled plugin wants to edit this connection's
// messages: an on_message_pending target with hold: true whose filter
// matches the handshake. Only then does the proxy strip compression.
func (h *wsHandler) Edits(s intercept.Subject) bool {
	rt := h.d.routes.Load()
	if rt == nil || len(rt.holdWS) == 0 {
		return false
	}
	origin, _ := OriginOf(s.Origin)
	facts := h.d.wsFacts(subjectFlow(s), "")
	for i := range rt.holdWS {
		if origin != rt.holdWS[i].p.src.Name && rt.holdWS[i].m.Match(&facts) {
			return true
		}
	}
	return false
}

// Decide runs the on_message_pending hooks in priority order and returns what
// to do with the message. Each may pass, edit (the next sees the edit),
// forward or drop. A timeout or error is a pass.
func (h *wsHandler) Decide(ctx context.Context, flowID uint64, dir websocket.Direction, typ websocket.Type, data []byte) websocket.Decision {
	rt := h.d.routes.Load()
	if rt == nil || len(rt.holdWS) == 0 {
		return websocket.Decision{}
	}
	f, ok := h.store.Get(flowID)
	if !ok {
		return websocket.Decision{}
	}
	origin, _ := OriginOf(f.Origin)
	facts := h.d.wsFacts(&f, dir.String())

	current := data
	var edited bool
	for i := range rt.holdWS {
		r := &rt.holdWS[i]
		if origin == r.p.src.Name || !r.m.Match(&facts) {
			continue
		}
		budget := time.Duration(r.millis) * time.Millisecond
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < budget {
			budget = time.Until(dl)
		}
		if budget <= 0 {
			break
		}
		msg := current
		answer, err := r.p.await(manifest.WebSocketOnMessagePending, r.fn, 0, budget, func(vm *goja.Runtime) goja.Value {
			o := vm.NewObject()
			o.Set("hook", string(manifest.WebSocketOnMessagePending))
			o.Set("flow", flowValue(vm, &f))
			o.Set("message", wsMessageValue(vm, &f, capture.WSMessage{
				Dir: capture.WSDirection(dir), Type: capture.WSType(typ), Data: msg,
			}))
			return o
		})
		if err != nil || ctx.Err() != nil {
			continue
		}
		action, text, bin, err := readWSDecision(answer)
		if err != nil {
			r.p.fail(manifest.WebSocketOnMessagePending, r.fn, err)
			continue
		}
		switch action {
		case "edit":
			if bin != nil {
				current = bin
			} else {
				current = []byte(text)
			}
			edited = true
		case "forward":
			if bin != nil {
				current, edited = bin, true
			} else if text != "" {
				current, edited = []byte(text), true
			}
			return decisionFor(current, edited)
		case "drop":
			return websocket.Decision{Drop: true}
		}
	}
	return decisionFor(current, edited)
}

func (h *wsHandler) Opened(flowID uint64, _ intercept.Subject) {
	h.emitConn(flowID, manifest.WebSocketOnOpen)
}

func (h *wsHandler) Closed(flowID uint64) {
	h.emitConn(flowID, manifest.WebSocketOnClose)
}

func (h *wsHandler) emitConn(flowID uint64, hook manifest.Hook) {
	rt := h.d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return
	}
	f, ok := h.store.Get(flowID)
	if !ok {
		return
	}
	origin, _ := OriginOf(f.Origin)
	facts := h.d.wsFacts(&f, "")
	for _, r := range rt.byHook[hookIndex[hook]] {
		if origin == r.p.src.Name && !r.self {
			continue
		}
		if r.m.Match(&facts) {
			flow := f
			r.p.observe(hook, r.fn, 0, func(vm *goja.Runtime) goja.Value {
				return wsConnValue(vm, hook, &flow)
			})
		}
	}
}

func decisionFor(data []byte, edited bool) websocket.Decision {
	if edited {
		return websocket.Decision{Data: data}
	}
	return websocket.Decision{}
}

// subjectFlow turns an intercept.Subject back into a capture.Flow, enough for
// the WebSocket match facts (the handshake has no direction).
func subjectFlow(s intercept.Subject) *capture.Flow {
	return &capture.Flow{
		Host: s.Host, Path: s.Path, Method: s.Method, TargetID: s.TargetID,
		Source: capture.Source(s.Source), Origin: s.Origin,
	}
}

// readWSDecision reads a websocket.on_message_pending answer: a decision
// object, or nothing (a pass). An edit's payload may be text or a Uint8Array.
func readWSDecision(v any) (action, text string, bin []byte, err error) {
	if v == nil {
		return "pass", "", nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", "", nil, errDecisionShape
	}
	action, _ = m["action"].(string)
	if t, ok := m["text"].(string); ok {
		text = t
	}
	switch d := m["data"].(type) {
	case goja.ArrayBuffer:
		bin = d.Bytes()
	case []byte:
		bin = d
	}
	switch action {
	case "pass", "":
		return "pass", "", nil, nil
	case "edit", "forward", "drop":
		return action, text, bin, nil
	}
	return "", "", nil, errDecisionShape
}

var errDecisionShape = errorString("a websocket decision must be decision.pass/edit/forward/drop")

type errorString string

func (e errorString) Error() string { return string(e) }
