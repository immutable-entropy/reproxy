package cli

// The author tools are the one kind of command that does not run inside a
// reproxy process. They work on a directory of the user's — a plugin being
// written — so they have to run where the user typed them, with that
// working directory, and they must work with no reproxy running at all.
//
// They are declared here so that there is still one list: an instance
// registers them like any other command, so `reproxy plugins` lists them
// and `--help` describes them, and the command line recognises them by
// this same declaration and runs them itself instead of sending them on.

// LocalCommands are the commands the command line runs in its own process.
func LocalCommands() []Command {
	return []Command{
		{
			Namespace: "plugins", Action: "validate", Local: true,
			Summary:     "check a plugin's plugin.json and its exports",
			Description: "Reads the manifest, reports every problem it finds, and checks that main.ts exports each function the manifest names.",
			Args:        "<dir>",
		},
		{
			Namespace: "plugins", Action: "test", Local: true,
			Summary:     "run a plugin's tests (*.test.ts)",
			Description: "The tests run against a throwaway engagement, so nothing they record touches the user's own.",
			Args:        "<dir>",
			Flags: []Flag{
				{Name: "run", Type: FlagString, Placeholder: "regexp", Description: "only the tests whose name matches"},
				{Name: "verbose", Short: "v", Type: FlagBool, Description: "list every test, and show the plugin's log"},
			},
		},
		{
			Namespace: "plugins", Action: "types", Local: true,
			Summary:     "write the SDK into <dir>/.reproxy for an editor to check against",
			Description: "Also writes a tsconfig.json pointing \"reproxy\" at it, unless there is one already.",
			Args:        "<dir>",
		},
	}
}

// IsLocal reports whether a namespace and action name a command the command
// line runs itself.
func IsLocal(namespace, action string) bool {
	ns, act := Canonical(namespace), fold(action)
	for _, c := range LocalCommands() {
		if c.Namespace == ns && c.Action == act {
			return true
		}
	}
	return false
}

// LocalCommand returns one of them, for its flags and its help.
func LocalCommand(namespace, action string) (Command, bool) {
	ns, act := Canonical(namespace), fold(action)
	for _, c := range LocalCommands() {
		if c.Namespace == ns && c.Action == act {
			return c, true
		}
	}
	return Command{}, false
}
