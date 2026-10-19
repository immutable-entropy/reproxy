package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dop251/goja"

	"reproxy/internal/cli"
	"reproxy/internal/plugin/manifest"
)

// Commands are what a plugin adds to reproxy's command line, under its own
// name: everything the wordpress plugin offers is `reproxy wordpress
// <action>`. Like actions, nothing calls one but the user: core parses the
// flags the plugin declared, then calls its function once, on the plugin's
// own goroutine, with the values. Traffic never comes near this.
//
// Because the namespace is the plugin's name, and two plugins can't share
// a name, the only thing to check is core: a plugin named for a core
// namespace (cli.Reserved) may not declare commands, and doesn't run at all
// rather than running with its commands missing — a command line that
// silently does something other than what it says is worse than one that
// refuses.

// commandBudget bounds a whole command: its turns and the waits between
// them. It matches an action's, since a command is as likely to send
// requests and wait for them.
const commandBudget = actionBudget

// commandHook names commands in a plugin's log and error counts.
const commandHook manifest.Hook = "command"

// Commands implements cli.PluginCommands: what the running plugins declare,
// by plugin name and then in each manifest's order.
func (m *Manager) Commands() []cli.Command {
	var out []cli.Command
	for _, r := range m.runningPlugins() {
		if r.man == nil {
			continue
		}
		if cli.Reserved(r.src.Name) {
			continue // refused at start; never offered
		}
		for _, c := range r.man.Commands {
			out = append(out, toCLICommand(r.src.Name, c))
		}
	}
	return out
}

// RunCommand implements cli.PluginCommands: it runs one plugin's command
// and waits for it. It is refused if the plugin has stopped since the
// command was listed.
func (m *Manager) RunCommand(_ context.Context, c cli.Command, in *cli.Input) (cli.Result, error) {
	var run *Running
	for _, r := range m.runningPlugins() {
		if r.src.Name == c.Plugin {
			run = r
		}
	}
	if run == nil {
		return cli.Result{}, fmt.Errorf("%s isn't running", c.Plugin)
	}
	decl, ok := run.command(c.Action)
	if !ok {
		return cli.Result{}, fmt.Errorf("%s no longer has a %q command", c.Plugin, c.Action)
	}
	return run.runCommand(decl, in)
}

// toCLICommand describes a manifest command the way the command line's
// registry wants it.
func toCLICommand(plugin string, c manifest.Command) cli.Command {
	out := cli.Command{
		Namespace:   plugin,
		Action:      c.Action,
		Summary:     strings.TrimSpace(c.Summary),
		Description: c.Description,
		Args:        c.Args,
		Plugin:      plugin,
	}
	for i := range c.Flags {
		f := &c.Flags[i]
		out.Flags = append(out.Flags, cli.Flag{
			Name:        f.Name,
			Type:        cli.FlagType(f.Type),
			Description: f.Description,
			Required:    f.Required,
			Default:     f.DefaultString(),
			Placeholder: f.Placeholder,
		})
	}
	return out
}

func (r *Running) command(action string) (manifest.Command, bool) {
	if r.man == nil {
		return manifest.Command{}, false
	}
	for _, c := range r.man.Commands {
		if c.Action == action {
			return c, true
		}
	}
	return manifest.Command{}, false
}

// runCommand calls a command's function with { hook, namespace, action,
// flags, args } and waits for it. The function may return (or resolve with)
// a string to print, or an object with output, json and code.
func (r *Running) runCommand(c manifest.Command, in *cli.Input) (cli.Result, error) {
	r.logf(slog.LevelInfo, "command "+c.Action)
	v, err := r.awaitFor(commandHook, c.Function, 0, turnBudget, commandBudget, func(vm *goja.Runtime) goja.Value {
		return commandEvent(vm, c, in)
	})
	if err != nil {
		return cli.Result{}, errors.New(firstLine(err.Error()))
	}
	return commandResult(v)
}

// commandEvent is what a command's function is called with.
func commandEvent(vm *goja.Runtime, c manifest.Command, in *cli.Input) goja.Value {
	o := vm.NewObject()
	o.Set("hook", string(commandHook))
	o.Set("namespace", in.Request.Namespace)
	o.Set("action", c.Action)
	o.Set("flags", vm.ToValue(in.Flags()))
	o.Set("args", vm.ToValue(append([]string{}, in.Args()...)))
	o.Set("json", in.Request.JSON)
	return o
}

// commandResult reads what a command returned.
func commandResult(v any) (cli.Result, error) {
	switch x := v.(type) {
	case nil:
		return cli.Result{}, nil
	case string:
		return cli.Result{Output: x}, nil
	case map[string]any:
		var res cli.Result
		if s, ok := x["output"].(string); ok {
			res.Output = s
		}
		if n, ok := x["code"].(float64); ok {
			res.Code = int(n)
		}
		if data, ok := x["json"]; ok && data != nil {
			b, err := json.Marshal(data)
			if err != nil {
				return cli.Result{}, fmt.Errorf("its json can't be encoded: %w", err)
			}
			res.JSON = b
		}
		return res, nil
	}
	// A number, a boolean or anything else: print it as it reads.
	return cli.Result{Output: fmt.Sprint(v)}, nil
}

// checkNamespace refuses a plugin whose commands would answer to a name
// that is core's. The namespace is the plugin's name, so there is nothing
// to hand out and nothing to order: either the name is core's or it isn't.
func checkNamespace(man *manifest.Manifest) error {
	ns := man.Namespace()
	if ns == "" || !cli.Reserved(ns) {
		return nil
	}
	return fmt.Errorf("a plugin's commands answer to its own name, and %q belongs to core: rename the plugin or drop its commands", ns)
}
