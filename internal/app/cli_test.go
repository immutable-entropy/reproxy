package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
)

// The commands are adapters: each one reads its flags, calls the method the
// window or an agent would call, and renders the answer. These tests go in
// through the registry, exactly as the hub route does, so what is tested is
// the whole path — the declaration, the parsing and the command — rather
// than the function in the middle of it.

// cliApp is a started app with an engagement open, and a way to run
// commands against it.
func cliApp(t *testing.T) (*App, *cli.Registry) {
	t.Helper()
	a, _ := newApp(t, freePort(t))
	a.Start()
	dir := filepath.Join(t.TempDir(), "engagement")
	if _, err := a.engagements.Create(dir, "Test", "", nil); err != nil {
		t.Fatal(err)
	}
	return a, a.newCommandRegistry()
}

// run resolves and runs one command line, failing the test if it could not
// be run at all. A command that answers with a complaint (a flag it doesn't
// take, a thing that isn't there) comes back in the Result, like it does
// for the user.
func run(t *testing.T, reg *cli.Registry, args ...string) cli.Result {
	t.Helper()
	req, _, err := cli.Split(args)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	res, err := reg.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return res
}

// runErr is run for the commands expected to fail.
func runErr(t *testing.T, reg *cli.Registry, args ...string) error {
	t.Helper()
	req, _, err := cli.Split(args)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.Run(context.Background(), req)
	if err == nil {
		t.Fatalf("%v was expected to fail", args)
	}
	return err
}

func contains(t *testing.T, res cli.Result, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(res.Output, w) {
			t.Errorf("output has no %q:\n%s", w, res.Output)
		}
	}
}

// Every command core offers is declared in a reserved namespace, takes
// flags that parse, and has help that can be rendered without running it.
func TestEveryCommandIsWellFormed(t *testing.T) {
	_, reg := cliApp(t)
	all := reg.All()
	if len(all) < 50 {
		t.Fatalf("only %d commands are registered", len(all))
	}
	for _, c := range all {
		if !cli.Reserved(c.Namespace) {
			t.Errorf("%s %s is not in a core namespace", c.Namespace, c.Action)
		}
		if c.Summary == "" {
			t.Errorf("%s %s has no summary", c.Namespace, c.Action)
		}
		res := run(t, reg, c.Namespace, c.Action, "--help")
		if !strings.Contains(res.Output, "Usage:") {
			t.Errorf("%s %s has no usage:\n%s", c.Namespace, c.Action, res.Output)
		}
		for _, f := range c.Flags {
			if f.Description == "" {
				t.Errorf("%s %s: --%s has no description", c.Namespace, c.Action, f.Name)
			}
		}
	}
}

func TestFlowCommands(t *testing.T) {
	a, reg := cliApp(t)
	raw := "GET /users/1 HTTP/1.1\r\nHost: app.test\r\n\r\n"
	id := a.store.Add(capture.Flow{
		Method: "GET", Host: "app.test", Path: "/users/1",
		Request: capture.Message{Raw: []byte(raw), Size: int64(len(raw))},
	})
	a.store.Complete(id, capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")}, 200, "200 OK", time.Now())

	contains(t, run(t, reg, "flows", "ls"), "app.test/users/1", "GET")
	contains(t, run(t, reg, "flows", "ls", "--filter", "nothing-matches-this"), "No request matches")
	contains(t, run(t, reg, "flows", "get", "--flow", "1"), "--- request ---", "Host: app.test", "hi")
	contains(t, run(t, reg, "flows", "get", "--flow", "1", "--part", "request"), "--- request ---")
	if res := run(t, reg, "flows", "get", "--flow", "1", "--part", "response"); strings.Contains(res.Output, "--- request ---") {
		t.Error("--part response showed the request")
	}

	// The JSON form is the whole record, not the table's abbreviations.
	res := run(t, reg, "flows", "ls", "--json")
	var page struct {
		Flows []struct {
			URL string `json:"url"`
		} `json:"flows"`
	}
	if err := json.Unmarshal(res.JSON, &page); err != nil {
		t.Fatalf("--json: %v (%s)", err, res.JSON)
	}
	if len(page.Flows) != 1 || page.Flows[0].URL != "http://app.test/users/1" {
		t.Errorf("--json gave %+v", page.Flows)
	}

	// Clearing is refused without --yes, and keeps the findings.
	if err := runErr(t, reg, "flows", "clear"); !strings.Contains(err.Error(), "--yes") {
		t.Errorf("clear without --yes says %q", err)
	}
	run(t, reg, "findings", "new", "--value", "secret", "--notes", "kept")
	contains(t, run(t, reg, "flows", "clear", "--yes"), "Cleared 1 request")
	contains(t, run(t, reg, "flows", "ls"), "No request matches")
	contains(t, run(t, reg, "findings", "ls"), "secret")
}

func TestInterceptCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "intercept", "status"), "Interception is off")
	contains(t, run(t, reg, "intercept", "on"), "Interception is on")
	if !a.intercept.Enabled() {
		t.Error("intercept on didn't reach the interceptor")
	}
	contains(t, run(t, reg, "intercept", "held"), "Nothing is held")
	contains(t, run(t, reg, "intercept", "off"), "Interception is off")
	if err := runErr(t, reg, "intercept", "forward", "--flow", "9"); !strings.Contains(err.Error(), "not held") {
		t.Errorf("forwarding what isn't held says %q", err)
	}
}

func TestTargetCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "targets", "ls"), "Nothing is in scope")
	contains(t, run(t, reg, "targets", "add", "--value", "app.test", "--name", "App"), "Added domain app.test")
	id := a.targets.List()[0].ID

	contains(t, run(t, reg, "targets", "intercept", "--id", id, "--responses", "on"), "requests+responses")
	if !a.targets.List()[0].Intercept.Responses {
		t.Error("the target didn't keep what it holds")
	}
	if err := runErr(t, reg, "targets", "intercept", "--id", id, "--responses", "maybe"); !strings.Contains(err.Error(), "on or off") {
		t.Errorf("a bad switch says %q", err)
	}
	contains(t, run(t, reg, "targets", "disable", "--id", id), "is disabled")
	if a.targets.List()[0].Enabled {
		t.Error("disable didn't save")
	}
	contains(t, run(t, reg, "targets", "enable", "--id", id), "is enabled")

	contains(t, run(t, reg, "targets", "suppress", "--host", "cdn.test,ads.test"), "Suppressed 2 hosts")
	contains(t, run(t, reg, "targets", "suppressed"), "cdn.test", "ads.test")
	contains(t, run(t, reg, "targets", "unsuppress", "--host", "ads.test"), "Removed 1 pattern")
	if res := run(t, reg, "targets", "suppressed"); strings.Contains(res.Output, "ads.test") {
		t.Error("unsuppress didn't save")
	}
	contains(t, run(t, reg, "targets", "rm", "--id", id), "Removed")
	if len(a.targets.List()) != 0 {
		t.Error("rm didn't save")
	}
}

// Importing a scope file reads what was piped in, since the file is the
// user's and the command runs in another process.
func TestTargetsImportFromPipe(t *testing.T) {
	_, reg := cliApp(t)
	scope := `{"target":{"scope":{"advanced_mode":true,"include":[{"enabled":true,"host":"^app\\.test$","protocol":"any"}]}}}`
	req, _, _ := cli.Split([]string{"targets", "import", "--file", "-", "--yes"})
	req.Stdin = scope
	res, err := reg.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, res, "Added 1 target")

	// Without --yes it says what it would add rather than adding it.
	req.Stdin = scope
	req.Argv = []string{"--file", "-"}
	if _, err := reg.Run(context.Background(), req); err == nil {
		t.Error("an import went ahead without --yes")
	}
}

func TestFindingCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "findings", "ls"), "Nothing has been found")
	contains(t, run(t, reg, "findings", "new", "--value", "tok_123", "--notes", "in the body"), "Recorded finding #1")
	// The same value merges rather than being recorded twice.
	contains(t, run(t, reg, "findings", "new", "--value", "tok_123", "--notes", "again"), "Merged into finding #1")
	if n := a.findings.Len(); n != 1 {
		t.Fatalf("%d findings, want 1", n)
	}

	contains(t, run(t, reg, "findings", "confidence", "--id", "1", "--score", "80"), "scored 80")
	contains(t, run(t, reg, "findings", "kv", "--id", "1", "--name", "where", "--value", "home"), "where = home")
	contains(t, run(t, reg, "findings", "hide", "--id", "1"), "masked")
	if res := run(t, reg, "findings", "ls"); strings.Contains(res.Output, "tok_123") {
		t.Errorf("a hidden value was printed:\n%s", res.Output)
	}
	contains(t, run(t, reg, "findings", "ls", "--reveal"), "tok_123")
	contains(t, run(t, reg, "findings", "show", "--id", "1"), "shown in full")
	contains(t, run(t, reg, "findings", "get", "--id", "1"), "tok_123", "in the body", "where = home")

	// A type it doesn't know is refused by name, not silently dropped.
	if err := runErr(t, reg, "findings", "classify", "--id", "1", "--type", "nonsense"); !strings.Contains(err.Error(), "no finding type") {
		t.Errorf("an unknown type says %q", err)
	}
	if len(a.findings.Types()) > 0 {
		name := a.findings.Types()[0].Name
		contains(t, run(t, reg, "findings", "classify", "--id", "1", "--type", name), name)
	}
	contains(t, run(t, reg, "findings", "rm", "--id", "1"), "Deleted finding #1")
	if a.findings.Len() != 0 {
		t.Error("rm didn't delete")
	}
}

func TestFileCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "files", "ls"), "Nothing has been recovered")

	// A response carrying a source map is recovered from the command line
	// the same way the traffic list's menu item does it.
	mapJSON := `{"version":3,"file":"app.js","mappings":"AAAA",` +
		`"sources":["../../src/main.ts","../../node_modules/vue/dist/vue.mjs"],` +
		`"sourcesContent":["const token = 'tok_123'","export default {}"]}`
	id := a.Store().Add(capture.Flow{
		Method: "GET", Host: "app.test", Path: "/assets/app.js.map",
		Response: capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + mapJSON)},
	})
	res := run(t, reg, "files", "recover", "--flow", strconv.FormatUint(id, 10))
	contains(t, res, "Recovered 2 files (1 the app's own)", "recovery ")
	if n := a.files.Len(); n != 2 {
		t.Fatalf("%d recovered files, want 2", n)
	}

	contains(t, run(t, reg, "files", "ls"), "app.test/app.js/src/main.ts", "text/typescript", "source-map")
	contains(t, run(t, reg, "files", "ls", "--kind", "source-map"), "src/main.ts")
	if out := run(t, reg, "files", "ls", "--kind", "source-map"); strings.Contains(out.Output, "node_modules") {
		t.Errorf("a dependency was listed as the app's own source:\n%s", out.Output)
	}
	contains(t, run(t, reg, "files", "batches"), "http://app.test/assets/app.js.map")

	// By path or by id, and the bytes come back from disk.
	contains(t, run(t, reg, "files", "get", "--path", "app.test/app.js/src/main.ts"),
		"text/typescript", "recovered by user", "sha256", "GET http://app.test/assets/app.js.map")
	contains(t, run(t, reg, "files", "cat", "--id", "1"), "tok_123")
	contains(t, run(t, reg, "files", "notes", "--id", "1", "--text", "has a token"), "Notes set")
	contains(t, run(t, reg, "files", "get", "--id", "1"), "has a token")

	// Deleting needs --yes, and takes the bytes with the record.
	if err := runErr(t, reg, "files", "rm", "--id", "1"); !strings.Contains(err.Error(), "--yes") {
		t.Errorf("rm without --yes says %q", err)
	}
	onDisk := filepath.Join(a.files.Root(), filepath.FromSlash("app.test/app.js/src/main.ts"))
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("the recovered file isn't on disk: %v", err)
	}
	contains(t, run(t, reg, "files", "rm", "--id", "1", "--yes"), "Deleted")
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Errorf("the bytes survived rm: %v", err)
	}
	contains(t, run(t, reg, "files", "clear", "--yes"), "Deleted 1 recovered file")
	if a.files.Len() != 0 {
		t.Error("clear didn't empty the store")
	}
	if err := runErr(t, reg, "files", "get", "--id", "9"); !strings.Contains(err.Error(), "files ls") {
		t.Errorf("a missing file says %q", err)
	}
}

