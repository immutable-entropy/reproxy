package app

import (
	"context"

	"reproxy/internal/capture"
	"reproxy/internal/mcp"
)

// These adapt the plugin Manager's actions to what the MCP action tools
// need, so mcp need not import the plugin package. They are wired into the
// registry in app.go (SetPluginActions).

// pluginActions lists the plugin actions offered for a flow.
func (a *App) pluginActions(f capture.Flow) []mcp.PluginAction {
	acts := a.plugins.Actions(f)
	out := make([]mcp.PluginAction, 0, len(acts))
	for _, act := range acts {
		out = append(out, mcp.PluginAction{Plugin: act.Plugin, ID: act.ID, Label: act.Label, Description: act.Description})
	}
	return out
}

// runPluginAction runs one plugin action and returns the message it reported.
// The plugin Manager bounds the call itself (actionBudget), so the context
// is not threaded through.
func (a *App) runPluginAction(_ context.Context, pluginName, actionID string, f capture.Flow) (string, error) {
	res, err := a.plugins.RunAction(pluginName, actionID, f)
	if err != nil {
		return "", err
	}
	return res.Message, nil
}
