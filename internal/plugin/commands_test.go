package plugin

import (
	"context"
	"strings"
	"testing"

	"reproxy/internal/cli"
)

const commander = `{
	"name": "commander", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
	"commands": [
		{
			"action": "count", "summary": "count the words given",
			"function": "count",
			"flags": [
				{"name": "upper", "type": "boolean", "description": "answer in capitals"},
				{"name": "times", "type": "number", "default": 1},
				{"name": "tags", "type": "list"}
			]
		},
		{"action": "boom", "summary": "always fails", "function": "boom"},
		{"action": "quiet", "summary": "answers nothing", "function": "quiet"}
	]
}`

const commanderTS = `
import type { CommandHandler } from "reproxy";
export const count: CommandHandler = ({ flags, args, json }) => {
	let text = args.length + " words, times " + flags.times + ", tags [" + (flags.tags as string[]).join("|") + "]";
	if (flags.upper) text = text.toUpperCase();
	if (json) return { output: text, json: { words: args, times: flags.times } };
	return text;
};
export function boom() { throw new Error("nope"); }
export function quiet() {}
`

func commandEnv(t *testing.T) *Manager {
	t.Helper()
	_, m := env(t, pluginFS("commander", commander, commanderTS))
	m.Rescan()
	m.Apply(Config{})
	return m
}

// run resolves and runs a command through a registry, the way the command
// line does.
func run(t *testing.T, m *Manager, args ...string) cli.Result {
	t.Helper()
	reg := cli.NewRegistry()
	reg.SetPlugins(m)
	req, _, err := cli.Split(args)
	if err != nil {
		t.Fatalf("splitting %v: %v", args, err)
	}
	res, err := reg.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return res
}

func TestCommandsDeclared(t *testing.T) {
	m := commandEnv(t)
	cmds := m.Commands()
	if len(cmds) != 3 {
		t.Fatalf("declared %d commands, want 3: %+v", len(cmds), cmds)
	}
	c := cmds[0]
	// The namespace is the plugin's own name: one plugin, one word.
	if c.Plugin != "commander" || c.Namespace != "commander" || c.Action != "count" {
		t.Errorf("first command is %+v", c)
	}
	if len(c.Flags) != 3 || c.Flags[1].Default != "1" {
		t.Errorf("flags are %+v", c.Flags)
	}
}

func TestRunCommand(t *testing.T) {
	m := commandEnv(t)
	res := run(t, m, "commander", "count", "alpha", "beta", "--times", "3", "--tags", "a,b")
	if want := "2 words, times 3, tags [a|b]"; res.Output != want {
		t.Errorf("output %q, want %q", res.Output, want)
	}
	if res.Code != 0 || res.Error != "" {
		t.Errorf("code %d, error %q", res.Code, res.Error)
	}
}

func TestRunCommandJSON(t *testing.T) {
	m := commandEnv(t)
	res := run(t, m, "commander", "count", "alpha", "--json")
	if !strings.Contains(string(res.JSON), `"words":["alpha"]`) {
		t.Errorf("json is %s", res.JSON)
	}
}

// A flag the command didn't declare is a complaint and the command's usage,
// not a plugin call: the plugin never sees an invocation it can't answer.
func TestRunCommandUnknownFlag(t *testing.T) {
	m := commandEnv(t)
	res := run(t, m, "commander", "count", "--nope")
	if res.Code != cli.UsageCode {
		t.Fatalf("code %d, want %d", res.Code, cli.UsageCode)
	}
	if !strings.Contains(res.Error, "--nope") {
		t.Errorf("error is %q", res.Error)
	}
	if !strings.Contains(res.Output, "Usage:") {
		t.Errorf("no usage in %q", res.Output)
	}
}

func TestRunCommandThatThrows(t *testing.T) {
	m := commandEnv(t)
	reg := cli.NewRegistry()
	reg.SetPlugins(m)
	req, _, _ := cli.Split([]string{"commander", "boom"})
	if _, err := reg.Run(context.Background(), req); err == nil {
		t.Fatal("a command that throws reported no error")
	}
}

func TestRunCommandWithNoAnswer(t *testing.T) {
	m := commandEnv(t)
	if res := run(t, m, "commander", "quiet"); res.Output != "" || res.Code != 0 {
		t.Errorf("got %+v", res)
	}
}

// Help is answered from the declaration, without entering the runtime.
func TestCommandHelp(t *testing.T) {
	m := commandEnv(t)
	res := run(t, m, "commander", "count", "--help")
	for _, want := range []string{"--upper", "--times <number>", "commander plugin"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("help has no %q:\n%s", want, res.Output)
		}
	}
}

// ---- Namespaces ------------------------------------------------------------

// A plugin's commands answer to its own name, so a plugin called
// "findings" is one core already answers for.
const coreTaker = `{
	"name": "findings", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
	"commands": [{"action": "ls", "summary": "steal core's", "function": "ls"}]
}`

const takerTS = `export function ls() { return "no"; }`

// A plugin named for a core namespace doesn't run at all, and says why.
func TestCoreNamespaceRefused(t *testing.T) {
	_, m := env(t, pluginFS("findings", coreTaker, takerTS))
	m.Rescan()
	m.Apply(Config{})

	in, ok := m.Get("findings")
	if !ok {
		t.Fatal("the plugin wasn't found")
	}
	if in.State != StateError {
		t.Errorf("state %q, want %q", in.State, StateError)
	}
	if problems := strings.Join(in.Problems, "; "); !strings.Contains(problems, `"findings" belongs to core`) {
		t.Errorf("problems are %q", problems)
	}
	if len(m.Commands()) != 0 {
		t.Errorf("it offered commands anyway: %+v", m.Commands())
	}
}

// Two plugins can't share a namespace because they can't share a name:
// the Manager knows a plugin by the directory it is in, and the manifest's
// name must equal it. This is the test that says so.
func TestNamespaceIsThePluginName(t *testing.T) {
	m := commandEnv(t)
	for _, c := range m.Commands() {
		if c.Namespace != c.Plugin {
			t.Errorf("%s %s answers to %q, not to its plugin's name", c.Plugin, c.Action, c.Namespace)
		}
	}
	if len(m.Commands()) == 0 {
		t.Error("the plugin offered no commands at all")
	}
}
