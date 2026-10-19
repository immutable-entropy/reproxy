package app

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"reproxy/internal/cli"
	"reproxy/internal/instance"
)

// The command line (internal/cli) is wired here, where the objects its
// commands act on live. Core commands are Go and are registered below;
// plugin commands come from the plugin Manager, which the registry asks
// each time, so enabling a plugin adds its namespace at once.
//
// The commands themselves are in the cli_*.go files beside this one, one
// per area. They are adapters and nothing more: each one reads its flags,
// calls the same method the window or an agent would call, and renders the
// answer. Anything that had to be worked out belongs in the package that
// owns the object, not here — if a command needs something core can't
// already do, the thing to add is a method on that package.
//
// Nothing here is on the proxy's path: a command runs when the user types
// one.

// newCommandRegistry builds this process's command line. A command that
// won't register is a mistake in its declaration, not something the user
// can cause, so it is logged and the rest of the command line still works.
func (a *App) newCommandRegistry() *cli.Registry {
	reg := cli.NewRegistry()
	reg.SetPlugins(a.plugins)
	sets := [][]cli.Command{
		a.flowCommands(),
		a.interceptCommands(),
		a.replayCommands(),
		a.websocketCommands(),
		a.dnsCommands(),
		a.targetCommands(),
		a.engagementCommands(),
		a.crawlCommands(),
		a.entityCommands(),
		a.findingCommands(),
		a.fileCommands(),
		a.pluginCommands(),
		a.agentCommands(),
		a.approvalCommands(),
		a.mcpCommands(),
		a.focusCommands(),
		a.instanceCommands(),
		a.settingsCommands(),
		a.caCommands(),
		a.browserCommands(),
		cli.LocalCommands(),
	}
	for _, set := range sets {
		if err := reg.Add(set...); err != nil {
			a.log.Error("a command could not be registered", "err", err)
		}
	}
	return reg
}

// resolveInstance finds the instance a command names, for cli.HubHandler.
// An empty id means this process, which is the hub (only the hub serves
// that route).
func (a *App) resolveInstance(id string) (cli.Runner, string, error) {
	node := a.Node()
	if node == nil {
		return nil, id, errors.New("this process has not joined the reproxy group")
	}
	router := node.Router()
	if id == "" {
		id = router.LocalID()
	}
	inst, ok := router.Get(id)
	if !ok {
		return nil, id, fmt.Errorf("no instance %q is reachable; try \"reproxy instances ls\"", id)
	}
	return inst, id, nil
}

// ---- What the commands share -----------------------------------------------

// self is this process as an instance: how the commands read traffic,
// interception and Replay, so they go through the same code an agent does.
func (a *App) self() (*instance.Local, error) {
	a.mu.Lock()
	local := a.local
	a.mu.Unlock()
	if local == nil {
		return nil, errors.New("this instance is still starting up")
	}
	return local, nil
}

// engagementOpen returns the open engagement, or an error naming what the
// command needed it for. The stores that live in an engagement's capture.db
// (findings, the Explorer, the crawler) answer nothing without one.
func (a *App) engagementOpen(what string) error {
	if _, ok := a.engagements.Current(); ok {
		return nil
	}
	return fmt.Errorf("no engagement is open, so there are no %s: \"reproxy engagements open --dir <path>\"", what)
}

// Flags used by more than one namespace, so they read the same everywhere.
func flowFlag(what string) cli.Flag {
	return cli.Flag{Name: "flow", Type: cli.FlagNumber, Required: true, Placeholder: "id",
		Description: "the flow id " + what}
}

func limitFlag(def string) cli.Flag {
	return cli.Flag{Name: "limit", Type: cli.FlagNumber, Default: def, Placeholder: "n",
		Description: "how many to list at most"}
}

func yesFlag(what string) cli.Flag {
	return cli.Flag{Name: "yes", Type: cli.FlagBool,
		Description: "go ahead without asking: " + what}
}

// confirmed refuses a command that would destroy something unless --yes was
// given. There is nobody to prompt: a command line answers once.
func confirmed(in *cli.Input, what string) error {
	if in.Bool("yes") {
		return nil
	}
	return fmt.Errorf("%s. Pass --yes if that is what you want", what)
}

