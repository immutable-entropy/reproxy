package cli

import "strings"

// A catalogue is the command line as data: every command this process can
// run, with the flags each one takes. `reproxy help --json` answers with
// one, which is how the interactive shell knows what to complete — the
// commands a plugin adds are in it the moment the plugin is enabled, and
// the shell needs no second source of truth to keep in step.

// Catalogue is every command a process offers.
type Catalogue struct {
	Commands []Command `json:"commands"`
	// Builtins are the words the command line answers itself.
	Builtins []string `json:"builtins"`
	// Globals are the flags every command takes, without their dashes.
	Globals []string `json:"globals"`
}

// Catalogue describes everything this registry can run.
func (r *Registry) Catalogue() Catalogue {
	return Catalogue{Commands: r.All(), Builtins: Builtins, Globals: GlobalFlags}
}

// WithWords returns the catalogue with more words to complete: what the
// caller answers itself, which for the interactive shell is exit and quit.
func (c Catalogue) WithWords(words ...string) Catalogue {
	c.Builtins = append(append([]string(nil), c.Builtins...), words...)
	return c
}

// Namespaces are the namespaces the catalogue holds commands in, in the
// order the commands were listed (core first, then plugins).
func (c Catalogue) Namespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, cmd := range c.Commands {
		if !seen[cmd.Namespace] {
			seen[cmd.Namespace] = true
			out = append(out, cmd.Namespace)
		}
	}
	return out
}

// Actions are the actions in one namespace.
func (c Catalogue) Actions(namespace string) []string {
	ns := Canonical(namespace)
	var out []string
	for _, cmd := range c.Commands {
		if cmd.Namespace == ns {
			out = append(out, cmd.Action)
		}
	}
	return out
}

// Command finds one command.
func (c Catalogue) Command(namespace, action string) (Command, bool) {
	ns, act := Canonical(namespace), fold(action)
	for _, cmd := range c.Commands {
		if cmd.Namespace == ns && cmd.Action == act {
			return cmd, true
		}
	}
	return Command{}, false
}

// Complete answers what the word under the cursor could become: the
// namespaces, then that namespace's actions, then the flags the command
// declared. It completes what is being typed and nothing else — there is
// no guessing at a flag's value, because a command line that fills in a
// host name or a flow id for you is a command line you can't trust.
func (c Catalogue) Complete(line string, cursor int) (word string, candidates []string) {
	if cursor > len(line) {
		cursor = len(line)
	}
	before := line[:cursor]
	words := strings.Fields(before)
	// A trailing space means the word under the cursor is a new, empty one.
	if before == "" || strings.HasSuffix(before, " ") {
		words = append(words, "")
	}
	word = words[len(words)-1]
	// The words before it, which say what is being completed.
	prior := words[:len(words)-1]

	switch {
	case strings.HasPrefix(word, "-"):
		candidates = c.flagsFor(prior, word)
	case len(prior) == 0:
		candidates = withPrefix(append(c.Namespaces(), c.Builtins...), word)
	case len(prior) == 1:
		candidates = withPrefix(c.Actions(prior[0]), word)
	}
	return word, candidates
}

// flagsFor are the flags that could follow, as --name.
func (c Catalogue) flagsFor(prior []string, word string) []string {
	var names []string
	if len(prior) >= 2 {
		if cmd, ok := c.Command(prior[0], prior[1]); ok {
			for _, f := range cmd.Flags {
				names = append(names, "--"+f.Name)
			}
		}
	}
	for _, g := range c.Globals {
		names = append(names, "--"+g)
	}
	// A flag already on the line is not offered again.
	var out []string
	for _, n := range names {
		if !hasWord(prior, n) {
			out = append(out, n)
		}
	}
	return withPrefix(out, word)
}

func hasWord(words []string, word string) bool {
	for _, w := range words {
		if w == word {
			return true
		}
	}
	return false
}

// withPrefix keeps the candidates that could be what is being typed.
func withPrefix(all []string, word string) []string {
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, word) {
			out = append(out, c)
		}
	}
	return out
}

// CommonPrefix is the longest prefix every candidate shares, which is how
// far one Tab can take a line that is still ambiguous.
func CommonPrefix(words []string) string {
	if len(words) == 0 {
		return ""
	}
	prefix := words[0]
	for _, w := range words[1:] {
		for !strings.HasPrefix(w, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}
