package manifest

import "sort"

// Hook names an event a plugin can attach a function to: a component and
// an event in it, "traffic.on_request_received".
type Hook string

// Class is how a hook's functions are called.
type Class int

const (
	// Lifecycle hooks are about the plugin itself: loading, unloading, its
	// status and settings changing. They jump the plugin's queue.
	Lifecycle Class = iota
	// Observe hooks report something that has happened. They are queued
	// and called asynchronously, after core has already acted, so they add
	// nothing to the proxy's latency; a plugin that falls behind loses
	// events (counted) rather than slowing traffic.
	Observe
	// Decide hooks are called while core waits for the answer: today the
	// intercept stage, on messages interception already holds.
	Decide
)

func (c Class) String() string {
	switch c {
	case Lifecycle:
		return "lifecycle"
	case Observe:
		return "observe"
	case Decide:
		return "decide"
	}
	return "unknown"
}

// Subject is what a hook's events are about, which decides what its match
// filter can test.
type Subject int

const (
	SubjectNone      Subject = iota // no match filter
	SubjectFlow                     // a captured exchange
	SubjectFinding                  // a finding
	SubjectEntity                   // a domain, host or URL in the Entity Explorer
	SubjectWebSocket                // a WebSocket connection or message
	SubjectFile                     // a file the work recovered
)

// HookInfo describes one hook in the catalog.
type HookInfo struct {
	Hook    Hook
	Class   Class
	Subject Subject
	// Request is set for flow hooks about the request side, so a content
	// type filter tests the request's Content-Type rather than the
	// response's.
	Request bool
	Doc     string
}

// The hooks, by component.
const (
	PluginOnLoad           Hook = "plugin.on_load"
	PluginOnUnload         Hook = "plugin.on_unload"
	PluginOnStatusChange   Hook = "plugin.on_status_change"
	PluginOnSettingsChange Hook = "plugin.on_settings_change"

	TrafficOnRequestReceived   Hook = "traffic.on_request_received"
	TrafficOnRequestForwarded  Hook = "traffic.on_request_forwarded"
	TrafficOnResponseReceived  Hook = "traffic.on_response_received"
	TrafficOnResponseForwarded Hook = "traffic.on_response_forwarded"
	TrafficOnFlowComplete      Hook = "traffic.on_flow_complete"
	TrafficOnFlowFailed        Hook = "traffic.on_flow_failed"
	TrafficOnFlowDropped       Hook = "traffic.on_flow_dropped"

	InterceptOnRequestPending  Hook = "intercept.on_request_pending"
	InterceptOnResponsePending Hook = "intercept.on_response_pending"
	InterceptOnReleased        Hook = "intercept.on_released"

	ReplayOnSent Hook = "replay.on_sent"

	FindingOnCreated Hook = "finding.on_finding_created"
	FindingOnUpdated Hook = "finding.on_finding_updated"
	FindingOnRemoved Hook = "finding.on_finding_removed"

	TargetOnChanged Hook = "target.on_changed"

	EngagementOnOpened Hook = "engagement.on_opened"
	EngagementOnClosed Hook = "engagement.on_closed"

	// Each type of entity has its lifecycle hooks, and so do entities as
	// a whole: a URL put in the Explorer calls url.on_created and
	// entity.on_created.
	EntityOnCreated Hook = "entity.on_created"
	EntityOnUpdated Hook = "entity.on_updated"
	EntityOnDeleted Hook = "entity.on_deleted"
	DomainOnCreated Hook = "domain.on_created"
	DomainOnUpdated Hook = "domain.on_updated"
	DomainOnDeleted Hook = "domain.on_deleted"
	HostOnCreated   Hook = "host.on_created"
	HostOnUpdated   Hook = "host.on_updated"
	HostOnDeleted   Hook = "host.on_deleted"
	URLOnCreated    Hook = "url.on_created"
	URLOnUpdated    Hook = "url.on_updated"
	URLOnDeleted    Hook = "url.on_deleted"

	// A file the work recovered: the source a map carried, a bundle taken
	// apart, anything else a plugin, an agent or the user pulled out of the
	// traffic (see internal/files). This is how a plugin that scans text
	// for secrets reads recovered client source with the code it already
	// has for response bodies.
	FileOnCreated       Hook = "file.on_created"
	FileOnUpdated       Hook = "file.on_updated"
	FileOnDeleted       Hook = "file.on_deleted"
	FileOnBatchComplete Hook = "file.on_batch_complete"

	WebSocketOnOpen           Hook = "websocket.on_open"
	WebSocketOnMessage        Hook = "websocket.on_message"
	WebSocketOnMessagePending Hook = "websocket.on_message_pending"
	WebSocketOnClose          Hook = "websocket.on_close"
)

