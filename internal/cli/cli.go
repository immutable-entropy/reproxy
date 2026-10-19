// Package cli is reproxy's command line: the shape of a command, the
// registry core and plugins declare commands in, and the plumbing that
// carries one invocation to the reproxy process that runs it.
//
// Every command reads the same way:
//
//	reproxy <namespace> <action> [--flags] [arguments]
//
// The namespace is a core object (findings, targets, plugins, flows …) or a
// namespace a plugin declared. Core namespaces are reserved: see
// namespaces.go, which is the whole list, and Reserved, which is what keeps
// a plugin from taking one.
//
// Commands run in a running reproxy process, not in the command line's own:
// the client finds the hub over loopback HTTP and the instance behind it
// does the work against its live stores, open engagement and loaded
// plugins. Only a command's declaration travels — flags are parsed where
// the declaration is, so core and plugin commands are parsed by one piece
// of code.
//
// It depends on nothing internal, and nothing in it is on the proxy's path.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is one invocation on its way to the instance that will run it.
type Request struct {
	Namespace string `json:"namespace,omitempty"`
	Action    string `json:"action,omitempty"`
	// Argv is everything after the action: flags and positional arguments,
	// still unparsed, because only the process holding the command's
	// declaration knows which flags it takes.
	Argv []string `json:"argv,omitempty"`
	// Stdin is what was piped into the command line, when anything was. A
	// command that takes a message, a scope file or a body reads it from
	// here when its flag is given as "-", so the user can pipe a file in
	// rather than quote it: there is no other way for a command running in
	// another process to reach the shell's input.
	Stdin string `json:"stdin,omitempty"`
	// JSON asks for the machine-readable form of the answer (--json).
	JSON bool `json:"json,omitempty"`
	// Help asks for the command's usage instead of running it (--help).
	Help bool `json:"help,omitempty"`
}

// Result is what a command reports back. A command fills Output for a
// person and JSON for a program; the client prints whichever the user asked
// for, falling back to the other.
type Result struct {
	Output string `json:"output,omitempty"`
	// Error is what went wrong, for stderr. A command that was called
	// wrongly answers with both: the complaint here and its usage in
	// Output, which is what a command line is expected to do.
	Error string          `json:"error,omitempty"`
	JSON  json.RawMessage `json:"json,omitempty"`
	// Code is the process's exit status: 0 unless the command says
	// otherwise.
	Code int `json:"code,omitempty"`
}

// Text returns a Result that is just text.
func Text(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...)}
}

// Data returns a Result with both forms: lines for a person, v for --json.
func Data(output string, v any) (Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: output, JSON: b}, nil
}

// FlagType is what a flag carries.
type FlagType string

// The flag types a command may declare. A list flag may be given more than
// once, or once with commas.
const (
	FlagString FlagType = "string"
	FlagNumber FlagType = "number"
	FlagBool   FlagType = "boolean"
	FlagList   FlagType = "list"
)

// Flag is one flag a command takes.
type Flag struct {
	Name string `json:"name"`
	// Short is a one-letter spelling (-v), for a core command whose older
	// form had one. A plugin's flags are long-form only: a plugin has its
	// own namespace, but the letters are everybody's.
	Short       string   `json:"short,omitempty"`
	Type        FlagType `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	// Default is the value when the flag is absent, written as it would be
	// typed. It is meaningless on a boolean, which defaults to false.
	Default string `json:"default,omitempty"`
	// Placeholder names the value in the usage line ("--name <plugin>");
	// the type's own name is used when it is empty.
	Placeholder string `json:"placeholder,omitempty"`
}

// Command is one action in one namespace.
type Command struct {
	Namespace string `json:"namespace"`
	Action    string `json:"action"`
	// Summary is the one line listings show.
	Summary string `json:"summary,omitempty"`
	// Description is the paragraph `--help` adds under the usage line.
	Description string `json:"description,omitempty"`
	// Args describes the positional arguments for the usage line, e.g.
	// "<dir>". Positional arguments are passed through unparsed.
	Args  string `json:"args,omitempty"`
	Flags []Flag `json:"flags,omitempty"`
	// Plugin is the plugin that declared the command, empty for core.
	Plugin string `json:"plugin,omitempty"`
	// Local marks a command the command line runs in its own process
	// rather than in a reproxy instance: the author tools, which work on a
	// directory of the user's and need no reproxy running. It is declared
	// here all the same, so help lists it with everything else.
	Local bool `json:"local,omitempty"`

	// Run does the work. It is set for core commands; a plugin's command
	// is run through the Registry's PluginCommands instead.
	Run func(ctx context.Context, in *Input) (Result, error) `json:"-"`
}

// Input is a parsed invocation: the command's own flags and arguments, and
// the request they came from.
type Input struct {
	Values
	Request Request
}

// Piped is what was piped into the command line. A flag given as "-" means
// "the piped input", which is how a message, a body or a file reaches a
// command running in another process.
func (in *Input) Piped() string { return in.Request.Stdin }

// Text is a flag's value, or what was piped in when the flag is "-".
func (in *Input) Text(flag string) string {
	if v := in.String(flag); v != "-" {
		return v
	}
	return in.Request.Stdin
}

// ErrUsage marks a command that was called wrongly: a flag it doesn't
// take, a missing one it needs, a value of the wrong kind. It is wrapped,
// so the text says which. The registry turns these into an answer rather
// than an error (see wrong), because nothing failed — the user typed
// something the command doesn't accept, and the right reply is the
// complaint and the usage.
var ErrUsage = errors.New("usage")

func usageErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, args...))
}

// IsUsage reports whether a command was called wrongly.
func IsUsage(err error) bool { return errors.Is(err, ErrUsage) }

// Message is an error's text without the wrapping sentinel, for printing.
func Message(err error) string {
	if s, ok := strings.CutPrefix(err.Error(), ErrUsage.Error()+": "); ok {
		return s
	}
	return err.Error()
}
