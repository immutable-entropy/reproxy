package app

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/files"
	"reproxy/internal/instance"
	"reproxy/internal/target"
	"reproxy/internal/upstream"
)

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func withPort(port int) config.Config {
	c := config.Default()
	c.ProxyPort = port
	return c
}

func newApp(t *testing.T, port int) (*App, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	a, err := New(Options{ConfigPath: path, PortOverride: port, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, path
}

// newAppOnHost builds an app whose saved config binds host:port, to exercise
// a startup bind failure that port auto-increment cannot recover from.
func newAppOnHost(t *testing.T, host string, port int) *App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.ProxyHost, cfg.ProxyPort = host, port
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{ConfigPath: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// A second app on a port already in use steps up to the next free one.
func TestProxyPortAutoIncrementsWhenBusy(t *testing.T) {
	port := freePort(t)
	first, _ := newApp(t, port)
	first.Start()
	if addr, err := first.ProxyStatus(); err != nil || !strings.HasSuffix(addr, strconv.Itoa(port)) {
		t.Fatalf("first proxy = %q, %v", addr, err)
	}
	second, _ := newApp(t, port) // same configured port
	second.Start()
	addr, err := second.ProxyStatus()
	if err != nil {
		t.Fatalf("second proxy didn't start: %v", err)
	}
	if !strings.HasSuffix(addr, strconv.Itoa(port+1)) {
		t.Fatalf("second proxy = %q, want port %d", addr, port+1)
	}
}

func TestApplySettingsMovesProxyAndSaves(t *testing.T) {
	a, path := newApp(t, freePort(t))
	a.Start()
	if _, err := a.ProxyStatus(); err != nil {
		t.Fatal(err)
	}

	next := freePort(t)
	if err := a.ApplySettings(withPort(next)); err != nil {
		t.Fatal(err)
	}
	addr, err := a.ProxyStatus()
	if err != nil || addr != "127.0.0.1:"+strconv.Itoa(next) {
		t.Fatalf("ProxyStatus = %q, %v", addr, err)
	}
	saved, err := config.Load(path)
	if err != nil || saved.ProxyPort != next {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
}

func TestApplySettingsBusyPortKeepsCurrent(t *testing.T) {
	a, path := newApp(t, freePort(t))
	a.Start()
	before, _ := a.ProxyStatus()

	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	err := a.ApplySettings(withPort(port))
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := a.ProxyStatus(); after != before {
		t.Fatalf("proxy moved to %s", after)
	}
	if a.Settings().ProxyPort == port {
		t.Fatal("settings updated despite failure")
	}
	if saved, _ := config.Load(path); saved.ProxyPort == port {
		t.Fatal("failed port was saved")
	}
}

func TestRecoversFromStartupFailureViaSettings(t *testing.T) {
	// A busy port is now recovered by auto-increment, so force a failure
	// auto-increment can't paper over: an address that isn't this machine's.
	a := newAppOnHost(t, "192.0.2.1", freePort(t))
	a.Start()
	if _, err := a.ProxyStatus(); err == nil {
		t.Fatal("expected startup error on an unroutable host")
	}

	if err := a.ApplySettings(withPort(freePort(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProxyStatus(); err != nil {
		t.Fatalf("still failing after new port: %v", err)
	}
}

func TestUIStatePersistsBesideConfig(t *testing.T) {
	a, path := newApp(t, freePort(t))
	if s := a.UIState(); s.Panels != nil {
		t.Fatalf("fresh UIState = %+v", s)
	}
	if err := a.SaveUIState(config.UIState{Panels: map[string]bool{"bottom": false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "ui-state.json")); err != nil {
		t.Fatal(err)
	}
	if s := a.UIState(); s.Panels["bottom"] != false || len(s.Panels) != 1 {
		t.Fatalf("UIState = %+v", s)
	}

	os.WriteFile(config.UIStatePath(path), []byte("garbage"), 0o644)
	if s := a.UIState(); s.Panels != nil {
		t.Fatalf("corrupt layout not ignored: %+v", s)
	}
}

func TestLaunchBrowserUsesConfiguredPathAndStopsOnClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell-script fake browser")
	}
	fake := filepath.Join(t.TempDir(), "fake-chrome")
	argsFile := fake + ".args"
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\nexec sleep 60\n"), 0o755)

	a, path := newApp(t, freePort(t))
	a.Start()
	cfg := a.Settings()
	cfg.Browser.Path = fake
	if err := a.ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}

	s, err := a.LaunchBrowser()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailedEarly(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	addr, _ := a.ProxyStatus()
	profile := filepath.Join(filepath.Dir(path), "browser", "fake-chrome")
	if !strings.Contains(string(args), "--proxy-server=socks5://"+addr) || !strings.Contains(string(args), "--user-data-dir="+profile) {
		t.Fatalf("browser args = %s", args)
	}

	a.Close()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("browser still running after Close")
	}
}

func TestLaunchBrowserNeedsRunningProxy(t *testing.T) {
	a := newAppOnHost(t, "192.0.2.1", freePort(t)) // unroutable: proxy can't start
	a.Start()
	if _, err := a.LaunchBrowser(); err == nil || !strings.Contains(err.Error(), "proxy isn't running") {
		t.Fatalf("err = %v", err)
	}
}

// Two processes sharing a data directory form one group: the first is the
// hub and can reach the second's traffic.
func TestAppsSharingADataDirFormOneGroup(t *testing.T) {
	dir := t.TempDir()
	open := func(port int) *App {
		a, err := New(Options{ConfigPath: filepath.Join(dir, "config.json"), PortOverride: port,
			Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		a.Start()
		t.Cleanup(func() { a.Close() })
		return a
	}
	first := open(freePort(t))
	second := open(freePort(t))
	if first.Node() == nil || !first.Node().IsHub() || second.Node().IsHub() {
		t.Fatal("expected the first app to be the hub")
	}
	second.Store().Add(capture.Flow{Method: "GET", Host: "two.test", Path: "/"})

	deadline := time.Now().Add(5 * time.Second)
	for len(first.Node().Router().Peers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("second app never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	inst, ok := first.Node().Router().Get(second.Node().ID())
	if !ok {
		t.Fatal("second app not routable")
	}
	page, err := inst.ListFlows(context.Background(), instance.FlowQuery{})
	if err != nil || len(page.Flows) != 1 || page.Flows[0].URL != "http://two.test/" {
		t.Fatalf("flows = %+v, %v", page, err)
	}
}

func TestSaveAgentConfigKeepsKeysInSecrets(t *testing.T) {
	a, path := newApp(t, freePort(t))
	ag := config.Agents{Profiles: []config.AgentProfile{
		{ID: "p1", Vendor: "anthropic", Name: "Claude", Model: "m"},
		{ID: "p2", Vendor: "ollama", Name: "Local", Model: "q"},
	}}
	if err := a.SaveAgentConfig(ag, map[string]string{"p1": "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	if a.APIKey("p1") != "sk-secret" || len(a.AgentConfig().Profiles) != 2 {
		t.Fatalf("key %q, profiles %+v", a.APIKey("p1"), a.AgentConfig())
	}
	data, _ := os.ReadFile(config.AgentsPath(path))
	if strings.Contains(string(data), "sk-secret") {
		t.Fatal("API key written to agents.json")
	}
	// Removing a profile removes its key.
	if err := a.SaveAgentConfig(config.Agents{Profiles: ag.Profiles[1:]}, nil); err != nil {
		t.Fatal(err)
	}
	if a.APIKey("p1") != "" {
		t.Fatal("removed profile's key kept")
	}
}

// Many hosts are suppressed in one save, each once.
func TestSuppressHostsSavesThemAllOnce(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	if err := a.SuppressHosts([]string{"a.test", "B.test:443", "a.test"}, false); err != nil {
		t.Fatal(err)
	}
	if got := a.Settings().Targets.Suppressed; len(got) != 2 || got[1].Pattern != "b.test" {
		t.Errorf("suppressed = %+v, want a.test and b.test", got)
	}
	if err := a.SuppressHosts([]string{"x.test"}, true); err == nil {
		t.Error("suppressed in an engagement with none open")
	}
}

func TestSuppressHostSavesAndHidesIt(t *testing.T) {
	a, path := newApp(t, freePort(t))
	if err := a.SuppressHost("CDN.test:443", false); err != nil {
		t.Fatal(err)
	}
	if err := a.SuppressHost("cdn.test", false); err != nil { // already suppressed: no second entry
		t.Fatal(err)
	}
	got := a.Settings().Targets.Suppressed
	if len(got) != 1 || got[0].Pattern != "cdn.test" {
		t.Fatalf("suppressed = %+v", got)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Targets.Suppressed) != 1 {
		t.Errorf("saved = %+v", saved.Targets.Suppressed)
	}
}

// The open engagement's overrides are in force while it is open, saved in
// its folder, and gone when it closes.
func TestEngagementSettingsFollowTheEngagement(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	dir := filepath.Join(filepath.Dir(configPath), "engagements", "acme")
	if _, _, ok := a.EngagementSettings(); ok {
		t.Fatal("engagement settings with none open")
	}
	if err := a.ApplyEngagementSettings(config.EngagementSettings{}); err == nil {
		t.Fatal("saved engagement settings with none open")
	}
	if _, err := a.Engagements().Create(dir, "Acme", "", []target.Target{
		target.New(target.KindDomain, "app.test", target.Palette[0])}); err != nil {
		t.Fatal(err)
	}
	if _, collect := a.scope("other.test", netip.Addr{}, "/"); collect {
		t.Fatal("off-target traffic collected with targets required")
	}

	off := false
	chain := upstream.Chain{Enabled: true, Proxies: []upstream.Proxy{
		{ID: "e1", Enabled: true, Kind: upstream.HTTP, Host: "jump.test", Port: 3128}}}
	if err := a.ApplyEngagementSettings(config.EngagementSettings{TargetsRequired: &off, Upstream: &chain}); err != nil {
		t.Fatal(err)
	}
	if _, collect := a.scope("other.test", netip.Addr{}, "/"); !collect {
		t.Error("the engagement's 'collect everything' wasn't applied")
	}
	if got := a.Effective().Upstream.Active(); len(got) != 1 || got[0].Host != "jump.test" {
		t.Errorf("effective chain = %+v", got)
	}
	if a.Settings().Targets.Required != true || len(a.Settings().Upstream.Active()) != 0 {
		t.Error("the engagement's settings leaked into the global ones")
	}
	if err := a.SuppressHost("cdn.test", true); err != nil {
		t.Fatal(err)
	}
	if e, _, _ := a.EngagementSettings(); len(e.Suppressed) != 1 || len(a.Settings().Targets.Suppressed) != 0 {
		t.Errorf("engagement-only suppression went to the wrong list: %+v / %+v", e.Suppressed, a.Settings().Targets.Suppressed)
	}
	saved, err := config.LoadEngagementSettings(filepath.Join(dir, "settings.json"))
	if err != nil || saved.TargetsRequired == nil || len(saved.Suppressed) != 1 {
		t.Errorf("saved = %+v, %v", saved, err)
	}

	if err := a.Engagements().Close(); err != nil {
		t.Fatal(err)
	}
	if _, collect := a.scope("other.test", netip.Addr{}, "/"); collect && len(a.Targets().List()) > 0 {
		t.Error("the engagement's override outlived it")
	}
	if len(a.Effective().Upstream.Active()) != 0 {
		t.Error("the engagement's chain outlived it")
	}

	// Reopening brings them back.
	if _, err := a.Engagements().Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, collect := a.scope("other.test", netip.Addr{}, "/"); !collect {
		t.Error("reopened engagement's override not applied")
	}
}

func TestExportSourceMap(t *testing.T) {
	a, configPath := newApp(t, freePort(t))

	mapJSON := `{"version":3,"file":"app.js","mappings":"AAAA",` +
		`"sources":["../../src/main.ts","../../src/api/client.ts","../../node_modules/vue/dist/vue.mjs"],` +
		`"sourcesContent":["import 'vue'","fetch('/api/me')","export default {}"]}`
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n" + mapJSON
	id := a.Store().Add(capture.Flow{
		Method:   "GET",
		Host:     "app.test",
		Path:     "/assets/app.js.map",
		Response: capture.Message{Raw: []byte(raw)},
	})

	res, err := a.ExportSourceMap(id, files.SourceUser, "")
	if err != nil {
		t.Fatalf("ExportSourceMap: %v", err)
	}
	if res.Files != 3 || res.CountKind(files.KindSourceMap) != 2 {
		t.Fatalf("want 3 files (2 app), got %d (%d app)", res.Files, res.CountKind(files.KindSourceMap))
	}
	// With no engagement open, the tree lands under the data directory.
	want := filepath.Join(filepath.Dir(configPath), "recovered")
	if res.Dir != want {
		t.Errorf("dir = %q, want %q", res.Dir, want)
	}
	if sub := res.CommonDir(); sub != "app.test/app.js" {
		t.Errorf("the recovery's own directory = %q", sub)
	}
	got, err := os.ReadFile(filepath.Join(res.Dir, "app.test", "app.js", "src", "api", "client.ts"))
	if err != nil {
		t.Fatalf("reading recovered file: %v", err)
	}
	if string(got) != "fetch('/api/me')" {
		t.Errorf("recovered content = %q", got)
	}
	// Every recovered file is now one of the engagement's, with where it
	// came from, so the file tools and the plugins' file hooks see them.
	rec, ok := a.Files().ByPath("app.test/app.js/src/api/client.ts")
	if !ok || rec.FlowID != id || rec.Kind != files.KindSourceMap || rec.Source != files.SourceUser ||
		rec.Host != "app.test" || rec.Batch != res.Batch || rec.MediaType != "text/typescript" {
		t.Errorf("recorded = %+v, ok %v", rec, ok)
	}
	if n := a.Files().Len(); n != 3 {
		t.Errorf("%d recovered files recorded, want 3", n)
	}
	// Recovering it again changes nothing: the scanners are not set off a
	// second time for files that have not changed.
	again, err := a.ExportSourceMap(id, files.SourceUser, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Files != 0 || again.Unchanged != 3 {
		t.Errorf("a second export: %+v", again)
	}

	// A flow that carries no map is refused cleanly.
	plain := a.Store().Add(capture.Flow{Method: "GET", Host: "app.test", Path: "/x",
		Response: capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n<html>")}})
	if _, err := a.ExportSourceMap(plain, files.SourceUser, ""); err == nil {
		t.Error("expected an error exporting a non-map response")
	}
}
