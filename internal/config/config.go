// Package config loads and saves the user's persistent settings.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"reproxy/internal/atomicfile"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/target"
	"reproxy/internal/upstream"
)

// DefaultProxyPort is the proxy's listen port used when none is configured.
const DefaultProxyPort = 7193

// DefaultProxyHost is the interface the proxy listens on by default. The
// proxy has no authentication, so it is loopback-only unless the user
// chooses otherwise.
const DefaultProxyHost = "127.0.0.1"

// DefaultMCPPort is the MCP server's default listen port.
const DefaultMCPPort = 3917

// MCP policies for clients outside the app (external agents). In-app agent
// sessions choose Ask or Auto per session instead.
const (
	// PolicyAsk asks the user in the app before any tool that sends or
	// changes traffic runs.
	PolicyAsk = "ask"
	// PolicyAuto lets such tools run without asking (the scope rule still
	// applies).
	PolicyAuto = "auto"
	// PolicyReadOnly refuses them outright.
	PolicyReadOnly = "read_only"
)

// Config is the set of user-adjustable settings. Add new fields with a
// sensible zero-value fallback in Default/normalize so older config files
// keep loading.
type Config struct {
	// ProxyHost is the IP address of the interface the proxy listens on;
	// 0.0.0.0 or :: means all interfaces.
	ProxyHost string `json:"proxy_host"`
	// ProxyPort is the TCP port the proxy listens on, for every protocol it
	// speaks (SOCKS5, SOCKS4/4a, HTTP and HTTPS).
	ProxyPort int `json:"proxy_port"`
	// Intercept configures which messages are held when interception is on.
	Intercept intercept.Settings `json:"intercept"`
	// Browser configures Tools → Open Browser.
	Browser Browser `json:"browser"`
	// MCP configures the MCP server that agents (in-app and external) use.
	MCP MCP `json:"mcp"`
	// AgentWarningSeen records that the user has been shown what an agent
	// can read of the captured traffic, which is asked before an agent is
	// added (Settings → Agents) and only until they have seen it. It is an
	// acknowledgement, not a preference: nothing in the UI turns it back
	// on, and deleting the settings file asks again.
	AgentWarningSeen bool `json:"agent_warning_seen,omitempty"`
	// Theme is the colour theme: "light" (default), "dark" or "matrix".
	// An empty or unknown value is treated as "light".
	Theme string `json:"theme,omitempty"`
	// NavPosition anchors the main window's navigation bar: "left"
	// (default), "top", "right", "bottom" or "hidden". An empty or unknown
	// value is treated as "left".
	NavPosition string `json:"nav_position,omitempty"`
	// Targets configures how targets scope the traffic collected.
	Targets Targets `json:"targets"`
	// Findings holds the finding types a finding can be classified with.
	// The findings themselves live in the open engagement's capture.db
	// (see package findings); the types are the user's own taxonomy, so
	// they are kept here and follow them across engagements.
	Findings Findings `json:"findings"`
	// Upstream is the chain of proxies outbound connections are routed
	// through. Empty (the default) means reProxy connects to targets
	// directly.
	Upstream upstream.Chain `json:"upstream"`
	// Plugins holds the user's choices about plugins: which run, and their
	// settings. A plugin's own plugin.json holds the defaults.
	Plugins Plugins `json:"plugins"`
}

// Plugins holds the settings on the Plugins page. The plugins themselves are
// found in the data directory's plugins/ (and in the binary); only what the
// user changed about them is kept here. Secret settings go to the secrets
// file instead.
type Plugins struct {
	// Overrides are the user's choices, by plugin name.
	Overrides map[string]PluginOverride `json:"overrides,omitempty"`
	// AllowRelease lets plugins' intercept hooks forward or drop a held
	// message before the user sees it. Off, everything interception holds
	// still reaches the user, edited or not.
	AllowRelease bool `json:"allow_release"`
	// AllowHold lets plugins have the proxy hold traffic for them (a
	// target's hold: true) even while interception is off. Off, plugins'
	// intercept hooks only see what the user's interception holds.
	AllowHold bool `json:"allow_hold"`
	// Repositories are the GitHub repositories the user can browse and
	// install plugins from (the Plugins window's Repositories section).
	Repositories []string `json:"repositories,omitempty"`
}

