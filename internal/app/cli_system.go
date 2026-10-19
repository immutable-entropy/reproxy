package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reproxy/internal/browser"
	"reproxy/internal/cli"
	"reproxy/internal/config"
	"reproxy/internal/elide"
)

// The namespaces that are about the process rather than the work: settings
// (every setting, the proxy port and the upstream chain included), ca (the
// HTTPS interception certificate authority) and browser (the browsers
// reproxy launches through itself).
//
// Settings are named by their path through the settings file, so what the
// command line calls a setting is what the file calls it. Applying one goes
// through App.ApplySettings like the Settings window's Apply, which
// validates, moves the proxy if the port changed, and saves.

// ---- settings ---------------------------------------------------------------

func (a *App) settingsCommands() []cli.Command {
	engagementFlag := cli.Flag{Name: "engagement", Type: cli.FlagBool,
		Description: "the open engagement's own settings, rather than the global ones"}
	return []cli.Command{
		{
			Namespace: "settings", Action: "ls",
			Summary: "every setting and what it is set to",
			Description: "Names are the paths through the settings file. An engagement can override some of them; " +
				"--engagement lists those, where null means \"inherit the global setting\".",
			Flags: []cli.Flag{
				engagementFlag,
				{Name: "filter", Type: cli.FlagString, Placeholder: "text", Description: "keep the settings whose name matches"},
				{Name: "all", Type: cli.FlagBool, Description: "include the groups, not only the settings in them"},
			},
			Run: a.cmdSettingsList,
		},
		{
			Namespace: "settings", Action: "get",
			Summary: "one setting",
			Flags: []cli.Flag{
				{Name: "key", Type: cli.FlagString, Required: true, Placeholder: "name", Description: "the setting's name"},
				engagementFlag,
			},
			Run: a.cmdSettingsGet,
		},
		{
			Namespace: "settings", Action: "set",
			Summary: "change one setting",
			Description: "The value is read as JSON where it parses as JSON, and as text otherwise: " +
				"--value 8081, --value true, --value dark. Changing the proxy port moves the listener. " +
				"On an engagement setting, --value null gives it back to the global one.",
			Flags: []cli.Flag{
				{Name: "key", Type: cli.FlagString, Required: true, Placeholder: "name", Description: "the setting's name"},
				{Name: "value", Type: cli.FlagString, Required: true, Placeholder: "value", Description: "what to set it to"},
				engagementFlag,
			},
			Run: a.cmdSettingsSet,
		},
		{
			Namespace: "settings", Action: "path",
			Summary:     "where the settings are kept",
			Description: "The global file, the data directory, and the open engagement's own settings.",
			Run:         a.cmdSettingsPath,
		},
		{
			Namespace: "settings", Action: "effective",
			Summary:     "the settings actually in force",
			Description: "The global settings with the open engagement's overrides applied, which is what the proxy and the plugins act on.",
			Flags:       []cli.Flag{{Name: "filter", Type: cli.FlagString, Placeholder: "text", Description: "keep the settings whose name matches"}},
			Run:         a.cmdSettingsEffective,
		},
	}
}

// settingsSubject is the settings a command works on: the user's own, or
// the open engagement's overrides.
func (a *App) settingsSubject(in *cli.Input) (any, string, error) {
	if !in.Bool("engagement") {
		c := a.Settings()
		return &c, "", nil
	}
	e, name, ok := a.EngagementSettings()
	if !ok {
		return nil, "", fmt.Errorf("no engagement is open, so there are no engagement settings")
	}
	return &e, name, nil
}

func (a *App) cmdSettingsList(_ context.Context, in *cli.Input) (cli.Result, error) {
	subject, name, err := a.settingsSubject(in)
	if err != nil {
		return cli.Result{}, err
	}
	fields := filterFields(cli.Fields(subject), in.String("filter"), in.Bool("all"))
	rows := make([][]string, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, []string{f.Path, f.Kind, elide.Runes(f.Value, 60)})
	}
	empty := "No setting matches."
	res, err := listing([]string{"NAME", "KIND", "VALUE"}, rows, empty, fields)
	if err != nil || name == "" {
		return res, err
	}
	res.Output = fmt.Sprintf("%s's own settings (null means it inherits the global one):\n\n", name) + res.Output
	return res, nil
}

