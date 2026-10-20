//go:build !headless

package main

import (
	"reproxy/internal/app"
	"reproxy/internal/ui"
)

// launchUI opens the reProxy window. This is the normal build; the Qt UI is
// linked in. The headless build (ui_headless.go, built with -tags headless)
// leaves Qt out entirely, so the command-line tools — reproxy plugins,
// findings, flows and the rest — build without a Qt toolchain.
func launchUI(a *app.App) int {
	ui.Version = version
	return ui.Run(a)
}
