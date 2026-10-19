package plugin

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/plugin/manifest"
)

// Actions are what a plugin offers to run on a flow the user picks, from
// the traffic list's context menu (manifest.Action). Nothing calls one but
// the user: listing them costs a filter match per action, done when the
// menu opens, and nothing is added to the proxy's path.
//
// An action's function is called on the plugin's goroutine like any hook,
// with each turn under the usual turnBudget; the call as a whole may take
// up to actionBudget, because an action is expected to send requests and
// wait for them. Whatever it sends goes through http.send, so through the
// upstream chain (see http.go).

// actionBudget bounds a whole action: its turns and the waits between them.
const actionBudget = 5 * time.Minute

// actionHook names actions in a plugin's log and error counts.
const actionHook manifest.Hook = "action"

// Action is one action a running plugin offers.
type Action struct {
	Plugin      string
	ID          string
	Label       string
	Description string
}

type compiledAction struct {
	manifest.Action
	m *manifest.Matcher
}

// ActionResult is what an action reports back.
type ActionResult struct {
	// Message is what the action returned for the user, if anything.
	Message string
}

// Actions lists the actions the running plugins offer for a flow: those
// whose filter accepts it, by plugin name and then in each manifest's order.
func (m *Manager) Actions(f capture.Flow) []Action {
	facts := m.actionFacts(&f)
	var out []Action
	for _, r := range m.runningPlugins() {
		for _, a := range r.actions {
			if a.m.Match(facts) {
				out = append(out, Action{Plugin: r.src.Name, ID: a.ID, Label: strings.TrimSpace(a.Label), Description: a.Description})
			}
		}
	}
	return out
}

// RunAction runs one plugin's action on a flow and waits for it to finish.
// The flow is read again from the store, so the action sees it as it is
// now. It is refused if the plugin isn't running or the action no longer
// applies to the flow.
func (m *Manager) RunAction(pluginName, actionID string, f capture.Flow) (ActionResult, error) {
	if m.deps.Store != nil {
		if now, ok := m.deps.Store.Get(f.ID); ok {
			f = now
		}
	}
	var run *Running
	for _, r := range m.runningPlugins() {
		if r.src.Name == pluginName {
			run = r
		}
	}
	if run == nil {
		return ActionResult{}, fmt.Errorf("%s isn't running", pluginName)
	}
	a, ok := run.action(actionID)
	if !ok {
		return ActionResult{}, fmt.Errorf("%s has no action %q", pluginName, actionID)
	}
	if !a.m.Match(m.actionFacts(&f)) {
		return ActionResult{}, fmt.Errorf("%s's %q doesn't apply to this request", pluginName, a.Label)
	}
	return run.runAction(a, f)
}

// runningPlugins are the plugins running and not faulted, by name.
func (m *Manager) runningPlugins() []*Running {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Running
	for _, e := range m.entries {
		if e.run != nil && !e.faulted {
			out = append(out, e.run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].src.Name < out[j].src.Name })
	return out
}

func (m *Manager) actionFacts(f *capture.Flow) *manifest.Facts {
	return flowFacts(f, func() bool { return m.disp.noTargets() })
}

// flowFacts are what a filter tests on a flow, with its response's head for
// content_types (an action's filter is about the response).
func flowFacts(f *capture.Flow, noTargets func() bool) *manifest.Facts {
	facts := &manifest.Facts{
		Source: sourceName(f.Source), TargetID: f.TargetID, Method: f.Method,
		Status: f.StatusCode, Host: f.Host, Path: f.Path, Head: f.Response.Raw,
	}
	if f.TargetID == "" {
		facts.NoTargets = noTargets()
	}
	return facts
}

func (r *Running) action(id string) (compiledAction, bool) {
	for _, a := range r.actions {
		if a.ID == id {
			return a, true
		}
	}
	return compiledAction{}, false
}

// runAction calls an action's function with { hook, action, flow } and waits
// for it. The function may return (or resolve with) a string, or an object
// with a message, for the user.
func (r *Running) runAction(a compiledAction, f capture.Flow) (ActionResult, error) {
	r.logf(slog.LevelInfo, fmt.Sprintf("action %s on %s %s", a.ID, f.Method, flowURL(&f)))
	v, err := r.awaitFor(actionHook, a.Function, 0, turnBudget, actionBudget, func(vm *goja.Runtime) goja.Value {
		return actionEvent(vm, a, &f)
	})
	if err != nil {
		return ActionResult{}, errors.New(firstLine(err.Error()))
	}
	res := ActionResult{Message: actionMessage(v)}
	if res.Message != "" {
		r.logf(slog.LevelInfo, fmt.Sprintf("action %s: %s", a.ID, res.Message))
	}
	return res, nil
}

// actionEvent is what an action's function is called with.
func actionEvent(vm *goja.Runtime, a compiledAction, f *capture.Flow) goja.Value {
	o := vm.NewObject()
	o.Set("hook", string(actionHook))
	o.Set("action", a.ID)
	o.Set("flow", flowValue(vm, f))
	return o
}

// actionMessage reads what an action returned: a string, or { message }.
func actionMessage(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		if s, ok := x["message"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func flowURL(f *capture.Flow) string {
	scheme := "http"
	if f.Secure {
		scheme = "https"
	}
	return scheme + "://" + f.Host + f.Path
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
