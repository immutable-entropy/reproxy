package cli

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Globals are the flags the command line answers to itself, wherever they
// appear in the arguments. They are reserved: no command may declare one
// (see checkFlags), so `--json` means the same thing everywhere.
type Globals struct {
	// Instance is the reproxy process to run in (--instance <id>); empty
	// means the hub's own.
	Instance string
	JSON     bool
	Help     bool
}

// GlobalFlags are the global flag names, without their dashes.
var GlobalFlags = []string{"instance", "json", "help"}

// Split turns a command line into the request that travels to the instance
// and the globals the client acts on. The first two bare words are the
// namespace and the action; everything else is handed on as it was typed,
// because the flags a command takes are declared where it runs.
func Split(args []string) (Request, Globals, error) {
	var (
		req  Request
		glob Globals
		rest []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
			continue
		case a == "-h", a == "--help":
			glob.Help = true
			continue
		case a == "--json":
			glob.JSON = true
			continue
		case a == "--instance", a == "-i":
			if i+1 >= len(args) {
				return req, glob, usageErr("--instance needs an instance id")
			}
			i++
			glob.Instance = args[i]
			continue
		}
		if v, ok := strings.CutPrefix(a, "--instance="); ok {
			glob.Instance = v
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			rest = append(rest, a)
			continue
		}
		switch {
		case req.Namespace == "" && len(rest) == 0:
			req.Namespace = a
		case req.Action == "" && len(rest) == 0:
			req.Action = a
		default:
			rest = append(rest, a)
		}
	}
	req.Argv, req.JSON, req.Help = rest, glob.JSON, glob.Help
	if req.Namespace == "" && len(rest) > 0 {
		// Something flag-shaped came before any namespace, so the words
		// after it were read as its value rather than as a command.
		return req, glob, usageErr("%q is not a global flag; it goes after the action", rest[0])
	}
	return req, glob, nil
}

// Values are a command's parsed flags and positional arguments.
type Values struct {
	flags map[string]any
	given map[string]bool
	args  []string
}

// NewValues builds Values directly, for a caller that has them already.
func NewValues(flags map[string]any, args []string) Values {
	given := make(map[string]bool, len(flags))
	for k := range flags {
		given[k] = true
	}
	return Values{flags: flags, given: given, args: args}
}

// Given reports whether the flag was actually typed, as against defaulted.
func (v Values) Given(name string) bool { return v.given[name] }

// String is the flag's value as text ("" when absent).
func (v Values) String(name string) string {
	s, _ := v.flags[name].(string)
	return s
}

// Float is a number flag's value.
func (v Values) Float(name string) float64 {
	f, _ := v.flags[name].(float64)
	return f
}

// Int is a number flag's value, truncated.
func (v Values) Int(name string) int { return int(v.Float(name)) }

// Bool is a boolean flag's value.
func (v Values) Bool(name string) bool {
	b, _ := v.flags[name].(bool)
	return b
}

// List is a list flag's values.
func (v Values) List(name string) []string {
	l, _ := v.flags[name].([]string)
	return l
}

// Args are the positional arguments, in the order they were given.
func (v Values) Args() []string { return v.args }

// Arg is the nth positional argument, or "".
func (v Values) Arg(n int) string {
	if n < 0 || n >= len(v.args) {
		return ""
	}
	return v.args[n]
}

// Flags are every flag's value, defaults included: what a plugin's command
// is handed. The values are strings, numbers, booleans and string lists, so
// they cross into JavaScript as themselves.
func (v Values) Flags() map[string]any {
	out := make(map[string]any, len(v.flags))
	maps.Copy(out, v.flags)
	return out
}

