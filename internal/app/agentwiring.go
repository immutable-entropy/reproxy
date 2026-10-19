package app

import (
	"context"
	"encoding/json"
	"fmt"

	"reproxy/internal/agent"
	"reproxy/internal/config"
	"reproxy/internal/mcp"
)

// This file connects the agent runtime (internal/agent) to the MCP tool
// registry and the configured providers, so the in-app Agents panel drives
// exactly the same tools as external MCP clients.

// agentProviders builds a Provider for a session's profile from the current
// agent configuration and the stored API key.
type agentProviders struct{ app *App }

func (p agentProviders) For(profileID string) (agent.Provider, error) {
	prof, ok := p.app.AgentConfig().Profile(profileID)
	if !ok {
		return nil, fmt.Errorf("this agent is no longer configured; add it again in Settings → Agents")
	}
	key := p.app.APIKey(profileID)
	if v, ok := agent.VendorByID(prof.Vendor); ok && v.NeedsKey && key == "" {
		return nil, fmt.Errorf("no API key is set for %q; add one in Settings → Agents", prof.Name)
	}
	return agent.NewProvider(prof.Vendor, prof.Model, prof.BaseURL, key, prof.MaxTokens)
}

// agentTools adapts the MCP registry to agent.ToolBox. A session's mode maps
// to the registry's approval policy, so an Ask session blocks on the user
// (through the broker) exactly as an external ask-mode client would.
type agentTools struct{ reg *mcp.Registry }

func (t agentTools) Specs() []agent.ToolSpec {
	tools := t.reg.Tools()
	out := make([]agent.ToolSpec, 0, len(tools))
	for _, tl := range tools {
		out = append(out, agent.ToolSpec{Name: tl.Name, Description: tl.Description, Schema: tl.Schema})
	}
	return out
}

func (t agentTools) Invoke(ctx context.Context, turn agent.Turn, name string, args json.RawMessage) (string, bool, error) {
	caller := mcp.Caller{Session: turn.Session, Policy: policyForMode(turn.Mode), AskedAt: turn.AskedAt}
	result, err := t.reg.Call(ctx, caller, name, args)
	if err != nil {
		// A tool-level error (denied, scope, bad args, not held) is shown to
		// the model so it can adjust; only a cancelled context ends the turn.
		if ctx.Err() != nil {
			return "", true, ctx.Err()
		}
		return err.Error(), true, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err.Error(), true, nil
	}
	return string(data), false, nil
}

// policyForMode maps an agent session's mode to the MCP approval policy.
func policyForMode(mode agent.Mode) string {
	if mode == agent.ModeAuto {
		return config.PolicyAuto
	}
	return config.PolicyAsk
}
