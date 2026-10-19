// Package intercept decides which messages the proxy holds for the user and
// manages them while they are held.
//
// It is core (see CLAUDE.md) and has no Qt dependency: the UI, and later
// plugins and LLM tooling, drive interception through the same Interceptor.
//
// What the user holds is configured per target (see package target): when
// interception is on, a target holds its requests, its responses, or both.
// The Interceptor is handed a function that answers that question for a
// subject (see Interceptor.SetUserHold); it keeps no rules of its own.
//
// Hot-path rule: every message passing through the proxy calls
// Interceptor.Holds. With interception off and no plugin wanting holds that
// is a couple of atomic loads.
package intercept

import "net/netip"

// Settings is the persisted interception configuration. What to hold lives on
// the targets now; only the editing behaviour is global. Whether interception
// is switched on is session state and lives on the Interceptor.
type Settings struct {
	// UpdateContentLength rewrites Content-Length to match an edited body.
	// Turn it off to send deliberately inconsistent lengths.
	UpdateContentLength bool `json:"update_content_length"`
}

// DefaultSettings returns the settings used before the user changes any.
func DefaultSettings() Settings {
	return Settings{UpdateContentLength: true}
}

// Validate reports whether the settings can be applied. There is nothing to
// compile any more, so they always can.
func (s Settings) Validate() error { return nil }

// Subject is what a hold decision is made about.
type Subject struct {
	Host        string // Host header, may include a port
	IP          netip.Addr
	Path        string // request-target, may include a query string
	ContentType string // raw Content-Type header value

	// DNS marks a name lookup rather than an HTTP message: Host is the name
	// and IP, for an answer, the first address it resolved to. A target
	// holds lookups only when it opts in (target.Intercept.DNS), and
	// plugins are never asked about them.
	DNS bool

	// The rest are for plugins' holds (see PluginHoldFunc); a target's
	// request/response choice doesn't look at them.
	Method   string
	Status   int    // a response's status code; 0 for a request
	TargetID string // the target the flow fell in
	Source   string // capture.Source of the flow: "" for the proxy's own
	Origin   string // the plugin that sent the flow, if one did
}