// filterFields keeps the settings a listing asked for: the leaves by
// default, since a group is only a heading.
func filterFields(fields []cli.Field, filter string, all bool) []cli.Field {
	filter = strings.ToLower(filter)
	out := make([]cli.Field, 0, len(fields))
	for _, f := range fields {
		if !all && !f.Leaf {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(f.Path), filter) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (a *App) cmdSettingsGet(_ context.Context, in *cli.Input) (cli.Result, error) {
	subject, _, err := a.settingsSubject(in)
	if err != nil {
		return cli.Result{}, err
	}
	key := in.String("key")
	f, ok := cli.FieldAt(subject, key)
	if !ok {
		return cli.Result{}, fmt.Errorf("there is no setting named %q; \"reproxy settings ls\" lists them", key)
	}
	return cli.Data(f.Value+"\n", f)
}

func (a *App) cmdSettingsSet(_ context.Context, in *cli.Input) (cli.Result, error) {
	key, value := in.String("key"), in.Text("value")
	if in.Bool("engagement") {
		e, name, ok := a.EngagementSettings()
		if !ok {
			return cli.Result{}, fmt.Errorf("no engagement is open, so there are no engagement settings")
		}
		if err := cli.SetField(&e, key, value); err != nil {
			return cli.Result{}, err
		}
		if err := a.ApplyEngagementSettings(e); err != nil {
			return cli.Result{}, err
		}
		f, _ := cli.FieldAt(&e, key)
		return cli.Data(fmt.Sprintf("%s = %s in %s.\n", f.Path, f.Value, name), f)
	}
	next := a.Settings()
	if err := cli.SetField(&next, key, value); err != nil {
		return cli.Result{}, err
	}
	if err := a.ApplySettings(next); err != nil {
		return cli.Result{}, err
	}
	f, _ := cli.FieldAt(&next, key)
	said := fmt.Sprintf("%s = %s\n", f.Path, f.Value)
	if _, _, ok := a.EngagementSettings(); ok {
		if eff, _ := cli.FieldAt(effectiveOf(a), f.Path); eff.Value != f.Value {
			said += fmt.Sprintf("The open engagement overrides it: %s is in force.\n", eff.Value)
		}
	}
	return cli.Data(said, f)
}

// effectiveOf is the settings in force, as something Fields can walk.
func effectiveOf(a *App) *config.Config {
	c := a.Effective()
	return &c
}

func (a *App) cmdSettingsEffective(_ context.Context, in *cli.Input) (cli.Result, error) {
	fields := filterFields(cli.Fields(effectiveOf(a)), in.String("filter"), false)
	rows := make([][]string, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, []string{f.Path, f.Kind, elide.Runes(f.Value, 60)})
	}
	return listing([]string{"NAME", "KIND", "VALUE"}, rows, "No setting matches.", fields)
}

func (a *App) cmdSettingsPath(_ context.Context, _ *cli.Input) (cli.Result, error) {
	paths := map[string]string{
		"settings": a.configPath,
		"data":     a.DataDir(),
		"secrets":  a.secrets.Path(),
		"targets":  a.targets.Path(),
	}
	rows := [][]string{
		{"settings", a.configPath},
		{"data", a.DataDir()},
		{"secrets", a.secrets.Path()},
		{"targets", a.targets.Path()},
	}
	if e, ok := a.engagements.Current(); ok {
		paths["engagement"] = e.Dir
		paths["engagement_settings"] = e.SettingsPath()
		paths["archive"] = e.CapturePath()
		rows = append(rows,
			[]string{"engagement", e.Dir},
			[]string{"engagement settings", e.SettingsPath()},
			[]string{"archive", e.CapturePath()})
	}
	return cli.Data(cli.Table([]string{"WHAT", "WHERE"}, rows), paths)
}

// ---- ca ---------------------------------------------------------------------

func (a *App) caCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "ca", Action: "show",
			Summary:     "the CA certificate and its fingerprint",
			Description: "Install it in a browser or a device to have reproxy's HTTPS interception trusted. Check the fingerprint before you do.",
			Flags:       []cli.Flag{{Name: "pem", Type: cli.FlagBool, Description: "print the certificate itself"}},
			Run:         a.cmdCAShow,
		},
		{
			Namespace: "ca", Action: "export",
			Summary:     "write the CA certificate to a file",
			Description: "Give an absolute path. The file is the certificate alone; the key never leaves the data directory.",
			Flags: []cli.Flag{{Name: "to", Type: cli.FlagString, Required: true, Placeholder: "path",
				Description: "where to write the PEM"}},
			Run: a.cmdCAExport,
		},
		{
			Namespace: "ca", Action: "path",
			Summary: "where the CA's files are kept", Run: a.cmdCAPath,
		},
	}
}

