package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"reproxy/internal/upstream"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || !reflect.DeepEqual(c, Default()) {
		t.Fatalf("Load = %+v, %v", c, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	want := Default()
	want.ProxyPort = 9000
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.ProxyPort != 9000 {
		t.Fatalf("Load = %+v, %v", c, err)
	}
}

func TestLoadFillsMissingFieldsWithDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{}`), 0o644)
	c, err := Load(path)
	if err != nil || c.ProxyPort != DefaultProxyPort || c.ProxyHost != DefaultProxyHost ||
		!c.Intercept.UpdateContentLength {
		t.Fatalf("Load = %+v, %v", c, err)
	}
}

func TestInvalidHostRejected(t *testing.T) {
	c := Default()
	c.ProxyHost = "localhost:80"
	if c.Validate() == nil {
		t.Fatal("accepted host with port")
	}
}

func TestInvalidPortRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Config{ProxyHost: DefaultProxyHost, ProxyPort: 70000}); err == nil {
		t.Fatal("Save accepted port 70000")
	}
	os.WriteFile(path, []byte(`{"proxy_port": 0}`), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted port 0")
	}
}

func TestDefaultPathIsUnderDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)

	want := filepath.Join(home, ".reproxy", "config.json")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "reproxy", "config.json")
	}
	if got, err := DefaultPath(); err != nil || got != want {
		t.Fatalf("DefaultPath = %q, %v; want %q", got, err, want)
	}
}

func TestUIStateRoundTrip(t *testing.T) {
	path := UIStatePath(filepath.Join(t.TempDir(), "config.json"))
	if s, err := LoadUIState(path); err != nil || s.Panels != nil || s.WindowGeometry != nil {
		t.Fatalf("missing file = %+v, %v", s, err)
	}

	want := UIState{
		WindowGeometry:     []byte{0x01, 0xd9, 0xd0, 0xcb, 0x00},
		OuterSplitter:      []byte{0xff, 0x00},
		TrafficDetailWidth: 640,
		Panels:             map[string]bool{"bottom": false, "requests": true},
	}
	if err := SaveUIState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadUIState(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadUIState = %+v, %v; want %+v", got, err, want)
	}

	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := LoadUIState(path); err == nil {
		t.Fatal("corrupt file accepted")
	}
}

func TestMCPDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"mcp":{"port":9000}}`), 0o644)
	c, err := Load(path)
	if err != nil || c.MCP.Port != 9000 || c.MCP.Host != DefaultProxyHost || c.MCP.ExternalPolicy != PolicyAsk {
		t.Fatalf("Load = %+v, %v", c.MCP, err)
	}
	if c.MCP.Addr() != "127.0.0.1:9000" {
		t.Fatalf("Addr = %s", c.MCP.Addr())
	}
	bad := Default()
	bad.MCP.ExternalPolicy = "yolo"
	if bad.Validate() == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestAgentsRoundTrip(t *testing.T) {
	path := AgentsPath(filepath.Join(t.TempDir(), "config.json"))
	if a, err := LoadAgents(path); err != nil || len(a.Profiles) != 0 {
		t.Fatalf("missing = %+v, %v", a, err)
	}
	want := Agents{Profiles: []AgentProfile{{ID: "p1", Vendor: "anthropic", Name: "Claude", Model: "m"}}}
	if err := SaveAgents(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAgents(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if p, ok := got.Profile("p1"); !ok || p.Name != "Claude" {
		t.Fatalf("Profile = %+v", p)
	}
	dup := Agents{Profiles: []AgentProfile{want.Profiles[0], want.Profiles[0]}}
	if SaveAgents(path, dup) == nil {
		t.Fatal("duplicate IDs accepted")
	}
}

func TestUpstreamChainRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := Default()
	want.Upstream = upstream.Chain{
		Enabled:   true,
		VerifyTLS: true,
		Proxies: []upstream.Proxy{
			{ID: "a", Enabled: true, Kind: upstream.SOCKS5, Host: "jump.test", Port: 1080, Username: "u"},
			{ID: "b", Enabled: true, Kind: upstream.HTTP, Host: "stage-proxy.test", Port: 8080},
		},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Upstream, want.Upstream) {
		t.Fatalf("Upstream = %+v, want %+v", c.Upstream, want.Upstream)
	}
}

func TestUpstreamDefaultsVerifyTLSOnOldConfigs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// A settings file written before upstream proxies existed.
	os.WriteFile(path, []byte(`{"proxy_port":7193}`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Upstream.VerifyTLS || c.Upstream.Enabled || len(c.Upstream.Proxies) != 0 {
		t.Fatalf("Upstream = %+v, want the default chain", c.Upstream)
	}
}

func TestUpstreamProxyPointingAtOurselvesRejected(t *testing.T) {
	base := Default()
	base.Upstream.Enabled = true

	// The exact listen address, and a wildcard listener that would answer
	// on whatever address the hop names, both loop back into the proxy.
	for name, tc := range map[string]struct {
		listenHost, proxyHost string
		port                  int
	}{
		"same address":                 {DefaultProxyHost, DefaultProxyHost, DefaultProxyPort},
		"loopback spelled differently": {"127.0.0.1", "::1", DefaultProxyPort},
		"wildcard listener":            {"0.0.0.0", "10.0.0.5", DefaultProxyPort},
	} {
		c := base
		c.ProxyHost = tc.listenHost
		c.ProxyPort = tc.port
		c.Upstream.Proxies = []upstream.Proxy{
			{ID: "a", Enabled: true, Kind: upstream.SOCKS5, Host: tc.proxyHost, Port: tc.port},
		}
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted a proxy pointing at our own listener", name)
		}
	}

	// A different port on the same host is a real proxy, not a loop.
	ok := base
	ok.Upstream.Proxies = []upstream.Proxy{
		{ID: "a", Enabled: true, Kind: upstream.SOCKS5, Host: DefaultProxyHost, Port: DefaultProxyPort + 1},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("rejected a valid local proxy: %v", err)
	}

	// A disabled hop can't loop, so it isn't blocked.
	off := base
	off.Upstream.Proxies = []upstream.Proxy{
		{ID: "a", Kind: upstream.SOCKS5, Host: DefaultProxyHost, Port: DefaultProxyPort},
	}
	if err := off.Validate(); err != nil {
		t.Errorf("rejected a disabled hop: %v", err)
	}
}

func TestInvalidUpstreamRejected(t *testing.T) {
	c := Default()
	c.Upstream.Proxies = []upstream.Proxy{{ID: "a", Kind: "socks6", Host: "p.test", Port: 1080}}
	if err := c.Validate(); err == nil {
		t.Error("an unknown upstream proxy type was accepted")
	}
}
