package cli

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Names a namespace, an action and a flag may have. A namespace is the
// shape of a plugin's name, because a plugin's namespace is its name.
var (
	namespaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)
	actionRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	flagNameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// ValidNamespace and ValidAction report whether a name is the right shape.
// The plugin Manager checks a plugin's declarations with them before it
// starts the plugin.
func ValidNamespace(s string) bool { return namespaceRe.MatchString(s) }
func ValidAction(s string) bool    { return actionRe.MatchString(s) }

// PluginCommands is the plugin Manager, as the registry needs it: what the
// running plugins declare, and a way to run one. It is an interface so that
// cli stays a leaf — the plugin package depends on this one, not the other
// way round.
type PluginCommands interface {
	// Commands lists the commands the running plugins declare, each with
	// its Plugin set.
	Commands() []Command
	// RunCommand runs one of them, already parsed.
	RunCommand(ctx context.Context, c Command, in *Input) (Result, error)
}

// Registry is every command this process can run: the core commands, which
// are Go, and whatever the running plugins declare.
//
// Core commands are registered at startup by whoever owns the objects they
// act on (internal/app). Plugin commands are asked for each time, because a
// plugin can be enabled, disabled or reloaded between one invocation and
// the next.
type Registry struct {
	mu   sync.RWMutex
	core map[string][]Command // by canonical namespace, in registration order

	plugins PluginCommands
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{core: make(map[string][]Command)}
}

// Add registers core commands. A core command must be in a core namespace
// (namespaces.go): the table is the contract, so a command outside it would
// be a namespace nobody reserved.
func (r *Registry) Add(cmds ...Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range cmds {
		ns := Canonical(c.Namespace)
		switch {
		case !Reserved(ns):
			return usageErr("%q is not a core namespace: add it to cli.coreNamespaces first", c.Namespace)
		case !ValidAction(c.Action):
			return usageErr("%s: %q is not an action name", ns, c.Action)
		case c.Run == nil && !c.Local:
			return usageErr("%s %s has no Run", ns, c.Action)
		}
		if err := checkFlags(c); err != nil {
			return err
		}
		for _, have := range r.core[ns] {
			if have.Action == c.Action {
				return usageErr("%s %s is registered twice", ns, c.Action)
			}
		}
		c.Namespace, c.Plugin = ns, ""
		r.core[ns] = append(r.core[ns], c)
	}
	return nil
}

// SetPlugins points the registry at the plugin Manager (nil for a process
// with no plugins).
func (r *Registry) SetPlugins(p PluginCommands) {
	r.mu.Lock()
	r.plugins = p
	r.mu.Unlock()
}

// pluginCommands is what the plugins declare right now, with their
// namespaces folded and anything that strays into core left out. Core wins
// twice over: the plugin Manager refuses to start a plugin that declares a
// core namespace, and the registry would ignore it anyway.
func (r *Registry) pluginCommands() []Command {
	r.mu.RLock()
	p := r.plugins
	r.mu.RUnlock()
	if p == nil {
		return nil
	}
	var out []Command
	for _, c := range p.Commands() {
		ns := fold(c.Namespace)
		if Reserved(ns) {
			continue
		}
		c.Namespace = ns
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Action < out[j].Action
	})
	return out
}

// All is every command this process can run, core first.
func (r *Registry) All() []Command {
	r.mu.RLock()
	var core []Command
	for _, cmds := range r.core {
		core = append(core, cmds...)
	}
	r.mu.RUnlock()
	sort.SliceStable(core, func(i, j int) bool {
		if core[i].Namespace != core[j].Namespace {
			return core[i].Namespace < core[j].Namespace
		}
		return core[i].Action < core[j].Action
	})
	return append(core, r.pluginCommands()...)
}

// commandsIn returns the commands in one namespace, core before plugin.
func (r *Registry) commandsIn(ns string) []Command {
	r.mu.RLock()
	out := append([]Command(nil), r.core[ns]...)
	r.mu.RUnlock()
	for _, c := range r.pluginCommands() {
		if c.Namespace == ns {
			out = append(out, c)
		}
	}
	return out
}

