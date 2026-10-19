package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"reproxy/internal/config"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/registry"
)

// fakeRepoFiles serves a canned GitHub tree and raw files, as a repository
// getter, without any network.
func fakeRepoFiles(files map[string]string) registry.Getter {
	return func(_ context.Context, url string) ([]byte, int, error) {
		if strings.Contains(url, "api.github.com") {
			type entry struct {
				Path string `json:"path"`
				Type string `json:"type"`
			}
			var tree []entry
			for p := range files {
				tree = append(tree, entry{Path: p, Type: "blob"})
			}
			sort.Slice(tree, func(i, j int) bool { return tree[i].Path < tree[j].Path })
			b, _ := json.Marshal(map[string]any{"sha": "deadbeef", "tree": tree})
			return b, 200, nil
		}
		if i := strings.Index(url, "/deadbeef/"); i >= 0 {
			if c, ok := files[url[i+len("/deadbeef/"):]]; ok {
				return []byte(c), 200, nil
			}
		}
		return nil, 404, nil
	}
}

func TestAvailableAndInstallPlugin(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	a.pluginGet = fakeRepoFiles(map[string]string{
		"plugins/greeter/plugin.json":    `{"name":"greeter","version":"1.0","author":{"name":"Ann"},"status":"enabled","description":"says hi","targets":[{"hook":"plugin.on_load","function":"init"}]}`,
		"plugins/greeter/main.ts":        `import { log } from "reproxy"; export function init() { log.info("hi"); }`,
		"plugins/greeter/src/x.ts":       `export const x = 1;`,
		"plugins/greeter/test/a.test.ts": `// not installed`,
	})
	repos := []string{"https://github.com/owner/repo"}

	avail, errs := a.AvailablePlugins(context.Background(), repos)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(avail) != 1 || avail[0].Name != "greeter" || avail[0].Installed {
		t.Fatalf("available = %+v", avail)
	}

	name, err := a.InstallPlugin(context.Background(), avail[0])
	if err != nil || name != "greeter" {
		t.Fatalf("install = %q, %v", name, err)
	}
	// Files landed in the plugins directory, tests excluded.
	dir := filepath.Join(plugin.UserDir(a.DataDir()), "greeter")
	for _, f := range []string{"plugin.json", "main.ts", "src/x.ts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "test", "a.test.ts")); err == nil {
		t.Error("a test file was installed")
	}
	// It is present but disabled, even though its manifest says enabled, so
	// downloaded code never runs until the user turns it on.
	info, ok := a.Plugins().Get("greeter")
	if !ok || info.State != plugin.StateDisabled {
		t.Fatalf("installed plugin state = %v (ok=%v)", info.State, ok)
	}
	if o := a.Settings().Plugins.Overrides["greeter"]; o.Status != "disabled" {
		t.Fatalf("override = %+v", o)
	}
	// It now shows as installed on a second listing.
	avail, _ = a.AvailablePlugins(context.Background(), repos)
	if len(avail) != 1 || !avail[0].Installed {
		t.Fatalf("after install, Installed = %v", avail)
	}
}

func TestInstallRejectsBrokenDownload(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	a.pluginGet = fakeRepoFiles(map[string]string{
		"bad/plugin.json": `{"name":"bad","version":"1.0","author":{"name":"A"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"missing"}]}`,
		"bad/main.ts":     `export function other() {}`,
	})
	avail, _ := a.AvailablePlugins(context.Background(), []string{"https://github.com/o/r"})
	if len(avail) != 1 {
		t.Fatalf("available = %v", avail)
	}
	if _, err := a.InstallPlugin(context.Background(), avail[0]); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("install of a broken plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plugin.UserDir(a.DataDir()), "bad")); err == nil {
		t.Error("a broken plugin was left in the plugins directory")
	}
}