// Catalog lists every hook a plugin can target.
var Catalog = []HookInfo{
	{PluginOnLoad, Lifecycle, SubjectNone, false, "The plugin was loaded: at startup, when enabled, reloaded, or changed on disk."},
	{PluginOnUnload, Lifecycle, SubjectNone, false, "The plugin is about to be unloaded."},
	{PluginOnStatusChange, Lifecycle, SubjectNone, false, "The plugin was enabled or disabled, by the user or because it kept failing."},
	{PluginOnSettingsChange, Lifecycle, SubjectNone, false, "The user changed the plugin's settings."},

	{TrafficOnRequestReceived, Observe, SubjectFlow, true, "A request was captured. Streamed requests are already on their way upstream."},
	{TrafficOnRequestForwarded, Observe, SubjectFlow, true, "A request was sent upstream (after any edit made while it was held)."},
	{TrafficOnResponseReceived, Observe, SubjectFlow, false, "A response arrived from upstream."},
	{TrafficOnResponseForwarded, Observe, SubjectFlow, false, "A response was sent to the client (after any edit made while it was held)."},
	{TrafficOnFlowComplete, Observe, SubjectFlow, false, "An exchange finished: one call per completed request and response."},
	{TrafficOnFlowFailed, Observe, SubjectFlow, true, "An exchange ended without a response."},
	{TrafficOnFlowDropped, Observe, SubjectFlow, true, "A held request or response was dropped."},

	{InterceptOnRequestPending, Decide, SubjectFlow, true, "Interception is holding a request: pass, edit, forward or drop it before the user sees it."},
	{InterceptOnResponsePending, Decide, SubjectFlow, false, "Interception is holding a response: pass, edit, forward or drop it before the user sees it."},
	{InterceptOnReleased, Observe, SubjectNone, false, "A held message was forwarded or dropped."},

	{ReplayOnSent, Observe, SubjectFlow, true, "A replay was sent, by the user, an agent or a plugin."},

	{FindingOnCreated, Observe, SubjectFinding, false, "A finding was recorded."},
	{FindingOnUpdated, Observe, SubjectFinding, false, "A finding changed: an edit, or a new sighting merged into it."},
	{FindingOnRemoved, Observe, SubjectFinding, false, "A finding was deleted."},

	{TargetOnChanged, Observe, SubjectNone, false, "The targets in scope changed."},

	{EngagementOnOpened, Observe, SubjectNone, false, "An engagement was opened."},
	{EngagementOnClosed, Observe, SubjectNone, false, "The open engagement was closed."},

	{EntityOnCreated, Observe, SubjectEntity, false, "An entity (domain, host or URL) was put in the Entity Explorer."},
	{EntityOnUpdated, Observe, SubjectEntity, false, "An entity changed: a request reached it or something under it, it gained a link, or its key/value store was edited."},
	{EntityOnDeleted, Observe, SubjectEntity, false, "An entity was removed from the Entity Explorer (deleted, or the Explorer cleared)."},
	{DomainOnCreated, Observe, SubjectEntity, false, "A domain was put in the Entity Explorer."},
	{DomainOnUpdated, Observe, SubjectEntity, false, "A domain changed: a request reached one of its hosts, or its key/value store was edited."},
	{DomainOnDeleted, Observe, SubjectEntity, false, "A domain was removed from the Entity Explorer."},
	{HostOnCreated, Observe, SubjectEntity, false, "A host was put in the Entity Explorer."},
	{HostOnUpdated, Observe, SubjectEntity, false, "A host changed: a request reached one of its URLs, or its key/value store was edited."},
	{HostOnDeleted, Observe, SubjectEntity, false, "A host was removed from the Entity Explorer."},
	{URLOnCreated, Observe, SubjectEntity, false, "A URL was put in the Entity Explorer: requested, or discovered in a response."},
	{URLOnUpdated, Observe, SubjectEntity, false, "A URL gained a request or a link, or its key/value store was edited."},
	{URLOnDeleted, Observe, SubjectEntity, false, "A URL was removed from the Entity Explorer."},

	{FileOnCreated, Observe, SubjectFile, false, "A file was recovered: written to the engagement's recovered files for the first time."},
	{FileOnUpdated, Observe, SubjectFile, false, "A recovered file changed: recovered again with different bytes, or its record edited. Recovering the same bytes again raises nothing."},
	{FileOnDeleted, Observe, SubjectFile, false, "A recovered file was deleted, or the recovered files were cleared."},
	{FileOnBatchComplete, Observe, SubjectFile, false, "A recovery of many files finished (unpacking a source map, say). Every file raised its own event too, but a plugin that cannot keep up with thousands of them can take the batch here and read it with files.list({batch})."},

	{WebSocketOnOpen, Observe, SubjectWebSocket, false, "A WebSocket connection was opened (its 101 upgrade completed)."},
	{WebSocketOnMessage, Observe, SubjectWebSocket, false, "A WebSocket message was sent or received."},
	{WebSocketOnMessagePending, Decide, SubjectWebSocket, false, "A WebSocket message is held for editing before it is forwarded (needs hold). The connection's compression is stripped so it can be edited."},
	{WebSocketOnClose, Observe, SubjectWebSocket, false, "A WebSocket connection closed."},
}

var byHook = func() map[Hook]HookInfo {
	m := make(map[Hook]HookInfo, len(Catalog))
	for _, h := range Catalog {
		m[h.Hook] = h
	}
	return m
}()

// Lookup returns a hook's description.
func Lookup(h Hook) (HookInfo, bool) {
	info, ok := byHook[h]
	return info, ok
}

// Hooks returns every hook name, sorted.
func Hooks() []string {
	out := make([]string, 0, len(Catalog))
	for _, h := range Catalog {
		out = append(out, string(h.Hook))
	}
	sort.Strings(out)
	return out
}