func (a *App) cmdCAShow(_ context.Context, in *cli.Input) (cli.Result, error) {
	pem := string(a.CACertPEM())
	info := map[string]string{"fingerprint": a.CAFingerprint(), "pem": pem}
	if in.Bool("pem") {
		return cli.Data(pem, info)
	}
	return cli.Data(lines(
		"SHA-256 "+a.CAFingerprint(),
		"--pem prints the certificate; \"reproxy ca export --to <path>\" writes it out.",
	), info)
}

func (a *App) cmdCAExport(_ context.Context, in *cli.Input) (cli.Result, error) {
	path := in.String("to")
	if !filepath.IsAbs(path) {
		return cli.Result{}, fmt.Errorf("--to needs an absolute path: %q would be written beside this process, not beside you", path)
	}
	if _, err := os.Stat(path); err == nil {
		return cli.Result{}, fmt.Errorf("%s already exists", path)
	}
	if err := os.WriteFile(path, a.CACertPEM(), 0o644); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Wrote %s (SHA-256 %s).\n", path, a.CAFingerprint()), nil
}

func (a *App) cmdCAPath(_ context.Context, _ *cli.Input) (cli.Result, error) {
	dir := a.DataDir()
	return cli.Data(lines(dir), map[string]string{"dir": dir})
}

// ---- browser ----------------------------------------------------------------

func (a *App) browserCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "browser", Action: "ls",
			Summary:     "the browsers reproxy can launch",
			Description: "A launched browser uses a profile of its own, is pointed at this instance's proxy, and resolves no names itself.",
			Run:         a.cmdBrowserList,
		},
		{
			Namespace: "browser", Action: "launch",
			Summary:     "start a browser pointed at this instance",
			Description: "It uses the browser chosen in Settings → Browser. Its traffic arrives in the request list at once.",
			Run:         a.cmdBrowserLaunch,
		},
	}
}

func (a *App) cmdBrowserList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	list := a.InstalledBrowsers()
	chosen := a.Effective().Browser
	rows := make([][]string, 0, len(list))
	for _, b := range list {
		mark := ""
		if chosen.Path == b.Path || (chosen.Path == "" && len(rows) == 0) {
			mark = "chosen"
		}
		rows = append(rows, []string{browserKind(b.Kind), b.Name, mark, elide.Runes(b.Path, 60)})
	}
	return listing([]string{"KIND", "NAME", "", "PATH"}, rows,
		"No browser was found on this machine.", list)
}

func (a *App) cmdBrowserLaunch(_ context.Context, _ *cli.Input) (cli.Result, error) {
	s, err := a.LaunchBrowser()
	if err != nil {
		return cli.Result{}, err
	}
	addr, _ := a.ProxyStatus()
	said := []string{fmt.Sprintf("Launched, pointed at %s.", addr)}
	if w := s.Warning(); w != "" {
		said = append(said, w)
	}
	return cli.Data(lines(said...), map[string]string{"proxy": addr, "warning": s.Warning()})
}

// browserKind names the two families the browser package knows, since the
// kind is an enum without a name of its own.
func browserKind(k browser.Kind) string {
	if k == browser.Firefox {
		return "firefox"
	}
	return "chromium"
}
