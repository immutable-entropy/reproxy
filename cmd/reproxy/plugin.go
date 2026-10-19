package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"reproxy/internal/cli"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/plugintest"
	"reproxy/internal/plugin/sdk"
)

// The author tools: `reproxy plugins validate|test|types <dir>`. They are
// the commands that run in this process rather than in a reproxy instance
// — a plugin being written is a directory of the user's, and none of this
// needs a running app. What they take is declared with every other command
// in cli.LocalCommands, and the arguments reaching here are already parsed
// against it.
//
// `reproxy plugin …` still works: `plugin` is the singular alias of the
// `plugins` namespace, so the older spelling resolves to the same command.

func runAuthorTool(action string, vals cli.Values, stdout, stderr io.Writer) int {
	dir, err := filepath.Abs(vals.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", err)
		return 1
	}
	if action == "types" {
		if err := writeTypes(dir); err != nil {
			fmt.Fprintln(stderr, "reproxy:", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s and tsconfig.json\n", filepath.Join(dir, ".reproxy"))
		return 0
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", err)
		return 1
	}
	defer root.Close()
	src := plugin.Source{Name: filepath.Base(dir), FS: root.FS(), Path: dir}

	switch action {
	case "validate":
		man, problems := plugin.Inspect(src)
		if len(problems) > 0 {
			fmt.Fprintf(stderr, "%s has problems:\n", src.Name)
			for _, p := range problems {
				fmt.Fprintf(stderr, "  %s\n", p)
			}
			return 1
		}
		parts := []string{fmt.Sprintf("%d targets", len(man.Targets))}
		if len(man.Actions) > 0 {
			parts = append(parts, fmt.Sprintf("%d actions", len(man.Actions)))
		}
		if len(man.Commands) > 0 {
			parts = append(parts, fmt.Sprintf("%d commands (reproxy %s …)", len(man.Commands), man.Name))
		}
		parts = append(parts, fmt.Sprintf("%d settings", len(man.Settings)))
		fmt.Fprintf(stdout, "ok\t%s %s: %s\n", man.Name, man.Version, strings.Join(parts, ", "))
		// A plugin's commands answer to its own name, so a plugin named
		// for a core namespace can't have any. The app would only say so
		// when someone enabled it; saying it here is the point of
		// validating.
		if ns := man.Namespace(); ns != "" && cli.Reserved(ns) {
			fmt.Fprintf(stderr, "  %q is a core namespace, so this plugin's commands can't answer to it: rename the plugin or drop its commands\n", ns)
			return 1
		}
		return 0
	case "test":
		var log io.Writer
		if vals.Bool("verbose") {
			log = stderr
		}
		report, err := plugintest.Run(src, plugintest.Options{Filter: vals.String("run"), Log: log})
		if err != nil {
			fmt.Fprintln(stderr, "reproxy:", err)
			return 1
		}
		plugintest.Print(stdout, src.Name, report, vals.Bool("verbose"))
		if !report.Passed() {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "reproxy: there is no plugins %s\n", action)
	return 2
}

// writeTypes copies the SDK next to a plugin and points a tsconfig.json at
// it, so an editor type-checks the plugin against the SDK it will run with.
// An existing tsconfig.json is left alone.
func writeTypes(dir string) error {
	out := filepath.Join(dir, ".reproxy")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	err := fs.WalkDir(sdk.Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sdk.Files(), p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(out, p), data, 0o644)
	})
	if err != nil {
		return err
	}
	tsconfig := filepath.Join(dir, "tsconfig.json")
	if _, err := os.Stat(tsconfig); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(tsconfig, []byte(strings.TrimLeft(`
{
  "compilerOptions": {
    "target": "ES2017",
    "lib": ["ES2020"],
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "strict": true,
    "noEmit": true,
    "types": [],
    "paths": {
      "reproxy": ["./.reproxy/index.ts"],
      "reproxy/testing": ["./.reproxy/testing.ts"]
    }
  },
  "include": ["**/*.ts", ".reproxy/globals.d.ts"],
  "exclude": ["node_modules"]
}
`, "\n")), 0o644)
}
