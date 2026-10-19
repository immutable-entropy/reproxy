package app

import (
	"context"
	"fmt"
	"strings"

	"reproxy/internal/cli"
	"reproxy/internal/config"
	"reproxy/internal/elide"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/manifest"
)

// The plugins namespace: the plugins installed in this instance, where they
// come from, and the tools for writing one. The author tools (validate,
// test, types) are declared in internal/cli and run in the command line
// itself; everything here acts on the running plugin Manager.

// ---- plugins ---------------------------------------------------------------

func (a *App) pluginCommands() []cli.Command {
	name := cli.Flag{
		Name: "name", Type: cli.FlagString, Required: true,
		Placeholder: "plugin", Description: "the plugin's name, as \"plugins ls\" lists it",
	}
	return []cli.Command{
		{
			Namespace: "plugins", Action: "ls",
			Summary: "list the plugins installed here",
			Description: "A plugin is running only if it is enabled and nothing is wrong with it; " +
				"anything that stopped it shows in the last column. " +
				"The plugins directory is read again first, so a plugin copied in by hand is listed.",
			Run: a.cmdPluginsList,
		},
		{
			Namespace: "plugins", Action: "enable",
			Summary: "switch a plugin on",
			Description: "The plugin starts at once, and stays enabled across restarts. " +
				"It is refused if the plugin wants a command namespace core or another plugin already has.",
			Flags: []cli.Flag{name},
			Run:   a.cmdPluginsEnable,
		},
		{
			Namespace: "plugins", Action: "disable",
			Summary: "switch a plugin off",
			Flags:   []cli.Flag{name},
			Run:     a.cmdPluginsDisable,
		},
		{
			Namespace: "plugins", Action: "reload",
			Summary:     "load a plugin again from its files",
			Description: "This is what clears a fault, and what picks up changes while a plugin is being written.",
			Flags:       []cli.Flag{name},
			Run:         a.cmdPluginsReload,
		},
	}
}

// seePlugins looks at the plugins directory again before a command acts on
// it, the way the Plugins window does each time it opens. The command line
// is the moment the user looks, and a plugin copied into the directory by
// hand should be there when they do.
func (a *App) seePlugins() {
	a.plugins.Rescan()
	a.applyPlugins()
}

// pluginRow is one plugin in the listing, and what --json answers with.
type pluginRow struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Status  string `json:"status"`
	State   string `json:"state"`
	// Commands are the actions the plugin adds under its own name, which
	// is its command-line namespace.
	Commands []string `json:"commands,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

func (a *App) cmdPluginsList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	a.seePlugins()
	infos := a.plugins.List()
	out := make([]pluginRow, 0, len(infos))
	rows := make([][]string, 0, len(infos))
	for _, in := range infos {
		r := pluginRow{Name: in.Name, Status: string(in.Status), State: string(in.State), Problems: in.Problems}
		if in.Manifest != nil {
			r.Version = in.Manifest.Version
			r.Commands = actionsOf(in)
		}
		out = append(out, r)
		rows = append(rows, []string{
			r.Name, r.Version, r.State, strings.Join(r.Commands, " "),
			elide.Runes(strings.Join(r.Problems, "; "), 60),
		})
	}
	if len(rows) == 0 {
		return cli.Data("No plugins are installed.\n", out)
	}
	return cli.Data(cli.Table([]string{"NAME", "VERSION", "STATE", "COMMANDS", "PROBLEM"}, rows), out)
}

func (a *App) cmdPluginsEnable(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setPluginStatus(in.String("name"), manifest.Enabled)
}

func (a *App) cmdPluginsDisable(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setPluginStatus(in.String("name"), manifest.Disabled)
}

// setPluginStatus writes the user's choice and waits for the plugins to be
// brought into line, so what the command reports back is what actually
// happened rather than what was asked for.
func (a *App) setPluginStatus(name string, status manifest.Status) (cli.Result, error) {
	a.seePlugins()
	if _, ok := a.plugins.Get(name); !ok {
		return cli.Result{}, fmt.Errorf("no plugin %q is installed here", name)
	}
	next := a.Settings()
	over := map[string]config.PluginOverride{}
	for k, v := range next.Plugins.Overrides {
		over[k] = v
	}
	o := over[name]
	o.Status = string(status)
	over[name] = o
	next.Plugins.Overrides = over
	if err := a.ApplySettings(next); err != nil {
		return cli.Result{}, err
	}
	a.applyPlugins() // ApplySettings does this in the background; this waits
	return a.pluginState(name)
}

func (a *App) cmdPluginsReload(_ context.Context, in *cli.Input) (cli.Result, error) {
	name := in.String("name")
	a.seePlugins()
	if _, ok := a.plugins.Get(name); !ok {
		return cli.Result{}, fmt.Errorf("no plugin %q is installed here", name)
	}
	if err := a.plugins.Reload(name); err != nil {
		return cli.Result{}, err
	}
	return a.pluginState(name)
}

// pluginState describes where a plugin ended up, for the commands that move
// it. A plugin that was asked to run and didn't says why.
func (a *App) pluginState(name string) (cli.Result, error) {
	info, ok := a.plugins.Get(name)
	if !ok {
		return cli.Result{}, fmt.Errorf("no plugin %q is installed here", name)
	}
	line := fmt.Sprintf("%s is %s", name, info.State)
	if len(info.Problems) > 0 {
		line += ": " + strings.Join(info.Problems, "; ")
	}
	if info.State == plugin.StateRunning {
		if acts := actionsOf(info); len(acts) > 0 {
			line += fmt.Sprintf(" (commands: reproxy %s %s)", name, strings.Join(acts, "|"))
		}
	}
	res, err := cli.Data(line+"\n", pluginRow{
		Name: info.Name, Status: string(info.Status), State: string(info.State),
		Commands: actionsOf(info), Problems: info.Problems,
	})
	if err != nil {
		return res, err
	}
	// A plugin that can't run is not a successful command.
	if info.State == plugin.StateError {
		res.Error, res.Output, res.Code = line, "", 1
	}
	return res, nil
}

// actionsOf are the command-line actions a plugin adds. They all live
// under the plugin's own name, which is its namespace.
func actionsOf(info plugin.Info) []string {
	if info.Manifest == nil {
		return nil
	}
	out := make([]string, 0, len(info.Manifest.Commands))
	for _, c := range info.Manifest.Commands {
		out = append(out, c.Action)
	}
	return out
}