// A finding scored below the floor is noise, and stays out of the listing
// until it is asked for.
func TestFindingsBelowTheFloorAreHidden(t *testing.T) {
	_, reg := cliApp(t)
	run(t, reg, "findings", "new", "--value", "noise", "--confidence", "5")
	if res := run(t, reg, "findings", "ls"); strings.Contains(res.Output, "noise") {
		t.Errorf("noise was listed:\n%s", res.Output)
	}
	contains(t, run(t, reg, "findings", "ls", "--all"), "noise")
	contains(t, run(t, reg, "findings", "ls", "--min-confidence", "0"), "noise")
}

func TestEntityCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "entities", "add", "--url", "https://app.test/admin"), "Added https://app.test/admin")
	contains(t, run(t, reg, "entities", "ls"), "app.test")
	contains(t, run(t, reg, "entities", "ls", "--type", "url"), "https://app.test/admin")
	if res := run(t, reg, "entities", "ls", "--type", "domain"); strings.Contains(res.Output, "/admin") {
		t.Error("--type domain listed a URL")
	}
	if err := runErr(t, reg, "entities", "ls", "--type", "nonsense"); !strings.Contains(err.Error(), "domain, host or url") {
		t.Errorf("a bad type says %q", err)
	}
	contains(t, run(t, reg, "entities", "kv", "--key", "https://app.test/admin", "--name", "note", "--value", "look"), "note = look")
	contains(t, run(t, reg, "entities", "get", "--key", "https://app.test/admin"), "discovered", "note = look")
	contains(t, run(t, reg, "entities", "rm", "--key", "https://app.test/admin"), "Deleted")

	// Promoting a related host is the target store's own CreateAll.
	contains(t, run(t, reg, "entities", "promote", "--host", "other.test"), "Promoted 1 host")
	if len(a.targets.List()) != 1 {
		t.Error("promote didn't add a target")
	}
}

func TestSettingsCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "settings", "ls", "--filter", "proxy"), "proxy_port", "proxy_host")
	contains(t, run(t, reg, "settings", "set", "--key", "theme", "--value", "dark"), `theme = "dark"`)
	if a.Settings().Theme != "dark" {
		t.Errorf("theme is %q", a.Settings().Theme)
	}
	contains(t, run(t, reg, "settings", "get", "--key", "theme"), `"dark"`)
	// A number stays a number, and a word that isn't JSON is a string.
	contains(t, run(t, reg, "settings", "set", "--key", "targets.all_entity_hooks", "--value", "true"), "true")
	if !a.Settings().Targets.AllEntityHooks {
		t.Error("a boolean setting didn't take")
	}
	if err := runErr(t, reg, "settings", "set", "--key", "nonsense", "--value", "1"); !strings.Contains(err.Error(), "no setting named") {
		t.Errorf("an unknown setting says %q", err)
	}
	if err := runErr(t, reg, "settings", "set", "--key", "proxy_port", "--value", "notaport"); !strings.Contains(err.Error(), "number") {
		t.Errorf("a bad value says %q", err)
	}

	// An engagement's own settings are separate, and null means inherit.
	contains(t, run(t, reg, "settings", "set", "--key", "targets_required", "--value", "true", "--engagement"), "targets_required = true")
	contains(t, run(t, reg, "settings", "ls", "--engagement"), "targets_required")
	contains(t, run(t, reg, "settings", "set", "--key", "targets_required", "--value", "null", "--engagement"), "null")
}

func TestEngagementCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "engagements", "current"), "Test")
	contains(t, run(t, reg, "engagements", "info"), "0 flows")
	contains(t, run(t, reg, "engagements", "rename", "--name", "Renamed"), "Renamed")

	out := filepath.Join(t.TempDir(), "copy.db")
	contains(t, run(t, reg, "engagements", "export", "--to", out), "Wrote "+out)
	if err := runErr(t, reg, "engagements", "export", "--to", out); !strings.Contains(err.Error(), "already exists") {
		t.Errorf("exporting over a file says %q", err)
	}
	contains(t, run(t, reg, "engagements", "close"), "Closed")
	if _, ok := a.engagements.Current(); ok {
		t.Error("close didn't close")
	}
	// Without an engagement, the stores that live in one say so plainly.
	if err := runErr(t, reg, "findings", "ls"); !strings.Contains(err.Error(), "no engagement is open") {
		t.Errorf("findings with no engagement says %q", err)
	}
}

