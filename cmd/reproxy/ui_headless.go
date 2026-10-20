//go:build headless

package main

import (
	"fmt"
	"os"

	"reproxy/internal/app"
)

// launchUI stands in for the window in a headless build, which links no Qt.
// Such a build is the command line (reproxy <command>) and the `-headless`
// server; it has no window to open, so a bare `reproxy` says so rather than
// starting a proxy nobody can see. runHeadless handles `-headless`, and the
// command line never reaches here.
func launchUI(_ *app.App) int {
	fmt.Fprintln(os.Stderr, "reproxy: this is a headless build with no window. "+
		"Use `reproxy -headless` for the proxy/MCP server, or `reproxy <command>` for the command line.")
	return 2
}
