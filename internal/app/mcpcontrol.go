package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"reproxy/internal/hub"
	"reproxy/internal/mcp"
)

// errNotJoined is returned when an MCP action is attempted before this
// process has joined the reproxy group.
var errNotJoined = errors.New("not connected to the reproxy group yet")

// The MCP server runs on the hub process. A non-hub process starts, stops
// and queries it through the hub's authenticated control route
// (hub.HubPrefix+"mcp/{action}"), so the footer and the Tools menu work the
// same in every window.

// mcpControl POSTs a start/stop action to the hub and returns the resulting
// status.
func (a *App) mcpControl(action string) error {
	_, err := a.hubMCP(http.MethodPost, action)
	return err
}

// hubMCPStatus asks the hub for the current MCP status.
func (a *App) hubMCPStatus() (mcp.Status, bool) {
	st, err := a.hubMCP(http.MethodGet, "status")
	if err != nil {
		return mcp.Status{}, false
	}
	return st, true
}

func (a *App) hubMCP(method, action string) (mcp.Status, error) {
	a.mu.Lock()
	node := a.node
	a.mu.Unlock()
	if node == nil {
		return mcp.Status{}, errNotJoined
	}
	addr, _, ok := node.Hub()
	if !ok {
		return mcp.Status{}, errNotJoined
	}
	tok, err := node.Token()
	if err != nil {
		return mcp.Status{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "http://" + addr + hub.HubPrefix + "mcp/" + action
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return mcp.Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return mcp.Status{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return mcp.Status{}, fmt.Errorf("hub MCP %s: %s", action, resp.Status)
	}
	var st mcp.Status
	json.Unmarshal(body, &st)
	return st, nil
}
