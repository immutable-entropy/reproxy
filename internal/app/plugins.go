package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/plugin/registry"
	"reproxy/internal/proxy"
	"reproxy/internal/randid"
	"reproxy/internal/replay"
	"reproxy/internal/upstream"
	"reproxy/internal/websocket"
)

// Plugins are wired to core here: the manager is handed the stores and the
// dial functions, and its dispatcher is subscribed to every store whose
// events a plugin can target. Each subscriber starts with one atomic load,
// so with no plugin enabled (the default) traffic pays nothing more.
//
// A plugin that sends traffic through the proxy authenticates to it with
// SOCKS5 username/password: the username names the plugin and the depth
// (plugin.OriginLabel), the password is a random secret made per process
// and never handed to plugin code. The proxy's origin resolver turns that
// back into the flow's Source and Origin.

// newPlugins builds the plugin manager. It runs at the end of New, once
// the stores and the proxy exist.
func (a *App) newPlugins() {
	// A plugin authenticates its own traffic with this, and pluginOrigin
	// compares it in constant time, so it has to be unguessable.
	a.pluginSecret = randid.Hex(16)

	a.plugins = plugin.NewManager(&plugin.Deps{
		DataDir:      a.DataDir(),
		Store:        a.store,
		Intercept:    a.intercept,
		Findings:     a.findings,
		Targets:      a.targets,
		Entities:     a.entities,
		Files:        a.files,
		Send:         replay.Send,
		Dial:         a.dialResolved,
		ProxyDial:    a.dialThroughProxy,
		Secrets:      a.secrets,
		EngagementDB: a.engagementDB,
		Log:          a.log,
	})
	d := a.plugins.Dispatcher()
	d.SetAllEntities(a.Effective().Targets.AllEntityHooks)
	a.store.Subscribe(d.OnFlow)
	a.store.Subscribe(d.OnWSMessage)
	a.findings.Subscribe(d.OnFinding)
	a.entities.Subscribe(d.OnEntity)
	a.files.Subscribe(d.OnFile)
	a.intercept.Subscribe(d.OnHeld)
	a.targets.Subscribe(func() {
		list := a.targets.List()
		out := make([]map[string]any, len(list))
		for i, t := range list {
			out[i] = map[string]any{"id": t.ID, "kind": string(t.Kind), "value": t.Value, "name": t.Label(), "color": t.Color, "enabled": t.Enabled}
		}
		d.Emit(manifest.TargetOnChanged, map[string]any{"targets": out})
	})
}

// startPlugins finds the plugins and starts the enabled ones. It runs in
// the background once the proxy is up: bundling takes a moment, and nothing
// should wait on it.
func (a *App) startPlugins() {
	go func() {
		a.plugins.Rescan()
		a.applyPlugins()
	}()
}

// applyPlugins hands the plugin manager the current settings. It is
// serialised and reads the settings when it runs, so however applies
// overlap, the last one leaves the plugins as the settings now say.
func (a *App) applyPlugins() {
	a.pluginMu.Lock()
	defer a.pluginMu.Unlock()
	a.plugins.Apply(pluginConfig(a.Settings().Plugins))
}

func pluginConfig(p config.Plugins) plugin.Config {
	out := plugin.Config{AllowRelease: p.AllowRelease, AllowHold: p.AllowHold, Overrides: make(map[string]plugin.Override, len(p.Overrides))}
	for name, o := range p.Overrides {
		out.Overrides[name] = plugin.Override{Status: manifest.Status(o.Status), Settings: o.Settings}
	}
	return out
}

// Plugins implements ui.Controller: the plugins, for the Plugins window.
func (a *App) Plugins() *plugin.Manager { return a.plugins }

// wsHandler adapts the plugin manager to proxy.WSHandler, so the plugin
// package needn't import proxy.
type wsHandler struct{ m *plugin.Manager }

func (h wsHandler) Edits(s intercept.Subject) bool { return h.m.WSEdits(s) }
func (h wsHandler) Decide(ctx context.Context, msg proxy.WSMessageInfo) websocket.Decision {
	return h.m.WSDecide(ctx, msg.FlowID, msg.Dir, msg.Type, msg.Data)
}
func (h wsHandler) Opened(flowID uint64, _ intercept.Subject) { h.m.WSOpened(flowID) }
func (h wsHandler) Closed(flowID uint64)                      { h.m.WSClosed(flowID) }

// SavePluginSecrets implements ui.Controller: it stores a plugin's secret
// settings (an empty value removes one) and hands the plugin the new
// values.
func (a *App) SavePluginSecrets(name string, values map[string]string) error {
	if err := a.secrets.SetPluginSecrets(name, values); err != nil {
		return err
	}
	go a.applyPlugins()
	return nil
}

// dialThroughProxy connects through reproxy's own proxy, labelled as a
// plugin's, so the exchange is recorded as that plugin's traffic.
func (a *App) dialThroughProxy(ctx context.Context, network, addr, label string) (net.Conn, error) {
	proxyAddr, err := a.ProxyStatus()
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1" // listening on every interface: loopback is one
	}
	d := upstream.NewDialer([]upstream.Hop{{
		Kind: upstream.SOCKS5, Addr: net.JoinHostPort(host, port), Username: label, Password: a.pluginSecret,
	}})
	return d.DialContext(ctx, network, addr)
}

