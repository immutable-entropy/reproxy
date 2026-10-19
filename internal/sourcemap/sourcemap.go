// Package sourcemap reads a JavaScript Source Map v3 and writes the original
// source files it embeds back to disk — what a tester means by
// "un-webpacking" a build.
//
// A production Vite/webpack/esbuild build writes a .map file beside each
// bundle. When that map carries sourcesContent (most do), it holds the
// original, un-minified source of every module, index-aligned to the sources
// paths. Recovering it is not the VLQ decoding the "mappings" field needs —
// that only maps positions back to lines — because the text is already there
// verbatim. So this package parses the map and lays the files out under a
// chosen directory for reading.
//
// The source paths come from a map fetched off a target, so they are
// hostile: "../../../../etc/passwd", "/etc/cron.d/x", "webpack://../x",
// "C:\\Windows\\x", a leading NUL from a bundler's virtual module. Files
// treats every path as untrusted and cleans it with files.SafeRel; writing
// the tree, and the confinement that keeps every write inside the
// engagement's recovered files, is package files' job — this package only
// reads the map.
package sourcemap

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"reproxy/internal/files"
)

// Map is a parsed Source Map v3, holding only what this package uses.
type Map struct {
	// File is the bundle the map describes (the "file" field), if given.
	File string
	// Sources are the original module paths, as written.
	Sources []string
	// Contents is sourcesContent, index-aligned to Sources: the original
	// text of each module, or "" where the map carried none.
	Contents []string
}

// Parse reads a map's JSON. It returns an error if the bytes are not a v3
// source map (version 3, a sources array, and a mappings string — the last
// is what tells a map from, say, a web-app manifest).
func Parse(data []byte) (*Map, error) {
	var raw struct {
		Version        int               `json:"version"`
		File           string            `json:"file"`
		Sources        []string          `json:"sources"`
		SourcesContent []json.RawMessage `json:"sourcesContent"`
		Mappings       *string           `json:"mappings"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if raw.Version != 3 || len(raw.Sources) == 0 || raw.Mappings == nil {
		return nil, errors.New("not a Source Map v3 (needs version 3, sources and mappings)")
	}
	m := &Map{File: raw.File, Sources: raw.Sources, Contents: make([]string, len(raw.Sources))}
	for i := range raw.Sources {
		if i >= len(raw.SourcesContent) {
			break
		}
		// An entry is a JSON string or null; only a string is content.
		if s := raw.SourcesContent[i]; len(s) > 0 && s[0] == '"' {
			var str string
			if json.Unmarshal(s, &str) == nil {
				m.Contents[i] = str
			}
		}
	}
	return m, nil
}

// HasContent reports whether the map carries any embedded source to recover.
func (m *Map) HasContent() bool {
	for _, c := range m.Contents {
		if c != "" {
			return true
		}
	}
	return false
}

// File is one recovered source: a cleaned relative path, the original text,
// and whether it is the application's own code rather than a dependency.
type File struct {
	Path    string
	Content string
	App     bool
}

// notApp matches the paths that are not the application's own code:
// dependencies, a bundler's virtual modules, and the polyfills a build
// injects. It mirrors the vue-spa plugin's rule so the two agree on what
// counts as app source.
var notApp = regexp.MustCompile(`(?i)(^|/)node_modules/|^[a-z][a-z0-9+.-]*:|(^|/)virtual:|/\.vite/|(^|/)vite/|rollup|commonjshelpers|modulepreload-polyfill`)

func isApp(src string) bool {
	return !strings.HasPrefix(src, "\x00") && !notApp.MatchString(src)
}

// Files returns the modules the map carries text for, each with a cleaned,
// confined relative path, application files first. A module present with no
// content is left out; a path that cannot be made safe is left out. Paths
// are deduplicated.
func (m *Map) Files() []File {
	var app, deps []File
	seen := make(map[string]bool)
	for i, src := range m.Sources {
		if m.Contents[i] == "" {
			continue
		}
		rel, ok := files.SafeRel(src)
		if !ok || seen[rel] {
			continue
		}
		seen[rel] = true
		f := File{Path: rel, Content: m.Contents[i], App: isApp(src)}
		if f.App {
			app = append(app, f)
		} else {
			deps = append(deps, f)
		}
	}
	return append(app, deps...)
}
