package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Commands are what a plugin adds to reproxy's command line:
//
//	reproxy <plugin> <action> --flags
//
// The namespace is the plugin's own name, so a plugin declares actions, not
// namespaces: everything the wordpress plugin adds is under
// `reproxy wordpress`. One plugin, one word, and the user can always see
// whose command they are running.
//
// The plugin declares each action and the flags it takes, and core does the
// rest: it parses the flags, writes the help, and calls the plugin's
// function with the values. Nothing enters the TypeScript runtime for a
// command the user didn't type.
//
// What is checked here is the shape alone. Whether the plugin's name is
// free — core's namespaces are reserved — is checked by the plugin Manager
// when the plugin starts, because only core knows what the core namespaces
// are.

// Command is one action a plugin adds to the command line.
type Command struct {
	// Action is the word after the plugin's name.
	Action string `json:"action"`
	// Summary is the one line listings show.
	Summary string `json:"summary,omitempty"`
	// Description is the paragraph --help adds.
	Description string `json:"description,omitempty"`
	// Args describes the positional arguments in the usage line, e.g.
	// "<file>...". They are passed through as typed; a command that wants
	// them checked should declare flags instead.
	Args     string        `json:"args,omitempty"`
	Flags    []CommandFlag `json:"flags,omitempty"`
	Function string        `json:"function"`
}

// CommandFlag is one flag a command takes.
type CommandFlag struct {
	Name string `json:"name"`
	// Type is string, number, boolean or list; inferred from Default when
	// empty, and string when there is neither.
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	// Default is the value when the flag is absent, as JSON.
	Default json.RawMessage `json:"default,omitempty"`
	// Placeholder names the value in the usage line ("--name <plugin>").
	Placeholder string `json:"placeholder,omitempty"`
}

// The flag types a command may declare. They are the Setting types a flag
// can be typed on a command line: a secret doesn't belong in shell history,
// and an enum is a string the command checks.
const (
	FlagString  = TypeString
	FlagNumber  = TypeNumber
	FlagBoolean = TypeBoolean
	FlagList    = TypeList
)

// MaxSummary is the longest a command's summary may be.
const MaxSummary = 80

// These mirror internal/cli, which owns the command line's rules. They are
// copied rather than imported because a manifest is parsed by things that
// have no business depending on the rest of reproxy (the plugin validator,
// the test runner), and the shapes are not going to move: a change here
// needs the same change there, and manifest_test keeps them honest.
var (
	cmdActionRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	cmdFlagRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// globalFlags belong to the command line itself, wherever they appear,
	// so a command can't declare one of its own.
	globalFlags = []string{"instance", "json", "help"}
)

func (c *Command) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !cmdActionRe.MatchString(c.Action) {
		add("action %q must be 1-32 lower-case letters, digits and dashes", c.Action)
	}
	if summary := strings.TrimSpace(c.Summary); summary == "" {
		add("summary is required: it is the line help lists the command with")
	} else if len([]rune(summary)) > MaxSummary || strings.ContainsAny(summary, "\r\n") {
		add("summary must be one line of at most %d characters", MaxSummary)
	}
	if !identRe.MatchString(c.Function) {
		add("function %q is not a function name", c.Function)
	}
	seen := map[string]bool{}
	for i := range c.Flags {
		f := &c.Flags[i]
		if seen[f.Name] {
			add("flags[%d]: --%s is declared twice", i, f.Name)
		}
		seen[f.Name] = true
		if err := f.validate(); err != nil {
			add("flags[%d] (%s): %v", i, f.Name, err)
		}
	}
	return errs
}

func (f *CommandFlag) validate() error {
	if !cmdFlagRe.MatchString(f.Name) {
		return errors.New("name must be lower-case letters, digits and dashes, starting with a letter")
	}
	for _, g := range globalFlags {
		if f.Name == g {
			return fmt.Errorf("--%s is a global flag of reproxy's own", g)
		}
	}
	if f.Type == "" {
		if f.Type = inferType(f.Default); f.Type == "" {
			f.Type = FlagString
		}
	}
	switch f.Type {
	case FlagString, FlagNumber, FlagBoolean, FlagList:
	default:
		return fmt.Errorf("type %q must be string, number, boolean or list", f.Type)
	}
	if f.Required && len(f.Default) > 0 && string(f.Default) != "null" {
		return errors.New("it is required, so a default means nothing")
	}
	if len(f.Default) > 0 {
		s := Setting{Key: "x", Type: f.Type, Options: nil}
		if err := s.Check(f.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// DefaultString renders a flag's default the way it would be typed, which
// is the form internal/cli works in.
func (f *CommandFlag) DefaultString() string {
	if len(f.Default) == 0 || string(f.Default) == "null" {
		return ""
	}
	var v any
	if json.Unmarshal(f.Default, &v) != nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// Namespace is the first word of this plugin's commands: its own name, or
// "" when it declares no commands. The Manager checks it against core's
// before it starts the plugin.
func (m *Manifest) Namespace() string {
	if len(m.Commands) == 0 {
		return ""
	}
	return m.Name
}
