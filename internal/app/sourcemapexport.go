package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"reproxy/internal/capture"
	"reproxy/internal/files"
	"reproxy/internal/sourcemap"
)

// ExportSourceMap recovers the original source files embedded in the source
// map a flow carries — the flow being a .map response, or a response whose
// body is itself a source map — and records them as the engagement's
// recovered files. It is the core behind the traffic list's "Export
// recovered sources…", the MCP export_source_map tool and `reproxy files
// recover`: the spa-recon plugin finds and mines maps, but a plugin cannot
// write files, so laying the tree out is core's job.
//
// by and author say who asked (the user from the menu, an agent through the
// tool), which is what each recovered file records as its source. The whole
// map is one batch, so a plugin hooking file.on_created can take the
// recovery as a whole (files.on_batch_complete) rather than file by file.
//
// The source paths in a map come from a target and are untrusted; the
// confinement that keeps each write inside the recovered-files directory is
// in package files.
func (a *App) ExportSourceMap(flowID uint64, by files.Source, author string) (files.Result, error) {
	f, ok := a.store.Get(flowID)
	if !ok {
		return files.Result{}, fmt.Errorf("no flow %d in this engagement", flowID)
	}
	body, ok := capture.DecodedBody(f.Response)
	if !ok {
		return files.Result{}, errors.New("the flow's response has no readable body to read a source map from")
	}
	m, err := sourcemap.Parse(body)
	if err != nil {
		return files.Result{}, fmt.Errorf("this response is not a source map: %w", err)
	}
	if !m.HasContent() {
		return files.Result{}, errors.New("the source map carries no embedded source (sourcesContent), so there is nothing to recover")
	}

	url := f.URL()
	prefix := sourceMapPrefix(f)
	entity := ""
	if e, ok := a.entities.Lookup(scheme(f), f.Host, f.Path); ok {
		entity = e.Key
	}
	recovered := m.Files()
	drafts := make([]files.Draft, 0, len(recovered))
	for _, src := range recovered {
		kind := files.KindSourceMapDependency
		note := "a dependency's source, recovered from " + url
		if src.App {
			kind = files.KindSourceMap
			note = "the application's own source, recovered from " + url
		}
		drafts = append(drafts, files.Draft{
			Path:       prefix + "/" + src.Path,
			Content:    []byte(src.Content),
			Kind:       kind,
			Notes:      note,
			Source:     by,
			Author:     author,
			BatchLabel: url,
			FlowID:     f.ID,
			Method:     f.Method,
			URL:        url,
			Host:       f.Host,
			Entity:     entity,
		})
	}
	return a.files.RecordAll(drafts, files.Limits{})
}

// sourceMapPrefix is where a flow's recovered sources are laid out under
// the engagement's recovered files: <host>/<bundle>/, unchanged from before
// the files were recorded.
func sourceMapPrefix(f capture.Flow) string {
	return filepath.ToSlash(filepath.Join(pathSafe(f.Host), bundleName(f.URL())))
}

func scheme(f capture.Flow) string {
	if f.Secure {
		return "https"
	}
	return "http"
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// pathSafe reduces a host or file name to a single safe path segment.
func pathSafe(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		return "unknown"
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// bundleName is the file a map describes, as a safe directory name: the last
// path segment with its query and a trailing ".map" removed.
func bundleName(rawurl string) string {
	u := rawurl
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimSuffix(u, ".map")
	return pathSafe(u)
}
