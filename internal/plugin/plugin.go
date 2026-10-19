// Package plugin runs plugins: user-definable TypeScript that reacts to
// what happens in reproxy and can drive its major actions.
//
// A plugin is a directory holding plugin.json (see package manifest) and
// main.ts, from which everything else it needs is imported. Plugins live in
// the data directory's plugins/: put there by hand, or installed from a
// repository (package registry). The Manager finds them, validates
// them, and runs each enabled one in a runtime of its own (package jsrt).
//
// The proxy's hot path pays for plugins only when one wants the traffic:
//
//   - The Dispatcher is a capture.Store subscriber whose first act is one
//     atomic load of an immutable routing table. With no enabled plugin
//     targeting traffic it returns at once.
//   - Otherwise it works out which hooks the event means from the flow's
//     state transition, and runs each target's compiled match filter (in
//     Go, cheapest test first). Traffic no filter accepts never reaches a
//     plugin.
//   - What does match is queued, non-blocking, on that plugin's own bounded
//     queue; a plugin that falls behind loses events (counted), it never
//     slows traffic. Building the values the plugin sees, decoding bodies
//     included, happens on the plugin's goroutine.
//
// Intercept hooks are the exception, by design: interception is already
// holding the message, so the Stage (installed on the Interceptor only while
// some plugin targets an intercept hook) asks the plugins in priority order
// and waits, each within its timeout.
//
// A plugin's own traffic — what it sends through the proxy to have it
// recorded — carries its name as the flow's Origin, and its hooks skip
// those flows unless a target opts in with self, and its intercept hooks
// always do. Sends through the proxy also carry a depth, refused past
// MaxDepth, so plugins that do opt in can't feed each other forever.
//
// Plugins can make outbound requests to any host: unlike an agent, a plugin
// is code the user installed, and internal/scope's same-domain rule (the
// LLM's) does not apply. Those requests always go out through the configured
// upstream proxy chain.
package plugin

import (
	"context"
	"database/sql"
	"log/slog"
	"net"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
	"reproxy/internal/target"
)

// MaxDepth is how many times traffic a plugin sends through the proxy may
// lead to more: a plugin whose hook sees its own traffic (self) and sends
// again from it is refused past this.
const MaxDepth = 3

// Deps are what plugins act on. The app provides them; the test runner
// provides its own.
type Deps struct {
	// DataDir is the data directory: user plugins are in its plugins/, and
	// each plugin's own storage in plugin-data/<name>/.
	DataDir string

	Store     *capture.Store
	Intercept *intercept.Interceptor
	Findings  *findings.Store
	Targets   *target.Store
	Entities  *entities.Store
	// Files is what the work recovered: a plugin reads recovered files and
	// adds its own through it (files.add/list/read).
	Files *files.Store

	// Send performs one exchange (replay.Send, or a fake in tests).
	Send func(context.Context, replay.Request) (replay.Response, error)
	// Dial connects through the configured upstream proxy chain.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// ProxyDial connects through reproxy's own proxy, labelled so the
	// flows it records are the plugin's (see OriginLabel). Nil when the
	// proxy isn't running.
	ProxyDial func(ctx context.Context, network, addr, label string) (net.Conn, error)
	// Secrets holds secret settings.
	Secrets SecretStore
	// EngagementDB is the open engagement's database, for plugins'
	// per-engagement storage; nil when there is none.
	EngagementDB func() *sql.DB

	Log *slog.Logger
}

// SecretStore keeps the values of plugins' secret settings.
type SecretStore interface {
	PluginSecret(plugin, key string) (string, error)
}

func (d *Deps) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}