// DefaultPluginRepository is the plugin repository every install starts
// with.
const DefaultPluginRepository = "https://github.com/immutable-entropy/reproxy-plugins"

// PluginOverride is what the user changed about one plugin.
type PluginOverride struct {
	// Status is "enabled" or "disabled"; empty keeps the plugin's default.
	Status string `json:"status,omitempty"`
	// Settings are the values the user set, by key, as JSON. Settings the
	// user hasn't touched follow the plugin's defaults.
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
}

// Targets holds the settings on the Targets page. The targets themselves
// live in the open engagement's targets.json (see packages target and
// engagement), because the UI edits them one at a time rather than through
// the settings dialog.
type Targets struct {
	// Required keeps traffic that matches no target out of the capture
	// entirely: the proxy relays it untouched and records nothing. It is on
	// by default, so a session only collects what the user is authorised to
	// be looking at.
	Required bool `json:"required"`
	// Suppressed are the hosts never suggested as related to a target
	// (Targets → Related), in the order they were added.
	Suppressed []target.Suppression `json:"suppressed,omitempty"`
	// AllEntityHooks runs plugins' entity created and updated hooks for
	// every entity in the Explorer, related hosts' included. Off (the
	// default), they run only for entities that belong to a target.
	AllEntityHooks bool `json:"all_entity_hooks,omitempty"`
}

// Findings holds the settings on the Findings page: the types findings can
// be classified with, each with the colour its pill is drawn in. Plugins
// will be able to declare types of their own at runtime; those are not
// written here.
type Findings struct {
	Types []findings.Type `json:"types"`
}

// DefaultFindingTypes is the taxonomy a first launch starts with. They are
// a starting point, not a fixed list: Settings → Findings adds, renames,
// recolours and removes them.
func DefaultFindingTypes() []findings.Type {
	names := []string{"Credential", "Sensitive Data", "Unexpected Response", "Error", "Information"}
	out := make([]findings.Type, 0, len(names))
	used := make([]string, 0, len(names))
	for _, name := range names {
		color := findings.PickColor(used)
		used = append(used, color)
		out = append(out, findings.Type{ID: findings.Slug(name), Name: name, Color: color})
	}
	return out
}

// Browser is the browser launched preconfigured with the proxy.
type Browser struct {
	// Path is the browser executable; empty picks the first installed one.
	Path string `json:"path"`
}

// MCP is the MCP server's configuration. Its bearer token is kept in the
// secrets file, not here.
type MCP struct {
	// Host is the IP address the server listens on; loopback by default.
	Host string `json:"host"`
	// Port is its TCP port.
	Port int `json:"port"`
	// ExternalPolicy is PolicyAsk, PolicyAuto or PolicyReadOnly.
	ExternalPolicy string `json:"external_policy"`
}

// Addr returns the host:port the MCP server listens on.
func (m MCP) Addr() string {
	return net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
}

// Default returns the settings used on first launch.
func Default() Config {
	return Config{
		ProxyHost: DefaultProxyHost,
		ProxyPort: DefaultProxyPort,
		Intercept: intercept.DefaultSettings(),
		MCP: MCP{
			Host:           DefaultProxyHost,
			Port:           DefaultMCPPort,
			ExternalPolicy: PolicyAsk,
		},
		Findings: Findings{Types: DefaultFindingTypes()},
		// Collecting only what a target covers is the safe default: a config
		// file written before this setting existed keeps it, because Load
		// starts from these defaults.
		Targets:  Targets{Required: true},
		Upstream: upstream.DefaultChain(),
		Plugins:  Plugins{AllowRelease: true, AllowHold: true, Repositories: []string{DefaultPluginRepository}},
	}
}