// pluginOrigin implements proxy.OriginFunc: a connection that authenticated
// with this process's plugin secret is the named plugin's.
func (a *App) pluginOrigin(user, pass string) (capture.Source, string, bool) {
	if subtle.ConstantTimeCompare([]byte(pass), []byte(a.pluginSecret)) != 1 {
		return "", "", false
	}
	origin, ok := plugin.ParseLabel(user)
	if !ok {
		return "", "", false
	}
	return capture.SourcePlugin, origin, true
}

// engagementDB is the open engagement's database, for plugins' storage.
func (a *App) engagementDB() *sql.DB {
	if cur := a.recorder.Current(); cur != nil {
		return cur.DB()
	}
	return nil
}

// notifyEngagement tells plugins when the open engagement changes. It runs
// at the end of followEngagement.
func (a *App) notifyEngagement() {
	dir, name := "", ""
	if e, ok := a.engagements.Current(); ok {
		dir, name = e.Dir, e.Label()
	}
	if dir == a.lastEngagement[0] {
		return
	}
	prev := a.lastEngagement
	a.lastEngagement = [2]string{dir, name}
	if a.plugins == nil {
		return
	}
	d := a.plugins.Dispatcher()
	if prev[0] != "" {
		d.Emit(manifest.EngagementOnClosed, map[string]any{"name": prev[1], "path": prev[0]})
	}
	if dir != "" {
		d.Emit(manifest.EngagementOnOpened, map[string]any{"name": name, "path": dir})
	}
}

// ---- Remote plugins ----------------------------------------------------------

// pluginHTTP is the client plugin repositories are fetched with: through the
// configured upstream proxy chain, like the rest of the app's outbound
// traffic, with a short timeout and a capped read.
func (a *App) pluginGetter() registry.Getter {
	if a.pluginGet != nil {
		return a.pluginGet // a test's getter
	}
	client := &http.Client{
		Timeout:   45 * time.Second,
		Transport: &http.Transport{DialContext: a.dialUpstream, ForceAttemptHTTP2: true},
	}
	return func(ctx context.Context, url string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", "reproxy")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, registry.MaxTotal))
		return body, resp.StatusCode, err
	}
}

// AvailablePlugins implements ui.Controller: the plugins the given
// repositories offer, each marked whether it is already installed in the
// plugins directory, at which version, and whether this one updates it.
func (a *App) AvailablePlugins(ctx context.Context, repos []string) ([]registry.Plugin, []error) {
	get := a.pluginGetter()
	var out []registry.Plugin
	var errs []error
	for _, repo := range repos {
		plugins, err := registry.List(ctx, get, repo)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, plugins...)
	}
	installed := a.installedPlugins()
	for i := range out {
		in, ok := installed[out[i].Name]
		if !ok {
			continue
		}
		out[i].Installed = true
		out[i].InstalledVersion = in.version
		// A copy put in the folder by hand has no record of where it came
		// from, but a repository offering a newer version of that name is
		// still an update worth raising: it is marked ByHand so the user is
		// told, before they accept it, that their own files are what will be
		// replaced. A copy that came from another repository is left alone:
		// that repository is its source, and a plugin of the same name
		// somewhere else is a different plugin.
		out[i].InstalledByHand = in.origin == nil
		fromHere := in.origin == nil || in.origin.Same(out[i].Origin())
		out[i].Update = fromHere && newerVersion(out[i].Version, in.version)
	}
	return out, errs
}

// installedPlugin is what AvailablePlugins needs of an installed plugin.
type installedPlugin struct {
	version string
	origin  *registry.Origin // nil: not installed from a repository
}

// installedPlugins is the plugins present in the user's plugins directory,
// by name, with their versions and where each was installed from.
func (a *App) installedPlugins() map[string]installedPlugin {
	out := map[string]installedPlugin{}
	for _, in := range a.plugins.List() {
		var p installedPlugin
		if in.Manifest != nil {
			p.version = in.Manifest.Version
		}
		p.origin = readOrigin(in.Path)
		out[in.Name] = p
	}
	return out
}

// readOrigin reads the record of where a plugin was installed from; nil
// when there is none (a plugin put in the folder by hand).
func readOrigin(dir string) *registry.Origin {
	data, err := os.ReadFile(filepath.Join(dir, registry.OriginFile))
	if err != nil {
		return nil
	}
	var o registry.Origin
	if json.Unmarshal(data, &o) != nil || o.Repo == "" {
		return nil
	}
	return &o
}

// newerVersion reports whether version remote is newer than local. A
// version that doesn't parse is never newer.
func newerVersion(remote, local string) bool {
	r, err := manifest.ParseVersion(remote)
	if err != nil {
		return false
	}
	l, err := manifest.ParseVersion(local)
	if err != nil {
		return false
	}
	return r.Compare(l) > 0
}

