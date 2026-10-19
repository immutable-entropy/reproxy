package cli

import "strings"

// The core namespaces. This table is the whole list, and it is reserved:
// every name and alias in it belongs to core, whether or not a command has
// been written for it yet, so a plugin can never take one (see Reserved,
// which the plugin Manager calls before it starts a plugin).
//
// A namespace is the plural of the core object it acts on, and the singular
// is an alias, so both `reproxy findings ls` and `reproxy finding ls` work.
// Reserving the names up front is deliberate: a plugin that took `findings`
// today would break the day core grew that command, and the user would have
// no way to tell whose command they were running.
//
// A plugin's namespace is its own name, so this table is also the list of
// names a plugin that wants commands may not be called.

// Namespace is one core namespace.
type Namespace struct {
	// Name is the canonical form: what help prints.
	Name string
	// Aliases are the other spellings that resolve to it.
	Aliases []string
	// Summary is the one line help lists it with.
	Summary string
	// Object names the core object or package behind it, so it is obvious
	// what a command in this namespace is allowed to touch.
	Object string
}

// Builtins are the words the command line answers itself, before any
// instance is contacted. They are reserved like a namespace.
var Builtins = []string{"help", "version", "shell", "completion"}

var coreNamespaces = []Namespace{
	// ---- Traffic -----------------------------------------------------
	{Name: "flows", Aliases: []string{"flow", "traffic"}, Object: "capture.Store",
		Summary: "captured requests and responses"},
	{Name: "intercept", Object: "intercept.Interceptor",
		Summary: "hold, edit, forward and drop messages"},
	{Name: "replays", Aliases: []string{"replay"}, Object: "replay.Sessions",
		Summary: "replay sessions and the requests sent from them"},
	{Name: "websockets", Aliases: []string{"websocket", "ws"}, Object: "websocket",
		Summary: "WebSocket messages within a flow"},
	{Name: "dns", Object: "dns.Resolver",
		Summary: "name lookups and the resolver's cache"},
	{Name: "ca", Aliases: []string{"cert", "certs"}, Object: "tlsca",
		Summary: "the HTTPS interception certificate authority"},

	// ---- Scope and the engagement ------------------------------------
	{Name: "targets", Aliases: []string{"target"}, Object: "target.Store",
		Summary: "what is in scope, what it holds, and what is suppressed"},
	{Name: "engagements", Aliases: []string{"engagement"}, Object: "engagement.Store",
		Summary: "the engagement folder and its archive"},
	{Name: "crawl", Object: "crawl.Crawler",
		Summary: "Autocrawl: fetch every URL the targets cover"},
	{Name: "entities", Aliases: []string{"entity"}, Object: "entities.Store",
		Summary: "the Explorer: domains, hosts and URLs the traffic turned up"},
	{Name: "findings", Aliases: []string{"finding"}, Object: "findings.Store",
		Summary: "what the work turned up"},
	{Name: "files", Aliases: []string{"file"}, Object: "files.Store",
		Summary: "the files the work recovered, and what they hold"},

	// ---- Plugins and agents ------------------------------------------
	{Name: "plugins", Aliases: []string{"plugin"}, Object: "plugin.Manager",
		Summary: "the plugins installed here, their repositories, and the author tools"},
	{Name: "agents", Aliases: []string{"agent"}, Object: "agent.Sessions",
		Summary: "LLM chat sessions watching the traffic"},
	{Name: "approvals", Aliases: []string{"approval"}, Object: "approval.Store",
		Summary: "agent actions waiting for a yes or no"},
	{Name: "mcp", Object: "mcp.Manager",
		Summary: "the MCP server external agents attach to"},
	{Name: "focus", Object: "focus.Tracker",
		Summary: "what the user is looking at, so \"this request\" resolves"},

	// ---- The process and its settings --------------------------------
	{Name: "instances", Aliases: []string{"instance"}, Object: "hub.Router",
		Summary: "the reproxy processes sharing this data directory"},
	{Name: "settings", Aliases: []string{"setting", "config"}, Object: "config.Config",
		Summary: "every setting, the proxy port and the upstream chain included"},
	{Name: "secrets", Aliases: []string{"secret"}, Object: "secrets.Store",
		Summary: "API keys and the MCP bearer token (reserved; nothing planned)"},
	{Name: "browser", Aliases: []string{"browsers"}, Object: "browser",
		Summary: "the browsers reproxy launches through itself"},
}

// byName indexes the table by every spelling it answers to.
var byName = func() map[string]*Namespace {
	m := make(map[string]*Namespace, len(coreNamespaces)*2)
	for i := range coreNamespaces {
		ns := &coreNamespaces[i]
		m[ns.Name] = ns
		for _, a := range ns.Aliases {
			m[a] = ns
		}
	}
	for _, b := range Builtins {
		m[b] = nil // reserved, but not a namespace
	}
	return m
}()

// CoreNamespaces returns the table, in the order it is declared.
func CoreNamespaces() []Namespace {
	out := make([]Namespace, len(coreNamespaces))
	copy(out, coreNamespaces)
	return out
}

// Reserved reports whether name belongs to core, by any spelling. A plugin
// named one of these may not declare commands: core names are not
// negotiable, because the user has to be able to trust what
// `reproxy findings ls` does.
func Reserved(name string) bool {
	_, ok := byName[fold(name)]
	return ok
}

// Lookup returns the core namespace a name resolves to.
func Lookup(name string) (Namespace, bool) {
	ns, ok := byName[fold(name)]
	if !ok || ns == nil {
		return Namespace{}, false
	}
	return *ns, true
}

// Canonical folds a namespace to the name help prints: a core alias becomes
// the core name, and anything else is returned as it was typed (a plugin's
// namespace has no aliases).
func Canonical(name string) string {
	if ns, ok := Lookup(name); ok {
		return ns.Name
	}
	return fold(name)
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
