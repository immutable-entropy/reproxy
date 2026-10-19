package sourcemap

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse(t *testing.T) {
	data := mustJSON(t, map[string]any{
		"version":        3,
		"file":           "index-abc.js",
		"mappings":       "AAAA,SAAS",
		"sources":        []string{"../src/main.ts", "../src/api.ts", "vendor.js"},
		"sourcesContent": []any{"const a = 1", nil, "export const x = 2"},
	})
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.File != "index-abc.js" || len(m.Sources) != 3 {
		t.Fatalf("unexpected map: %+v", m)
	}
	if m.Contents[0] != "const a = 1" || m.Contents[1] != "" || m.Contents[2] != "export const x = 2" {
		t.Fatalf("contents misaligned: %q", m.Contents)
	}
	if !m.HasContent() {
		t.Fatal("HasContent = false")
	}
}

func TestParseRejectsNonMaps(t *testing.T) {
	cases := map[string][]byte{
		"not json":        []byte("<html>"),
		"wrong version":   mustJSON(t, map[string]any{"version": 2, "sources": []string{"a"}, "mappings": ""}),
		"no mappings":     mustJSON(t, map[string]any{"version": 3, "sources": []string{"a"}}),
		"no sources":      mustJSON(t, map[string]any{"version": 3, "sources": []string{}, "mappings": ""}),
		"webapp manifest": mustJSON(t, map[string]any{"name": "App", "icons": []any{}, "start_url": "/"}),
	}
	for name, data := range cases {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestFilesAppFirstAndDeduped(t *testing.T) {
	m := &Map{
		Sources:  []string{"../src/main.ts", "../node_modules/vue/dist/vue.mjs", "../src/main.ts", "nope.ts"},
		Contents: []string{"app", "dep", "app", ""}, // last has no content
	}
	files := m.Files()
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(files), files)
	}
	if !files[0].App || files[0].Path != "src/main.ts" {
		t.Errorf("first should be app src/main.ts, got %+v", files[0])
	}
	if files[1].App || !strings.Contains(files[1].Path, "node_modules/vue") {
		t.Errorf("second should be the dependency, got %+v", files[1])
	}
}

// The important one: a map's source paths are attacker-controlled, so
// Extract must never write outside its directory, whatever the path.
