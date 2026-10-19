package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- The shape of a command line -------------------------------------------

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		args            []string
		ns, action      string
		argv            []string
		instance        string
		json, help, err bool
	}{
		{args: []string{"plugins", "ls"}, ns: "plugins", action: "ls"},
		{args: []string{"plugins", "disable", "--name", "x"}, ns: "plugins", action: "disable", argv: []string{"--name", "x"}},
		{args: []string{"--json", "findings", "ls"}, ns: "findings", action: "ls", json: true},
		{args: []string{"findings", "ls", "--json"}, ns: "findings", action: "ls", json: true},
		{args: []string{"--instance", "ab12", "plugins", "ls"}, ns: "plugins", action: "ls", instance: "ab12"},
		{args: []string{"plugins", "ls", "--instance=ab12"}, ns: "plugins", action: "ls", instance: "ab12"},
		{args: []string{"plugins", "ls", "-h"}, ns: "plugins", action: "ls", help: true},
		// A value that looks like a word still belongs to the flag before it.
		{args: []string{"flows", "ls", "--filter", "example.com"}, ns: "flows", action: "ls", argv: []string{"--filter", "example.com"}},
		// Everything after -- is the command's, globals included.
		{args: []string{"words", "count", "--", "--json"}, ns: "words", action: "count", argv: []string{"--json"}},
		{args: []string{}, ns: ""},
		{args: []string{"--nope", "plugins", "ls"}, err: true},
	} {
		req, glob, err := Split(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("Split(%v) error %v, want error: %v", tc.args, err, tc.err)
			continue
		}
		if err != nil {
			continue
		}
		if req.Namespace != tc.ns || req.Action != tc.action {
			t.Errorf("Split(%v) = %q %q, want %q %q", tc.args, req.Namespace, req.Action, tc.ns, tc.action)
		}
		if strings.Join(req.Argv, " ") != strings.Join(tc.argv, " ") {
			t.Errorf("Split(%v) argv = %v, want %v", tc.args, req.Argv, tc.argv)
		}
		if glob.Instance != tc.instance || glob.JSON != tc.json || glob.Help != tc.help {
			t.Errorf("Split(%v) globals = %+v", tc.args, glob)
		}
	}
}

var parseCmd = Command{
	Namespace: "demo", Action: "run",
	Flags: []Flag{
		{Name: "name", Type: FlagString, Required: true},
		{Name: "count", Type: FlagNumber, Default: "7"},
		{Name: "loud", Type: FlagBool},
		{Name: "quiet", Type: FlagBool, Default: "true"},
		{Name: "tags", Type: FlagList},
		{Name: "verbose", Short: "v", Type: FlagBool},
	},
}

