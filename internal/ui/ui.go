// Package ui is the Qt desktop interface.
//
// It is the only package that depends on Qt (via miqt/cgo). Everything it
// needs from the rest of the application comes through Controller, which
// keeps the proxy and capture logic testable without a display.
//
// Threading: Qt widgets may only be touched on the main thread. Anything
// arriving from another goroutine (store events) is handed over with
// mainthread.Start.
package ui

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/agent"
	"reproxy/internal/approval"
	"reproxy/internal/browser"
	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/crawl"
	"reproxy/internal/engagement"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/focus"
	"reproxy/internal/intercept"
	"reproxy/internal/mcp"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/registry"
	"reproxy/internal/replay"
	"reproxy/internal/target"
	"reproxy/internal/terminal"
)

// AppName is shown in the window title and used for Qt's settings scope.
const AppName = "reProxy"

// DesktopFileName and IconName must match what the packagers install:
// /usr/share/applications/<DesktopFileName>.desktop and the hicolor theme
// entries for <IconName> (see packaging/deb/build.sh).
const (
	DesktopFileName = "reproxy"
	IconName        = "reproxy"
)

// Controller is what the UI needs from the application.
type Controller interface {
	// Store holds the captured traffic shown in the request list.
	Store() *capture.Store
	// ExportSourceMap recovers the original source files embedded in the
	// source map a flow carries and records them as the engagement's
	// recovered files, returning what it wrote. by and author say who asked,
	// which each file records. It backs the traffic list's "Export recovered
	// sources…" item.
	ExportSourceMap(flowID uint64, by files.Source, author string) (files.Result, error)
	// Files is what the work recovered: the engagement's recovered files,
	// which back the Files section.
	Files() *files.Store
	// Interceptor holds messages for editing and is switched on and off
	// from the interception toggle at the top right.
	Interceptor() *intercept.Interceptor
	// Settings returns the settings currently in effect.
	Settings() config.Config
	// ApplySettings applies and persists new settings. On error nothing
	// changes and the message is shown to the user.
	ApplySettings(config.Config) error
	// Effective is Settings with the open engagement's overrides applied:
	// what is actually in force.
	Effective() config.Config
	// EngagementSettings returns the open engagement's overrides and its
	// name (ok false with none open); ApplyEngagementSettings saves them in
	// the engagement's folder and puts them into effect.
	EngagementSettings() (settings config.EngagementSettings, name string, ok bool)
	ApplyEngagementSettings(config.EngagementSettings) error
	// SuppressHosts adds hosts to the user's suppression list (Settings →
	// Global → Targets) or, with engagementOnly, the open engagement's, so
	// they are no longer suggested as related, and saves it once.
	SuppressHosts(hosts []string, engagementOnly bool) error
	// ProxyStatus reports where the proxy is listening, or why it isn't.
	ProxyStatus() (addr string, err error)
	// UIState returns the window layout saved last session (zero value:
	// use the defaults); SaveUIState stores it for the next one.
	UIState() config.UIState
	SaveUIState(config.UIState) error
	// InstalledBrowsers lists browsers the Browser button can launch.
	InstalledBrowsers() []browser.Browser
	// LaunchBrowser starts the configured browser pointed at the proxy.
	LaunchBrowser() (*browser.Session, error)
	// CACertPEM returns the proxy's CA certificate (PEM) for the user to
	// download and install so HTTPS interception is trusted; CAFingerprint
	// is its SHA-256 fingerprint, shown for verification.
	CACertPEM() []byte
	CAFingerprint() string
	// ReplaySessions holds this process's Replay sessions; Replay windows
	// are views onto them, shared with agents.
	ReplaySessions() *replay.Sessions
	// Focus records what the user is looking at, for agents to resolve
	// "this request".
	Focus() *focus.Tracker
	// Agents manages the agent chat sessions shown in the Agents panel.
	Agents() *agent.Manager
	// Terminals runs the shells shown in the Sessions panel's Terminal tab.
	Terminals() *terminal.Manager
	// AgentConfig returns the configured agent profiles.
	AgentConfig() config.Agents
	// SaveAgentConfig saves the profiles and their API keys (by profile ID;
	// an empty key removes it). Keys of profiles no longer listed are
	// removed.
	SaveAgentConfig(a config.Agents, keys map[string]string) error
	// APIKey returns a profile's saved API key, or "".
	APIKey(profileID string) string
	// UpstreamPassword returns the saved password for an upstream proxy
	// (by its ID), or "". SaveUpstreamPasswords saves those given and
	// removes those given as ""; proxies not mentioned keep theirs (the
	// global chain and engagements' chains share the secrets file).
	UpstreamPassword(proxyID string) string
	SaveUpstreamPasswords(byID map[string]string) error
	// MCPToken returns the bearer token MCP clients must send;
	// RegenerateMCPToken replaces it.
	MCPToken() (string, error)
	RegenerateMCPToken() (string, error)
	// MCPStatus reports whether the shared MCP server is listening; StartMCP
	// and StopMCP control it (it runs on the hub, driven over the group).
	MCPStatus() mcp.Status
	StartMCP() error
	StopMCP() error
	// Approvals is the broker holding agent actions that await the user's
	// Approve/Deny decision.
	Approvals() *approval.Broker
	// Targets holds the hosts in scope. The proxy consults the same store
	// to decide what is collected at all.
	Targets() *target.Store
	// Engagements owns the open engagement: the File menu's New, Open and
	// Close go through it, and opening one brings its targets with it.
	Engagements() *engagement.Manager
	// Findings holds what the work turned up, for the Findings section. The
	// same store is written by the create_finding tool, so a finding an
	// agent records appears in the list as it is made.
	Findings() *findings.Store
	// Entities is the engagement's Entity Explorer — its domains, hosts and
	// URLs — which the Explorer section draws. Captured traffic is indexed
	// into it in the background, plugins add what they discover, and each
	// target its domain or address.
	Entities() *entities.Store
	// Crawler is the engagement's Autocrawl, which the Autocrawl window
	// starts, stops and follows.
	Crawler() *crawl.Crawler
	// Plugins finds and runs the plugins, for the Plugins window.
	Plugins() *plugin.Manager
	// SavePluginSecrets stores a plugin's secret settings in the secrets
	// file (an empty value removes one).
	SavePluginSecrets(name string, values map[string]string) error
	// AvailablePlugins lists the plugins the given repositories offer, with
	// any errors reaching them (so a bad repository doesn't hide the rest).
	AvailablePlugins(ctx context.Context, repos []string) ([]registry.Plugin, []error)
	// InstallPlugin downloads a plugin into the plugins directory, disabled,
	// and reloads; it returns the installed name.
	InstallPlugin(ctx context.Context, p registry.Plugin) (string, error)
	// UpdatePlugin replaces an installed plugin with a newer version from
	// the repository it was installed from, keeping its status and
	// settings, and reloads; it returns the plugin's name.
	UpdatePlugin(ctx context.Context, p registry.Plugin) (string, error)
	// DataDir is where all of the app's persistent state lives. The New
	// Engagement wizard suggests a folder under it.
	DataDir() string
}