// InstallPlugin implements ui.Controller: it downloads a plugin into the
// plugins directory and reloads, leaving it disabled so the user reviews and
// enables it rather than running remote code on download. It returns the
// installed plugin's name.
func (a *App) InstallPlugin(ctx context.Context, p registry.Plugin) (string, error) {
	if err := a.downloadPlugin(ctx, p); err != nil {
		return "", err
	}
	// Record it disabled, so it doesn't run until the user enables it, and
	// reload so it shows at once.
	a.disablePlugin(p.Name)
	a.plugins.Rescan()
	a.applyPlugins()
	return p.Name, nil
}

// UpdatePlugin implements ui.Controller: it replaces an installed plugin
// with the given one from the repository it was installed from, and
// reloads. Unlike an install, the plugin keeps the status the user gave it
// (an enabled plugin is restarted on the new version) and its settings: the
// user chose to update it, one plugin at a time. A plugin that wasn't
// installed from that repository directory is refused, so an update can't
// replace a plugin put in the folder by hand, or one from elsewhere.
func (a *App) UpdatePlugin(ctx context.Context, p registry.Plugin) (string, error) {
	in, ok := a.plugins.Get(p.Name)
	if !ok {
		return "", fmt.Errorf("%s is not installed", p.Name)
	}
	// A plugin that came from another repository is not updated from this
	// one. A plugin with no record at all was put in the folder by hand, and
	// the user has been told (registry.Plugin.InstalledByHand) that updating
	// replaces their own copy.
	if o := readOrigin(in.Path); o != nil && !o.Same(p.Origin()) {
		return "", fmt.Errorf("%s was not installed from %s, so it can't be updated from there", p.Name, p.Repo)
	}
	if err := a.downloadPlugin(ctx, p); err != nil {
		return "", err
	}
	a.plugins.Rescan()
	a.applyPlugins()
	return p.Name, nil
}

// downloadPlugin downloads a plugin, checks it, and puts it in the plugins
// directory in place of any earlier copy, recording where it came from.
func (a *App) downloadPlugin(ctx context.Context, p registry.Plugin) error {
	if p.Name == "" || !validPluginName(p.Name) {
		return fmt.Errorf("the plugin's name %q is not valid", p.Name)
	}
	dir := plugin.UserDir(a.DataDir())
	tmp, err := os.MkdirTemp(dir, ".download-*")
	if err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if tmp, err = os.MkdirTemp(dir, ".download-*"); err != nil {
			return err
		}
	}
	defer os.RemoveAll(tmp)

	root, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer root.Close()
	write := func(rel string, data []byte) error {
		if d := filepath.Dir(rel); d != "." {
			if err := root.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		return root.WriteFile(rel, data, 0o644)
	}
	if err := p.Download(ctx, a.pluginGetter(), write); err != nil {
		return err
	}

	// Validate what was downloaded before it is put in place: a manifest
	// that doesn't parse, or a function a target names that isn't exported,
	// is refused rather than installed broken.
	man, problems := plugin.Inspect(plugin.Source{Name: p.Name, FS: root.FS(), Path: tmp})
	if man == nil {
		return fmt.Errorf("the download has no readable %s", "plugin.json")
	}
	if man.Name != p.Name {
		return fmt.Errorf("the downloaded plugin is named %q, not %q", man.Name, p.Name)
	}
	if len(problems) > 0 {
		return fmt.Errorf("the downloaded plugin has problems: %s", strings.Join(problems, "; "))
	}

	origin, _ := json.Marshal(p.Origin())
	if err := root.WriteFile(registry.OriginFile, origin, 0o644); err != nil {
		return err
	}
	root.Close() // before the rename: Windows won't move an open directory

	// Move it into place, replacing any earlier copy. The earlier copy is
	// moved aside first and put back if the new one can't take its place,
	// so a failed update leaves the plugin as it was.
	dest := filepath.Join(dir, p.Name)
	old := filepath.Join(dir, ".old-"+filepath.Base(tmp))
	hadOld := false
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
		hadOld = true
	}
	if err := os.Rename(tmp, dest); err != nil {
		if hadOld {
			_ = os.Rename(old, dest)
		}
		return err
	}
	if hadOld {
		_ = os.RemoveAll(old)
	}
	return nil
}

// disablePlugin records a disabled override for a plugin and saves the
// config, so an installed plugin never runs on its own.
func (a *App) disablePlugin(name string) {
	a.mu.Lock()
	next := a.cfg
	over := map[string]config.PluginOverride{}
	for k, v := range next.Plugins.Overrides {
		over[k] = v
	}
	o := over[name]
	o.Status = "disabled"
	over[name] = o
	next.Plugins.Overrides = over
	a.cfg = next
	err := config.Save(a.configPath, next)
	a.mu.Unlock()
	if err != nil {
		a.log.Error("saving the installed plugin's setting", "plugin", name, "err", err)
	}
}

// validPluginName mirrors the manifest's name rule, so a repository can't
// name a plugin something that would escape the plugins directory.
func validPluginName(name string) bool {
	if len(name) < 2 || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}
