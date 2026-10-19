package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeRepo serves a canned GitHub tree and raw files through a Getter.
type fakeRepo struct {
	files map[string]string // repo path -> contents
	hits  []string
}

func (r *fakeRepo) get(_ context.Context, url string) ([]byte, int, error) {
	r.hits = append(r.hits, url)
	if strings.Contains(url, "api.github.com") && strings.Contains(url, "/git/trees/") {
		var tree []treeEntry
		for p := range r.files {
			tree = append(tree, treeEntry{Path: p, Type: "blob"})
		}
		sort.Slice(tree, func(i, j int) bool { return tree[i].Path < tree[j].Path })
		b, _ := json.Marshal(map[string]any{"sha": "abc123", "tree": tree})
		return b, 200, nil
	}
	// raw.githubusercontent.com/owner/repo/<ref>/<path>
	i := strings.Index(url, "/abc123/")
	if i < 0 {
		return nil, 404, nil
	}
	p := url[i+len("/abc123/"):]
	if c, ok := r.files[p]; ok {
		return []byte(c), 200, nil
	}
	return nil, 404, nil
}

func manifestJSON(name, version string) string {
	return fmt.Sprintf(`{"name":%q,"version":%q,"description":"d for %s","author":{"name":"Ann"}}`, name, version, name)
}

func TestParseGitHub(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/immutable-entropy/reproxy-plugins": "immutable-entropy/reproxy-plugins",
		"https://github.com/owner/repo.git/":                   "owner/repo",
		"http://github.com/a/b":                                "a/b",
	} {
		o, r, ok := ParseGitHub(url)
		if !ok || o+"/"+r != want {
			t.Errorf("ParseGitHub(%q) = %q/%q ok=%v, want %q", url, o, r, ok, want)
		}
	}
	for _, bad := range []string{"https://gitlab.com/a/b", "https://github.com/onlyowner", "not a url", "https://github.com/"} {
		if _, _, ok := ParseGitHub(bad); ok {
			t.Errorf("ParseGitHub(%q) accepted", bad)
		}
	}
}

func TestListAndDownload(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		"README.md":                   "# repo",
		"alpha/plugin.json":           manifestJSON("alpha", "1.0"),
		"alpha/main.ts":               "export const x = 1",
		"alpha/src/util.ts":           "export const y = 2",
		"alpha/test/alpha.test.ts":    "// a test, not installed",
		"alpha/node_modules/dep/i.ts": "// vendored, not installed",
		"beta/plugin.json":            manifestJSON("beta", "2.1-3"),
		"beta/main.ts":                "export const z = 3",
		"notaplugin/readme.txt":       "no manifest here",
	}}
	url := "https://github.com/immutable-entropy/reproxy-plugins"
	plugins, err := List(context.Background(), repo.get, url)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 || plugins[0].Name != "alpha" || plugins[1].Name != "beta" {
		t.Fatalf("plugins = %+v", plugins)
	}
	if plugins[0].Description != "d for alpha" || plugins[0].Version != "1.0" || plugins[0].Author != "Ann" {
		t.Errorf("metadata = %+v", plugins[0])
	}
	// Only installable files, no tests or node_modules.
	if !reflect.DeepEqual(plugins[0].Files, []string{"alpha/main.ts", "alpha/plugin.json", "alpha/src/util.ts"}) {
		t.Errorf("files = %v", plugins[0].Files)
	}

	// Download writes the files relative to the plugin directory.
	got := map[string]string{}
	if err := plugins[0].Download(context.Background(), repo.get, func(rel string, data []byte) error {
		got[rel] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"main.ts": "export const x = 1", "plugin.json": manifestJSON("alpha", "1.0"), "src/util.ts": "export const y = 2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("downloaded = %v", got)
	}
}

func TestListFindsRootSrcAndPluginsDirs(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		"plugins/alpha/plugin.json": manifestJSON("alpha", "1.0"),
		"plugins/alpha/main.ts":     "a",
		"src/beta/plugin.json":      manifestJSON("beta", "1.0"),
		"src/beta/main.ts":          "b",
		"gamma/plugin.json":         manifestJSON("gamma", "1.0"),
		"gamma/main.ts":             "c",
		"docs/guide.md":             "not a plugin",
	}}
	plugins, err := List(context.Background(), repo.get, "https://github.com/o/r")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, p := range plugins {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("names = %v", names)
	}
	for _, p := range plugins {
		if p.Name == "beta" && !reflect.DeepEqual(p.Files, []string{"src/beta/main.ts", "src/beta/plugin.json"}) {
			t.Errorf("beta files = %v", p.Files)
		}
	}
}

func TestListRootIsASinglePlugin(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		"plugin.json": manifestJSON("solo", "1.0"),
		"main.ts":     "x",
		"src/a.ts":    "y",
	}}
	plugins, err := List(context.Background(), repo.get, "https://github.com/o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].Name != "solo" || plugins[0].Dir != "" {
		t.Fatalf("plugins = %+v", plugins)
	}
	if !reflect.DeepEqual(plugins[0].Files, []string{"main.ts", "plugin.json", "src/a.ts"}) {
		t.Errorf("files = %v", plugins[0].Files)
	}
}

func TestListRejectsNonGitHub(t *testing.T) {
	if _, err := List(context.Background(), (&fakeRepo{}).get, "https://example.com/x"); err == nil {
		t.Fatal("non-GitHub URL accepted")
	}
}

func TestDownloadRefusesEscapingPaths(t *testing.T) {
	p := Plugin{Name: "x", Dir: "x", Files: []string{"x/../../etc/passwd"}, owner: "o", repo: "r", ref: "abc123"}
	err := p.Download(context.Background(), func(context.Context, string) ([]byte, int, error) { return []byte("x"), 200, nil },
		func(string, []byte) error { return nil })
	if err == nil {
		t.Fatal("escaping path accepted")
	}
}

func TestOriginSame(t *testing.T) {
	a := Origin{Repo: "https://github.com/Owner/Repo", Dir: "plugins/x"}
	for _, c := range []struct {
		b    Origin
		want bool
	}{
		{Origin{Repo: "https://github.com/owner/repo.git/", Dir: "plugins/x"}, true},
		{Origin{Repo: "https://github.com/owner/repo", Dir: "x"}, false},
		{Origin{Repo: "https://github.com/owner/other", Dir: "plugins/x"}, false},
		{Origin{Repo: "not a url", Dir: "plugins/x"}, false},
	} {
		if got := a.Same(c.b); got != c.want {
			t.Errorf("Same(%+v) = %v, want %v", c.b, got, c.want)
		}
	}
}

func TestKeepInstallableSkipsOriginFile(t *testing.T) {
	got := keepInstallable([]string{"x/plugin.json", "x/" + OriginFile, "x/main.ts"})
	if len(got) != 2 {
		t.Fatalf("keepInstallable = %v", got)
	}
}