// Run creates the Qt application, shows the main window and blocks until it
// is closed. It must be called from the main goroutine.
func Run(ctrl Controller) int {
	// Only argv[0] is passed through: the CLI owns flag parsing, and Qt
	// options such as -platform can be set with QT_QPA_PLATFORM instead.
	app := qt.NewQApplication(os.Args[:1])
	qt.QCoreApplication_SetApplicationName(AppName)
	qt.QGuiApplication_SetApplicationDisplayName(AppName)

	// Tie the window to the installed desktop entry (reproxy.desktop, from
	// the package) so the shell matches the two: that is what lets the task
	// bar show our icon and lets the user pin it. Wayland uses the desktop
	// file name alone; X11 shells generally match on it too, falling back to
	// WM_CLASS, which Qt sets from argv[0] ("reproxy") either way.
	qt.QGuiApplication_SetDesktopFileName(DesktopFileName)
	// Running from a source tree there is no installed icon theme entry, and
	// fromTheme then returns a null icon, which is harmless: the window keeps
	// Qt's default.
	qt.QGuiApplication_SetWindowIcon(qt.QIcon_FromTheme(IconName))
	defer initTheme(app, themeKindFor(ctrl.Settings().Theme))()

	win := newMainWindow(ctrl)
	win.show()

	// Ctrl+C in the terminal or a SIGTERM quits the same way File → Quit
	// does, so the layout is saved and launched browsers are closed.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; ok {
			mainthread.Start(qt.QCoreApplication_Quit)
		}
	}()

	code := qt.QApplication_Exec()
	win.close()
	return code
}