// Run resolves a request and runs it. Help is answered here too, so the
// help a user sees always comes from the process that would run the
// command — plugin namespaces and all.
func (r *Registry) Run(ctx context.Context, req Request) (Result, error) {
	ns := Canonical(req.Namespace)
	if ns == "" {
		return Text("%s", r.overview()), nil
	}
	if ns == "help" {
		if req.Action == "" && req.JSON {
			// The whole command line as data, which is what the
			// interactive shell completes from.
			return Data(r.overview(), r.Catalogue())
		}
		// `reproxy help [namespace [action]]`.
		return r.Run(ctx, Request{Namespace: req.Action, Action: firstArg(req.Argv), Help: true, JSON: req.JSON})
	}
	if ns == "completion" {
		// Reserved for a completion script one day. Until then, say where
		// completion does exist rather than "no such namespace".
		return Text("Shell completion isn't written yet.\n" +
			"\"reproxy shell\" completes as you type: Tab for namespaces, actions and flags.\n"), nil
	}
	cmds := r.commandsIn(ns)
	if len(cmds) == 0 {
		// A reserved namespace with nothing in it yet is a different thing
		// from one that doesn't exist, and the user is owed the difference:
		// core is being built out, and the name is already spoken for.
		if core, ok := Lookup(ns); ok {
			return wrong(fmt.Sprintf("%s (%s) has no commands yet", core.Name, core.Summary), "")
		}
		return wrong(fmt.Sprintf("there is no %q namespace%s", req.Namespace, didYouMean(ns, r.namespaceNames())), "")
	}
	if req.Action == "" {
		return Text("%s", r.namespaceHelp(ns, cmds)), nil
	}
	var cmd Command
	found := false
	for _, c := range cmds {
		if c.Action == fold(req.Action) {
			cmd, found = c, true
			break
		}
	}
	if !found {
		actions := make([]string, len(cmds))
		for i, c := range cmds {
			actions[i] = c.Action
		}
		return wrong(fmt.Sprintf("%s has no %q action%s", ns, req.Action, didYouMean(fold(req.Action), actions)),
			r.namespaceHelp(ns, cmds))
	}
	if req.Help {
		return Text("%s", commandHelp(cmd)), nil
	}
	vals, err := ParseFlags(cmd, req.Argv)
	if err != nil {
		return wrong(Message(err), commandHelp(cmd))
	}
	in := &Input{Values: vals, Request: req}
	if cmd.Local {
		// The command line runs these itself; an instance was asked only
		// because something bypassed it.
		return Result{}, fmt.Errorf("%s %s runs in the command line, not in the app", ns, cmd.Action)
	}
	if cmd.Plugin != "" {
		r.mu.RLock()
		p := r.plugins
		r.mu.RUnlock()
		if p == nil {
			return Result{}, fmt.Errorf("%s %s: its plugin is no longer running", ns, cmd.Action)
		}
		return p.RunCommand(ctx, cmd, in)
	}
	return cmd.Run(ctx, in)
}

// wrong answers a command that was called wrongly: the complaint, the help
// that would have prevented it, and the exit status a command line uses for
// being used wrongly. It is not a Go error — nothing failed, the user typed
// something that doesn't exist — so it travels like any other answer.
func wrong(complaint, help string) (Result, error) {
	return Result{Error: complaint, Output: help, Code: UsageCode}, nil
}

// UsageCode is the exit status for a command that was called wrongly.
const UsageCode = 2

// namespaceNames is every namespace that has a command, for suggestions.
func (r *Registry) namespaceNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range r.All() {
		if !seen[c.Namespace] {
			seen[c.Namespace] = true
			out = append(out, c.Namespace)
		}
	}
	return out
}

func firstArg(argv []string) string {
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// didYouMean suggests the closest name, when one is close enough to be
// worth printing.
func didYouMean(typed string, names []string) string {
	best, bestDist := "", 0
	for _, n := range names {
		d := distance(typed, n)
		if d > len(n)/2+1 {
			continue
		}
		if best == "" || d < bestDist {
			best, bestDist = n, d
		}
	}
	if best == "" {
		return ""
	}
	return ". Did you mean " + best + "?"
}

// distance is the Levenshtein distance between two short words.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
