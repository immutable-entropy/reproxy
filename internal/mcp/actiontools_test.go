package mcp

import (
	"context"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/config"
)

func TestPluginActionTools(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "app.test", "/assets/app.js.map", "{}")
	handle := flowHandle(testInstance, id)

	var ranPlugin, ranAction string
	f.reg.SetPluginActions(
		func(fl capture.Flow) []PluginAction {
			if fl.ID != id {
				return nil
			}
			return []PluginAction{{Plugin: "spa-recon", ID: "recover-sources", Label: "Recover source from map", Description: "…"}}
		},
		func(_ context.Context, plugin, action string, _ capture.Flow) (string, error) {
			ranPlugin, ranAction = plugin, action
			return "Recovered 3 source files", nil
		},
	)

	// list_plugin_actions returns what the flow is offered.
	var list struct {
		Actions []PluginAction `json:"actions"`
	}
	if err := f.call(t, auto, "list_plugin_actions", map[string]any{"flow": handle}, &list); err != nil {
		t.Fatalf("list_plugin_actions: %v", err)
	}
	if len(list.Actions) != 1 || list.Actions[0].ID != "recover-sources" {
		t.Fatalf("listed actions = %+v", list.Actions)
	}

	// run_plugin_action runs it and returns the message.
	var run struct {
		Message string `json:"message"`
	}
	if err := f.call(t, auto, "run_plugin_action",
		map[string]any{"flow": handle, "plugin": "spa-recon", "action": "recover-sources"}, &run); err != nil {
		t.Fatalf("run_plugin_action: %v", err)
	}
	if run.Message != "Recovered 3 source files" || ranPlugin != "spa-recon" || ranAction != "recover-sources" {
		t.Fatalf("run = %q, ran %s/%s", run.Message, ranPlugin, ranAction)
	}

	// An action the flow is not offered is refused before running.
	ranPlugin = ""
	err := f.call(t, auto, "run_plugin_action",
		map[string]any{"flow": handle, "plugin": "spa-recon", "action": "nope"}, nil)
	if err == nil {
		t.Fatal("expected an error for an action the flow isn't offered")
	}
	if ranPlugin != "" {
		t.Fatalf("the action ran despite not being offered")
	}

	// run_plugin_action mutates, so a read-only caller is refused.
	ro := Caller{Policy: config.PolicyReadOnly}
	if err := f.call(t, ro, "run_plugin_action",
		map[string]any{"flow": handle, "plugin": "spa-recon", "action": "recover-sources"}, nil); err != ErrReadOnly {
		t.Fatalf("read-only run = %v, want ErrReadOnly", err)
	}
	// list_plugin_actions is read-only and allowed.
	if err := f.call(t, ro, "list_plugin_actions", map[string]any{"flow": handle}, nil); err != nil {
		t.Fatalf("read-only list refused: %v", err)
	}
}