// Validate reports whether c can be applied.
func (c Config) Validate() error {
	if net.ParseIP(c.ProxyHost) == nil {
		return fmt.Errorf("proxy host %q is not an IP address", c.ProxyHost)
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return fmt.Errorf("proxy port %d is out of range (1-65535)", c.ProxyPort)
	}
	if err := c.Intercept.Validate(); err != nil {
		return fmt.Errorf("interception: %w", err)
	}
	if net.ParseIP(c.MCP.Host) == nil {
		return fmt.Errorf("MCP host %q is not an IP address", c.MCP.Host)
	}
	if c.MCP.Port < 1 || c.MCP.Port > 65535 {
		return fmt.Errorf("MCP port %d is out of range (1-65535)", c.MCP.Port)
	}
	switch c.MCP.ExternalPolicy {
	case PolicyAsk, PolicyAuto, PolicyReadOnly:
	default:
		return fmt.Errorf("unknown MCP external policy %q", c.MCP.ExternalPolicy)
	}
	if err := c.Findings.Validate(); err != nil {
		return fmt.Errorf("findings: %w", err)
	}
	if err := c.Upstream.Validate(); err != nil {
		return fmt.Errorf("upstream proxy: %w", err)
	}
	if err := target.ValidateSuppressions(c.Targets.Suppressed); err != nil {
		return fmt.Errorf("targets: %w", err)
	}
	for name, o := range c.Plugins.Overrides {
		switch o.Status {
		case "", "enabled", "disabled":
		default:
			return fmt.Errorf("plugin %s: status %q must be enabled or disabled", name, o.Status)
		}
	}
	for i, p := range c.Upstream.Active() {
		if p.Port == c.ProxyPort && sameHost(p.Host, c.ProxyHost) {
			return fmt.Errorf("upstream proxy %d points at %s, which is reProxy's own listen address: "+
				"connections would loop back into the proxy forever", i+1, p.Addr())
		}
	}
	return nil
}

// Validate reports whether the finding types can be saved: each one usable,
// and no two sharing an id, since a finding stores the id alone.
func (f Findings) Validate() error {
	seen := make(map[string]bool, len(f.Types))
	for _, t := range f.Types {
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("two types share the id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// sameHost reports whether an upstream proxy's host would reach our own
// listener. A wildcard listen address answers on every interface, and
// loopback is loopback whichever spelling is used.
func sameHost(proxyHost, listenHost string) bool {
	if strings.EqualFold(proxyHost, listenHost) {
		return true
	}
	a, b := net.ParseIP(proxyHost), net.ParseIP(listenHost)
	if a == nil || b == nil {
		return false
	}
	if b.IsUnspecified() {
		return true
	}
	if a.IsLoopback() && b.IsLoopback() {
		return true
	}
	return a.Equal(b)
}

// ProxyAddr returns the host:port the proxy listens on.
func (c Config) ProxyAddr() string {
	return net.JoinHostPort(c.ProxyHost, fmt.Sprint(c.ProxyPort))
}

// IsLoopback reports whether the proxy is only reachable from this machine.
func (c Config) IsLoopback() bool {
	ip := net.ParseIP(c.ProxyHost)
	return ip != nil && ip.IsLoopback()
}

// DataDir returns the per-user directory that holds all of the app's
// persistent state: %APPDATA%\reproxy on Windows, ~/.reproxy everywhere else.
// Anything saved between sessions belongs under it.
func DataDir() (string, error) {
	if runtime.GOOS == "windows" {
		appData, err := os.UserConfigDir() // %APPDATA%
		if err != nil {
			return "", fmt.Errorf("locating %%APPDATA%%: %w", err)
		}
		return filepath.Join(appData, "reproxy"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".reproxy"), nil
}

// DefaultPath returns the settings file location inside DataDir.
func DefaultPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config at path. A missing file is not an error: the
// defaults are returned so the first launch works without setup.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}

	c := Default()
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path atomically, creating parent directories as needed.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return writeJSON(path, c)
}

// writeJSON writes v as indented JSON to path, for every settings file in
// the data directory. They are the user's own: readable only by them.
func writeJSON(path string, v any) error {
	return atomicfile.WriteJSON(path, v, atomicfile.Private, 0o755)
}