func TestParseFlags(t *testing.T) {
	v, err := ParseFlags(parseCmd, []string{"--name", "a", "--count=3", "--loud", "--no-quiet",
		"--tags", "x,y", "--tags", "z", "-v", "one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if v.String("name") != "a" || v.Int("count") != 3 {
		t.Errorf("name=%q count=%d", v.String("name"), v.Int("count"))
	}
	if !v.Bool("loud") || v.Bool("quiet") || !v.Bool("verbose") {
		t.Errorf("booleans: loud=%v quiet=%v verbose=%v", v.Bool("loud"), v.Bool("quiet"), v.Bool("verbose"))
	}
	if got := strings.Join(v.List("tags"), "|"); got != "x|y|z" {
		t.Errorf("tags = %q", got)
	}
	if got := strings.Join(v.Args(), " "); got != "one two" {
		t.Errorf("args = %q", got)
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	v, err := ParseFlags(parseCmd, []string{"--name", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Int("count") != 7 || !v.Bool("quiet") || v.Bool("loud") {
		t.Errorf("defaults: count=%d quiet=%v loud=%v", v.Int("count"), v.Bool("quiet"), v.Bool("loud"))
	}
	if v.Given("count") {
		t.Error("a defaulted flag counts as given")
	}
}

func TestParseFlagsRefuses(t *testing.T) {
	for _, tc := range []struct{ argv, want []string }{
		{argv: []string{"--name", "a", "--nope"}, want: []string{"--nope"}},
		{argv: []string{}, want: []string{"--name"}},
		{argv: []string{"--name", "a", "--count", "lots"}, want: []string{"--count", "lots"}},
		{argv: []string{"--name"}, want: []string{"--name", "needs a value"}},
	} {
		_, err := ParseFlags(parseCmd, tc.argv)
		if err == nil {
			t.Errorf("ParseFlags(%v) was accepted", tc.argv)
			continue
		}
		if !IsUsage(err) {
			t.Errorf("ParseFlags(%v) = %v, not a usage error", tc.argv, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseFlags(%v) = %q, which doesn't mention %q", tc.argv, err, want)
			}
		}
	}
}

func TestTokenize(t *testing.T) {
	got, err := Tokenize(`plugins disable --name "my plugin" 'a b' c\ d`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plugins", "disable", "--name", "my plugin", "a b", "c d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
	if _, err := Tokenize(`a "b`); err == nil {
		t.Error("an unclosed quote was accepted")
	}
}

// ---- Namespaces ------------------------------------------------------------

func TestNamespacesAreReserved(t *testing.T) {
	for _, name := range []string{"findings", "finding", "FINDINGS", "plugins", "plugin", "help", "shell", "version"} {
		if !Reserved(name) {
			t.Errorf("%q is not reserved", name)
		}
	}
	if Reserved("fuzzer") {
		t.Error("a name core doesn't use is reserved")
	}
	if Canonical("finding") != "findings" || Canonical("Fuzzer") != "fuzzer" {
		t.Errorf("Canonical: %q %q", Canonical("finding"), Canonical("Fuzzer"))
	}
}

// Every namespace is described, and no two share a spelling: the table is
// what plugins are kept out of, so a mistake in it is a mistake in the
// contract.
func TestNamespaceTable(t *testing.T) {
	seen := map[string]string{}
	for _, ns := range CoreNamespaces() {
		if !ValidNamespace(ns.Name) {
			t.Errorf("%q is not a namespace name", ns.Name)
		}
		if ns.Summary == "" || ns.Object == "" {
			t.Errorf("%s has no summary or object", ns.Name)
		}
		for _, name := range append([]string{ns.Name}, ns.Aliases...) {
			if other, ok := seen[name]; ok {
				t.Errorf("%q is used by both %s and %s", name, other, ns.Name)
			}
			seen[name] = ns.Name
		}
	}
	for _, b := range Builtins {
		if other, ok := seen[b]; ok {
			t.Errorf("the builtin %q is also the namespace %s", b, other)
		}
	}
}

// ---- The registry ----------------------------------------------------------

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	err := reg.Add(Command{
		Namespace: "plugins", Action: "ls", Summary: "list them",
		Run: func(context.Context, *Input) (Result, error) { return Text("one\ntwo\n"), nil },
	}, Command{
		Namespace: "plugin", Action: "disable", Summary: "switch one off",
		Flags: []Flag{{Name: "name", Type: FlagString, Required: true}},
		Run: func(_ context.Context, in *Input) (Result, error) {
			return Text("disabled %s", in.String("name")), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func runReq(t *testing.T, reg *Registry, args ...string) Result {
	t.Helper()
	req, _, err := Split(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return res
}

func TestRegistryRuns(t *testing.T) {
	reg := testRegistry(t)
	if res := runReq(t, reg, "plugins", "ls"); res.Output != "one\ntwo\n" {
		t.Errorf("output %q", res.Output)
	}
	// An alias resolves to the same namespace, both ways round.
	if res := runReq(t, reg, "plugin", "ls"); res.Output != "one\ntwo\n" {
		t.Errorf("through the alias: %q", res.Output)
	}
	if res := runReq(t, reg, "plugins", "disable", "--name", "x"); res.Output != "disabled x" {
		t.Errorf("output %q", res.Output)
	}
}

func TestRegistryRefusesNonCoreNamespace(t *testing.T) {
	reg := NewRegistry()
	err := reg.Add(Command{Namespace: "whatever", Action: "ls", Run: func(context.Context, *Input) (Result, error) {
		return Result{}, nil
	}})
	if err == nil {
		t.Fatal("a core command was registered outside the namespace table")
	}
}

func TestRegistryUnknown(t *testing.T) {
	reg := testRegistry(t)
	res := runReq(t, reg, "plugins", "lst")
	if res.Code != UsageCode || !strings.Contains(res.Error, "Did you mean ls?") {
		t.Errorf("got %+v", res)
	}
	if !strings.Contains(res.Output, "Actions:") {
		t.Errorf("no actions listed: %q", res.Output)
	}

	// A reserved namespace nothing is registered in says so, rather than
	// claiming it doesn't exist.
	res = runReq(t, reg, "findings", "ls")
	if res.Code != UsageCode || !strings.Contains(res.Error, "no commands yet") {
		t.Errorf("got %+v", res)
	}
}

func TestRegistryHelp(t *testing.T) {
	reg := testRegistry(t)
	overview := runReq(t, reg).Output
	for _, want := range []string{"Core namespaces:", "plugins", "reproxy shell", "Reserved for core, with no commands yet:"} {
		if !strings.Contains(overview, want) {
			t.Errorf("the overview has no %q:\n%s", want, overview)
		}
	}
	if got := runReq(t, reg, "help", "plugins").Output; !strings.Contains(got, "disable") {
		t.Errorf("`help plugins` = %q", got)
	}
	if got := runReq(t, reg, "plugins", "disable", "--help").Output; !strings.Contains(got, "--name <value>") {
		t.Errorf("`plugins disable --help` = %q", got)
	}
}

// ---- Plugin commands through the registry ----------------------------------

type fakePlugins struct {
	cmds []Command
	ran  *Input
}

func (f *fakePlugins) Commands() []Command { return f.cmds }
func (f *fakePlugins) RunCommand(_ context.Context, c Command, in *Input) (Result, error) {
	f.ran = in
	return Text("%s %s ran with %s", c.Namespace, c.Action, in.String("word")), nil
}

func TestPluginCommands(t *testing.T) {
	reg := testRegistry(t)
	p := &fakePlugins{cmds: []Command{{
		Namespace: "fuzz", Action: "run", Summary: "fuzz it", Plugin: "fuzzer",
		Flags: []Flag{{Name: "word", Type: FlagString, Default: "x"}},
	}}}
	reg.SetPlugins(p)

	if res := runReq(t, reg, "fuzz", "run", "--word", "boo"); res.Output != "fuzz run ran with boo" {
		t.Errorf("output %q", res.Output)
	}
	if res := runReq(t, reg, "fuzz", "run"); res.Output != "fuzz run ran with x" {
		t.Errorf("the default didn't reach the plugin: %q", res.Output)
	}
	if got := runReq(t, reg).Output; !strings.Contains(got, "from the fuzzer plugin") {
		t.Errorf("the overview doesn't list it:\n%s", got)
	}
}

// A plugin that declares a core namespace is ignored even if it somehow
// started: core's answer is the one the user gets.
func TestPluginCannotShadowCore(t *testing.T) {
	reg := testRegistry(t)
	reg.SetPlugins(&fakePlugins{cmds: []Command{{
		Namespace: "plugins", Action: "ls", Summary: "not this one", Plugin: "sneaky",
	}}})
	if res := runReq(t, reg, "plugins", "ls"); res.Output != "one\ntwo\n" {
		t.Errorf("a plugin answered for core: %q", res.Output)
	}
}

// ---- The hub route ---------------------------------------------------------

type fakeRunner struct{ got Request }

func (f *fakeRunner) RunCommand(_ context.Context, req Request) (Result, error) {
	f.got = req
	if req.Action == "boom" {
		return Result{}, errors.New("it went wrong")
	}
	return Text("ran %s %s", req.Namespace, req.Action), nil
}

func TestHubHandler(t *testing.T) {
	runner := &fakeRunner{}
	srv := httptest.NewServer(HubHandler(func(id string) (Runner, string, error) {
		if id == "missing" {
			return nil, id, errors.New("no instance \"missing\" is reachable")
		}
		if id == "" {
			id = "local"
		}
		return runner, id, nil
	}))
	t.Cleanup(srv.Close)

	post := func(body HubRequest) HubReply {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply HubReply
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}

	reply := post(HubRequest{Request: Request{Namespace: "plugins", Action: "ls", Argv: []string{"--name", "x"}}})
	if reply.Error != "" || reply.Result == nil || reply.Result.Output != "ran plugins ls" {
		t.Fatalf("got %+v", reply)
	}
	if reply.Instance != "local" {
		t.Errorf("ran in %q, want the hub's own", reply.Instance)
	}
	// The arguments reach the instance as typed: they are parsed there.
	if strings.Join(runner.got.Argv, " ") != "--name x" {
		t.Errorf("argv arrived as %v", runner.got.Argv)
	}

	if reply := post(HubRequest{Instance: "missing", Request: Request{Namespace: "plugins", Action: "ls"}}); reply.Error == "" {
		t.Error("an unreachable instance was not reported")
	}
	if reply := post(HubRequest{Request: Request{Namespace: "plugins", Action: "boom"}}); reply.Error != "it went wrong" {
		t.Errorf("a failing command came back as %+v", reply)
	}
}