func TestInstanceAndFocusCommands(t *testing.T) {
	a, reg := cliApp(t)
	contains(t, run(t, reg, "focus", "get"), "Nothing is selected")
	id := a.store.Add(capture.Flow{Method: "GET", Host: "app.test", Path: "/"})
	contains(t, run(t, reg, "flows", "open", "--flow", "1"), "Selected 1")
	if a.focus.Snapshot().SelectedFlow != id {
		t.Error("flows open didn't reach the focus tracker")
	}
	contains(t, run(t, reg, "focus", "get"), "app.test")
}

func TestCrawlAndMCPCommands(t *testing.T) {
	_, reg := cliApp(t)
	contains(t, run(t, reg, "crawl", "status"), "Autocrawl is idle")
	contains(t, run(t, reg, "crawl", "settings"), "rate", "threads")
	contains(t, run(t, reg, "crawl", "settings", "--rate", "3"), "3 requests a second")
	if err := runErr(t, reg, "crawl", "start"); !strings.Contains(err.Error(), "--yes") {
		t.Errorf("a crawl without --yes says %q", err)
	}
	contains(t, run(t, reg, "mcp", "status"), "MCP server")
	if err := runErr(t, reg, "mcp", "token", "--regenerate"); !strings.Contains(err.Error(), "--yes") {
		t.Errorf("regenerating without --yes says %q", err)
	}
}

// A command that needs a findings store with no engagement open, and one
// that takes a flow that isn't there, both fail with something the user can
// act on rather than a bare error.
func TestErrorsNameWhatToDo(t *testing.T) {
	_, reg := cliApp(t)
	for _, tc := range []struct{ args, want []string }{
		{args: []string{"flows", "get", "--flow", "99"}, want: []string{"no such flow"}},
		{args: []string{"findings", "get", "--id", "99"}, want: []string{"no finding 99", "findings ls"}},
		{args: []string{"targets", "rm", "--id", "nope"}, want: []string{"no target", "targets ls"}},
		{args: []string{"entities", "get", "--key", "nope"}, want: []string{"no entity", "entities ls"}},
		{args: []string{"agents", "get", "--session", "nope"}, want: []string{"no agent session"}},
	} {
		err := runErr(t, reg, tc.args...)
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%v says %q, which doesn't mention %q", tc.args, err, w)
			}
		}
	}
}

// A shell started from the Sessions panel can run reproxy's own command
// line without anything being installed: the running binary's directory is
// first on its PATH, and the two variables point it at this process.
func TestTerminalEnv(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	env := map[string]string{}
	for _, kv := range a.terminalEnv() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["REPROXY_INSTANCE"] != a.instanceID {
		t.Errorf("REPROXY_INSTANCE = %q, want this instance", env["REPROXY_INSTANCE"])
	}
	if env["REPROXY_DATA_DIR"] != a.DataDir() {
		t.Errorf("REPROXY_DATA_DIR = %q, want %q", env["REPROXY_DATA_DIR"], a.DataDir())
	}
	dir, err := executableDir()
	if err != nil {
		t.Skipf("no executable path here: %v", err)
	}
	first := filepath.SplitList(env["PATH"])
	if len(first) == 0 || !samePath(first[0], dir) {
		t.Errorf("PATH starts %v, want %q first", first, dir)
	}
	// The rest of the PATH is still there, and the directory is not listed
	// twice when it was already on it.
	if again := prependPath(dir, env["PATH"]); again != env["PATH"] {
		t.Errorf("a directory already on the PATH was added again:\n%s\n%s", env["PATH"], again)
	}
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	for _, tc := range []struct{ dir, path, want string }{
		{dir: "/opt/reproxy", path: "/usr/bin" + sep + "/bin", want: "/opt/reproxy" + sep + "/usr/bin" + sep + "/bin"},
		{dir: "/opt/reproxy", path: "", want: "/opt/reproxy"},
		// Already there, wherever it is: left alone rather than listed twice.
		{dir: "/opt/reproxy", path: "/usr/bin" + sep + "/opt/reproxy", want: "/usr/bin" + sep + "/opt/reproxy"},
		{dir: "/opt/reproxy", path: "/opt/reproxy/" + sep + "/usr/bin", want: "/opt/reproxy/" + sep + "/usr/bin"},
	} {
		if got := prependPath(tc.dir, tc.path); got != tc.want {
			t.Errorf("prependPath(%q, %q) = %q, want %q", tc.dir, tc.path, got, tc.want)
		}
	}
}
