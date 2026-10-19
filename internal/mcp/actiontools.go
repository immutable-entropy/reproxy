package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The action tools let an agent run a plugin's actions — the same actions a
// user runs from the traffic list's right-click menu (manifest.Action, see
// internal/plugin). list_plugin_actions reports which a flow is offered;
// run_plugin_action runs one.
//
// An action can send requests (through the upstream chain) and record
// findings or entities, so run_plugin_action is a Mutates tool: it passes
// the approval gate like send_request. Unlike send_request it does not
// enforce the agent's same-domain scope rule, because a plugin is not bound
// by it — the plugin's own code decides what it fetches; the approval gate
// is the control. Plugin actions are local to the process that has the
// engagement open, so both tools resolve a flow of this instance.

var listPluginActions = &Tool{
	Name: "list_plugin_actions",
	Description: "List the plugin actions offered for a flow — the same actions a user could run from the " +
		"traffic list's right-click menu. Each has a plugin, an id, a label and a description. Pass one of " +
		"these to run_plugin_action. Which actions apply depends on the flow (an action filters on the " +
		"request/response), so call this for the specific flow you mean.",
	Schema: object(map[string]any{
		"flow": strField("The flow to list actions for, as a flow handle from list_flows. It must be a flow of this instance."),
	}, "flow"),
	run: func(ctx context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow string `json:"flow"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if r.listActions == nil {
			return nil, errors.New("this reproxy process has no plugins")
		}
		inst, flowID, err := r.localFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		f, err := inst.GetFlow(ctx, flowID)
		if err != nil {
			return nil, err
		}
		actions := r.listActions(f)
		if actions == nil {
			actions = []PluginAction{}
		}
		return map[string]any{"actions": actions}, nil
	},
}

var runPluginAction = &Tool{
	Name: "run_plugin_action",
	Description: "Run a plugin action on a flow — the same thing a user does from the traffic list's " +
		"right-click menu. Get plugin and action from list_plugin_actions for the flow. The action may send " +
		"requests (through the configured upstream proxy, to any host the plugin chooses — the agent's " +
		"same-domain limit does not apply to a plugin) and may record findings or entities you can then read " +
		"back. Returns the message the action reported.",
	Mutates: true,
	Schema: object(map[string]any{
		"flow":   strField("The flow to run the action on, as a flow handle from list_flows. It must be a flow of this instance."),
		"plugin": strField("The plugin's name, from list_plugin_actions."),
		"action": strField("The action's id, from list_plugin_actions."),
	}, "flow", "plugin", "action"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow   string `json:"flow"`
			Plugin string `json:"plugin"`
			Action string `json:"action"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Plugin == "" || a.Action == "" {
			return nil, errors.New("plugin and action are required")
		}
		if r.runAction == nil || r.listActions == nil {
			return nil, errors.New("this reproxy process has no plugins")
		}
		inst, flowID, err := r.localFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		f, err := inst.GetFlow(ctx, flowID)
		if err != nil {
			return nil, err
		}
		// Only offer what the flow is actually offered, so the agent can't
		// run an action whose filter doesn't match (as the menu wouldn't),
		// and so the summary can name the action the user is approving.
		label := ""
		for _, act := range r.listActions(f) {
			if act.Plugin == a.Plugin && act.ID == a.Action {
				label = act.Label
				break
			}
		}
		if label == "" {
			return nil, fmt.Errorf("the %s plugin offers no action %q for this flow; call list_plugin_actions", a.Plugin, a.Action)
		}
		summary := fmt.Sprintf("Run the %s plugin's action %q on %s %s (it may send requests)", a.Plugin, label, f.Method, f.URL())
		if err := r.gate(ctx, c, "run_plugin_action", summary); err != nil {
			return nil, err
		}
		msg, err := r.runAction(ctx, a.Plugin, a.Action, f)
		if err != nil {
			return nil, err
		}
		return map[string]any{"plugin": a.Plugin, "action": a.Action, "message": msg}, nil
	},
}