// openInput is the file a command was given, or what was piped into the
// command line when the flag is "-". A path has to be absolute: the command
// runs in this process, which is not where the user typed it, so a relative
// path would be read against the wrong directory.
func openInput(in *cli.Input, flag string) (io.Reader, func(), error) {
	path := strings.TrimSpace(in.String(flag))
	if path == "-" {
		if in.Piped() == "" {
			return nil, nil, fmt.Errorf("--%s was given \"-\" but nothing was piped in", flag)
		}
		return strings.NewReader(in.Piped()), func() {}, nil
	}
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("--%s needs an absolute path, or \"-\" to read what is piped in: "+
			"%q would be read against this process's directory, not yours", flag, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

// targetOf is the target a URL falls under, or "" — what an entity is
// tagged with when a command puts it in the Explorer.
func (a *App) targetOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	m, ok := a.targets.Set().MatchHost(host)
	if !ok {
		return ""
	}
	return m.ID
}

// flowsByID is the query for "the requests behind this thing": the IDs an
// entity or a finding recorded, newest first.
func flowsByID(ids []uint64, limit int) instance.FlowQuery {
	return instance.FlowQuery{IDs: ids, Limit: limit, Newest: true}
}

// ---- Rendering --------------------------------------------------------------

// listing renders rows as a table, or says there were none. The JSON form
// is the value itself, so --json is always the whole record and never the
// table's abbreviations.
func listing(header []string, rows [][]string, empty string, v any) (cli.Result, error) {
	if len(rows) == 0 {
		return cli.Data(empty+"\n", v)
	}
	return cli.Data(cli.Table(header, rows), v)
}

// since is how long ago t was, in the one unit that reads best.
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// yesNo is how a boolean reads in a table.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// count is "1 thing" or "3 things".
func count(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// lines joins what a command has to say, one statement per line.
func lines(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// ---- The shells in the Sessions panel ---------------------------------------

// terminalEnv is what the Sessions panel's shells get on top of this
// process's own environment, so that reproxy's command line works in them
// with nothing installed and nothing to configure:
//
//   - the running binary's directory is first on PATH, so `reproxy` is the
//     one the user is looking at, whether or not a copy is installed system
//     wide (and whichever copy that is);
//   - REPROXY_INSTANCE names this process, so a command typed in its window
//     acts on it rather than on whichever process holds the hub role;
//   - REPROXY_DATA_DIR names the directory this process is using, which is
//     not the default one when it was started with -config.
//
// It is worked out once, when the Manager is built: the binary does not
// move while it runs.
func (a *App) terminalEnv() []string {
	env := []string{
		"REPROXY_INSTANCE=" + a.instanceID,
		"REPROXY_DATA_DIR=" + a.DataDir(),
	}
	dir, err := executableDir()
	if err != nil {
		// Not fatal: the shells still work, `reproxy` just has to be found
		// the usual way.
		a.log.Warn("terminals: couldn't find this binary, so reproxy may not be on their PATH", "err", err)
		return env
	}
	if name := commandName(); !hasCommand(dir, name) {
		a.log.Debug("terminals: this binary is not named reproxy, so PATH may not offer that command",
			"dir", dir, "want", name)
	}
	return append(env, "PATH="+prependPath(dir, os.Getenv("PATH")))
}

// executableDir is the directory the running reproxy is in, with symlinks
// resolved so a link from /usr/local/bin doesn't put the wrong directory on
// the PATH.
func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// commandName is what has to be in that directory for the promise to hold.
func commandName() string {
	if runtime.GOOS == "windows" {
		return "reproxy.exe"
	}
	return "reproxy"
}

func hasCommand(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}

// prependPath puts dir first in a PATH list, so the reproxy the user is
// running wins over any other copy. A directory already in the list is left
// where it is rather than listed twice.
func prependPath(dir, path string) string {
	for _, p := range filepath.SplitList(path) {
		if p != "" && samePath(p, dir) {
			return path
		}
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}

// samePath compares two directories the way the platform does.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