func TestPluginUpdate(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	manifestAt := func(version string) string {
		return `{"name":"greeter","version":"` + version + `","author":{"name":"Ann"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"init"}]}`
	}
	files := map[string]string{
		"greeter/plugin.json": manifestAt("1.0"),
		"greeter/main.ts":     `export function init() {}`,
	}
	a.pluginGet = fakeRepoFiles(files)
	repos := []string{"https://github.com/owner/repo"}

	avail, _ := a.AvailablePlugins(context.Background(), repos)
	if _, err := a.InstallPlugin(context.Background(), avail[0]); err != nil {
		t.Fatal(err)
	}
	// The user enables it and changes a setting.
	cfg := a.Settings()
	cfg.Plugins.Overrides = map[string]config.PluginOverride{"greeter": {Status: "enabled"}}
	if err := a.ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}

	// The same version is no update.
	avail, _ = a.AvailablePlugins(context.Background(), repos)
	if !avail[0].Installed || avail[0].Update || avail[0].InstalledVersion != "1.0" {
		t.Fatalf("same version: %+v", avail[0])
	}

	// A newer version in the repository is, from a URL written differently.
	files["greeter/plugin.json"] = manifestAt("1.1")
	files["greeter/main.ts"] = `export function init() {} // 1.1`
	avail, _ = a.AvailablePlugins(context.Background(), []string{"https://github.com/Owner/repo.git"})
	if !avail[0].Update || avail[0].Version != "1.1" || avail[0].InstalledVersion != "1.0" {
		t.Fatalf("newer version: %+v", avail[0])
	}
	if _, err := a.UpdatePlugin(context.Background(), avail[0]); err != nil {
		t.Fatal(err)
	}
	info, ok := a.Plugins().Get("greeter")
	if !ok || info.Manifest == nil || info.Manifest.Version != "1.1" {
		t.Fatalf("after update: %+v", info)
	}
	// It kept the status the user gave it.
	if o := a.Settings().Plugins.Overrides["greeter"]; o.Status != "enabled" {
		t.Fatalf("override after update = %+v", o)
	}
	avail, _ = a.AvailablePlugins(context.Background(), repos)
	if avail[0].Update {
		t.Fatalf("still an update after updating: %+v", avail[0])
	}
	// Nothing was left behind in the plugins directory.
	entries, _ := os.ReadDir(plugin.UserDir(a.DataDir()))
	for _, e := range entries {
		if e.Name() != "greeter" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// A plugin put in the plugins folder by hand has no record of where it came
// from, but a repository offering a newer version of that name is still an
// update worth raising. It is marked so the user can be told, before they
// accept it, that their own files are what will be replaced.
func TestPluginUpdateOfAHandPlacedCopy(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	a.pluginGet = fakeRepoFiles(map[string]string{
		"greeter/plugin.json": `{"name":"greeter","version":"9.0","author":{"name":"Ann"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"init"}]}`,
		"greeter/main.ts":     `export function init() {} // from the repository`,
	})
	dir := filepath.Join(plugin.UserDir(a.DataDir()), "greeter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"greeter","version":"1.0","author":{"name":"Me"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"init"}]}`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.ts"), []byte(`export function init() {} // mine`), 0o644)
	a.Plugins().Rescan()

	avail, _ := a.AvailablePlugins(context.Background(), []string{"https://github.com/o/r"})
	if !avail[0].Installed || !avail[0].Update || !avail[0].InstalledByHand {
		t.Fatalf("a hand-placed plugin wasn't offered the newer version: %+v", avail[0])
	}
	if avail[0].InstalledVersion != "1.0" || avail[0].Version != "9.0" {
		t.Fatalf("versions: %+v", avail[0])
	}

	// Taking it replaces the user's own files, and records where the copy
	// now comes from, so the next update is an ordinary one.
	if _, err := a.UpdatePlugin(context.Background(), avail[0]); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.ts")); !strings.Contains(string(b), "from the repository") {
		t.Fatal("the update didn't replace the hand-placed copy")
	}
	avail, _ = a.AvailablePlugins(context.Background(), []string{"https://github.com/o/r"})
	if avail[0].Update || avail[0].InstalledByHand {
		t.Fatalf("after updating: %+v", avail[0])
	}
}

// A plugin installed from one repository is not updated from another that
// happens to offer the same name: that repository is its source, and a
// plugin of the same name elsewhere is a different plugin.
func TestPluginUpdateOnlyFromItsOrigin(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	files := map[string]string{
		"greeter/plugin.json": `{"name":"greeter","version":"1.0","author":{"name":"Ann"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"init"}]}`,
		"greeter/main.ts":     `export function init() {} // theirs`,
	}
	a.pluginGet = fakeRepoFiles(files)

	mine := []string{"https://github.com/owner/mine"}
	avail, _ := a.AvailablePlugins(context.Background(), mine)
	if _, err := a.InstallPlugin(context.Background(), avail[0]); err != nil {
		t.Fatal(err)
	}

	// Somewhere else offers a newer plugin of the same name.
	files["greeter/plugin.json"] = `{"name":"greeter","version":"9.0","author":{"name":"Someone"},"status":"disabled","targets":[{"hook":"plugin.on_load","function":"init"}]}`
	theirs := []string{"https://github.com/stranger/theirs"}
	avail, _ = a.AvailablePlugins(context.Background(), theirs)
	if !avail[0].Installed || avail[0].Update || avail[0].InstalledByHand {
		t.Fatalf("an update was offered from a repository it didn't come from: %+v", avail[0])
	}
	if _, err := a.UpdatePlugin(context.Background(), avail[0]); err == nil {
		t.Fatal("updated a plugin from a repository it wasn't installed from")
	}
	// The one it did come from still updates it.
	avail, _ = a.AvailablePlugins(context.Background(), mine)
	if !avail[0].Update {
		t.Fatalf("its own repository offered no update: %+v", avail[0])
	}
}

var _ = fmt.Sprint
