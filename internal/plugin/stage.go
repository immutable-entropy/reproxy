package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/manifest"
)

// stage runs the intercept hooks: installed on the Interceptor only while
// some running plugin targets one, it asks each such plugin about a message
// interception is holding, in priority order (then by name), before the
// user sees it.
//
// Each answer is one of:
//
//	pass     no opinion (also: undefined, a timeout, an error)
//	edit     replace the message; the next plugin sees the edit
//	forward  send it on now (edited, if text is given)
//	drop     drop it now
//
// Forward and drop release the message: the user never sees it. With
// allowRelease off they are taken as an edit and a pass for what the user's
// interception holds, so all of that still reaches the user. A message only
// a plugin asked to hold (hold: true) is the plugins' alone: it is released
// whatever they answer, and forwarded when they pass.
type stage struct {
	d            *Dispatcher
	store        *capture.Store
	allowRelease atomic.Bool
}

func (s *stage) run(ctx context.Context, p intercept.Pending) intercept.StageResult {
	hook := manifest.InterceptOnRequestPending
	if p.Dir == intercept.Response {
		hook = manifest.InterceptOnResponsePending
	}
	rt := s.d.routes.Load()
	if rt == nil || rt.mask&bit(hook) == 0 {
		return intercept.StageResult{}
	}
	f, ok := s.store.Get(p.FlowID)
	if !ok || f.IsDNS() { // the intercept hooks are about HTTP messages
		return intercept.StageResult{}
	}
	origin, _ := OriginOf(f.Origin)
	facts := manifest.Facts{
		Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
		Status: f.StatusCode, Host: f.Host, Path: f.Path, Head: p.Raw,
	}
	if f.TargetID == "" {
		facts.NoTargets = s.d.noTargets()
	}

	current := p.Raw
	var res intercept.StageResult
	for _, r := range rt.byHook[hookIndex[hook]] {
		if origin == r.p.src.Name || !r.m.Match(&facts) {
			continue // a plugin never decides on its own traffic
		}
		budget := time.Duration(r.millis) * time.Millisecond
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < budget {
			budget = time.Until(dl)
		}
		if budget <= 0 {
			break
		}
		msg := current
		flow := f
		answer, err := r.p.await(hook, r.fn, 0, budget, func(vm *goja.Runtime) goja.Value {
			o := event(vm, hook, "flow", flowValue(vm, &flow)).ToObject(vm)
			o.Set("message", messageValue(vm, msg, int64(len(msg)), false, ""))
			return o
		})
		if err != nil || ctx.Err() != nil {
			continue // already counted and logged; a failed answer is a pass
		}
		action, text, err := readDecision(answer)
		if err != nil {
			r.p.fail(hook, r.fn, err)
			continue
		}
		if text != nil {
			current = intercept.ApplyEdit(current, *text, true)
			res.Raw = current
		}
		switch action {
		case "forward", "drop":
			if p.ForUser && !s.allowRelease.Load() {
				r.p.logf(slog.LevelInfo, fmt.Sprintf("%s asked to %s flow %d, but plugins may not release held messages (Plugins window)", r.fn, action, p.FlowID))
				continue
			}
			v := intercept.Forward
			if action == "drop" {
				v = intercept.Drop
			}
			res.Release = &v
			return res
		}
	}
	return res
}

// readDecision reads an intercept hook's answer: a Decision object, or
// nothing (a pass).
func readDecision(v any) (action string, text *string, err error) {
	if v == nil {
		return "pass", nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("an intercept hook must return a decision (see decision.*), not %T", v)
	}
	action, _ = m["action"].(string)
	if t, ok := m["text"].(string); ok {
		text = &t
	}
	switch strings.ToLower(action) {
	case "pass", "":
		return "pass", nil, nil
	case "edit":
		if text == nil {
			return "", nil, fmt.Errorf("an edit decision needs the edited text")
		}
		return "edit", text, nil
	case "forward":
		return "forward", text, nil
	case "drop":
		return "drop", nil, nil
	}
	return "", nil, fmt.Errorf("unknown decision %q", action)
}