// ParseFlags parses a command's arguments against what it declared. A flag
// that wasn't declared is an error rather than a positional argument: a
// mistyped flag should say so, not be silently handed on.
func ParseFlags(c Command, argv []string) (Values, error) {
	decl := make(map[string]*Flag, len(c.Flags))
	short := make(map[string]*Flag)
	for i := range c.Flags {
		decl[c.Flags[i].Name] = &c.Flags[i]
		if s := c.Flags[i].Short; s != "" {
			short[s] = &c.Flags[i]
		}
	}
	v := Values{flags: map[string]any{}, given: map[string]bool{}}

	take := func(f *Flag, raw string) error {
		switch f.Type {
		case FlagNumber:
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return usageErr("--%s takes a number, not %q", f.Name, raw)
			}
			v.flags[f.Name] = n
		case FlagList:
			var list []string
			if prev, ok := v.flags[f.Name].([]string); ok && v.given[f.Name] {
				list = prev
			}
			for part := range strings.SplitSeq(raw, ",") {
				if part = strings.TrimSpace(part); part != "" {
					list = append(list, part)
				}
			}
			v.flags[f.Name] = list
		default:
			v.flags[f.Name] = raw
		}
		v.given[f.Name] = true
		return nil
	}

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			v.args = append(v.args, argv[i+1:]...)
			break
		}
		long := strings.HasPrefix(a, "--")
		if !long && (!strings.HasPrefix(a, "-") || a == "-") {
			v.args = append(v.args, a)
			continue
		}
		// One dash or two: `-run x` is how these flags were written before
		// the command line had a shape, and a one-letter flag is always
		// written with one.
		name, raw, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f, ok := decl[name]
		if !ok && !long {
			f, ok = short[name]
		}
		if !ok {
			// --no-<bool> switches a boolean off, which is how a flag that
			// defaults on is turned off.
			if off, cut := strings.CutPrefix(name, "no-"); cut && !hasValue {
				if f, ok = decl[off]; ok && f.Type == FlagBool {
					v.flags[f.Name], v.given[f.Name] = false, true
					continue
				}
			}
			return v, usageErr("%s doesn't take --%s", c.Namespace+" "+c.Action, name)
		}
		if f.Type == FlagBool {
			switch {
			case !hasValue:
				v.flags[f.Name], v.given[f.Name] = true, true
			default:
				b, err := strconv.ParseBool(raw)
				if err != nil {
					return v, usageErr("--%s takes true or false, not %q", f.Name, raw)
				}
				v.flags[f.Name], v.given[f.Name] = b, true
			}
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return v, usageErr("--%s needs a value", f.Name)
			}
			i++
			raw = argv[i]
		}
		if err := take(f, raw); err != nil {
			return v, err
		}
	}

	// Defaults for what wasn't given, and a word about what had to be.
	var missing []string
	for i := range c.Flags {
		f := &c.Flags[i]
		if v.given[f.Name] {
			continue
		}
		if f.Required {
			missing = append(missing, "--"+f.Name)
			continue
		}
		switch f.Type {
		case FlagBool:
			v.flags[f.Name] = f.Default == "true"
		case FlagNumber:
			n, _ := strconv.ParseFloat(f.Default, 64)
			v.flags[f.Name] = n
		case FlagList:
			var list []string
			for part := range strings.SplitSeq(f.Default, ",") {
				if part = strings.TrimSpace(part); part != "" {
					list = append(list, part)
				}
			}
			v.flags[f.Name] = list
		default:
			v.flags[f.Name] = f.Default
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return v, usageErr("%s needs %s", c.Namespace+" "+c.Action, strings.Join(missing, " and "))
	}
	return v, nil
}

// checkFlags reports what is wrong with a command's declaration. It runs
// when a command is registered, so a bad declaration is caught where it is
// written rather than when someone runs it.
func checkFlags(c Command) error {
	seen := map[string]bool{}
	for _, f := range c.Flags {
		switch {
		case !flagNameRe.MatchString(f.Name):
			return usageErr("flag %q must be lower-case letters, digits and dashes", f.Name)
		case seen[f.Name]:
			return usageErr("flag --%s is declared twice", f.Name)
		case isGlobal(f.Name):
			return usageErr("--%s is a global flag and can't be a command's own", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case FlagString, FlagNumber, FlagBool, FlagList:
		default:
			return usageErr("flag --%s has type %q, not string, number, boolean or list", f.Name, f.Type)
		}
		if f.Required && f.Default != "" {
			return usageErr("flag --%s is required, so a default means nothing", f.Name)
		}
		if f.Type == FlagNumber && f.Default != "" {
			if _, err := strconv.ParseFloat(f.Default, 64); err != nil {
				return usageErr("flag --%s has a default that isn't a number", f.Name)
			}
		}
		if f.Type == FlagBool && f.Default != "" && f.Default != "true" && f.Default != "false" {
			return usageErr("flag --%s has a default that isn't true or false", f.Name)
		}
	}
	return nil
}

func isGlobal(name string) bool { return slices.Contains(GlobalFlags, name) }

// Tokenize splits one line typed in the shell (`reproxy shell`) the way a
// shell would: on spaces, with single and double quotes holding a value
// together and a backslash escaping the next character. It is deliberately
// small — this is reproxy's own prompt, not a shell, so there is no
// expansion of any kind.
func Tokenize(line string) ([]string, error) {
	var (
		out   []string
		cur   strings.Builder
		have  bool
		quote rune
	)
	flush := func() {
		if have {
			out = append(out, cur.String())
			cur.Reset()
			have = false
		}
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\' && quote != '\'' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			have = true
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			have = true // "" is an argument, empty though it is
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteRune(c)
			have = true
		}
	}
	if quote != 0 {
		return nil, usageErr("unclosed %c quote", quote)
	}
	flush()
	return out, nil
}
