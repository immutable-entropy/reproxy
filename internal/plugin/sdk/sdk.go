// Package sdk is the library plugins import: the "reproxy" module (its
// models, and the actions a plugin can take) and "reproxy/testing" (the
// harness a plugin's unit tests run under). Both are TypeScript embedded
// here and handed to the bundler as virtual modules, so a plugin never needs
// a copy on disk to build; `reproxy plugin types` writes one for editors.
//
// The modules are thin: what they call is the host object package plugin
// installs in each plugin's runtime as __reproxy_host.
package sdk

import (
	"embed"
	"io/fs"
)

//go:embed index.ts testing.ts globals.d.ts
var files embed.FS

// Module names, as a plugin imports them.
const (
	Main    = "reproxy"
	Testing = "reproxy/testing"
)

// Modules returns the SDK's modules by import name, as TypeScript source.
func Modules() map[string]string {
	return map[string]string{Main: read("index.ts"), Testing: read("testing.ts")}
}

// Files is the SDK as files, for writing out next to a plugin for its
// editor.
func Files() fs.FS { return files }

func read(name string) string {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err) // embedded: a failure is a build bug
	}
	return string(b)
}
