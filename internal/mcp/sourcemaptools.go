package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"reproxy/internal/files"
)

// export_source_map recovers to disk the original source files a JavaScript
// source map embeds — "un-webpacking" a production build so the un-minified
// client source can be read. The work is in package sourcemap; where the
// files land (the open engagement's folder) is app's, injected through
// SetSourceMapExporter, so this tool only resolves the flow and reports the
// result. Each file it writes is one of the engagement's recovered files
// (package files), so list_files and get_file read them back and the
// plugins that hook file.on_created scan them.
//
// Like create_finding it is a Writes tool: it changes the local disk only,
// nothing leaves the machine, so it never asks for approval, but a read-only
// caller may not call it.

var exportSourceMap = &Tool{
	Name: "export_source_map",
	Description: "Recover the original source files embedded in a JavaScript source map and write them to disk, " +
		"recreating the application's source tree (\"un-webpacking\" the build). Pass a flow that is a .map " +
		"response, or whose body is itself a source map (the spa-recon plugin's findings point at these). The files are " +
		"written under the open engagement and recorded as recovered files (see list_files and get_file); " +
		"the tool returns the directory, the batch they share and how many files it wrote, and " +
		"never writes outside that directory. Use it to read an app's un-minified client source — its routes, " +
		"client-side checks and the URLs it calls.",
	Writes: true,
	Schema: object(map[string]any{
		"flow": strField("The flow to export, as a flow handle from list_flows. It must be a flow of this instance."),
	}, "flow"),
	run: func(_ context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Flow string `json:"flow"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Flow == "" {
			return nil, errors.New("flow is required")
		}
		if r.sourceMapExporter == nil {
			return nil, errors.New("this reproxy process cannot write recovered sources (no engagement store)")
		}
		_, flowID, err := r.localFlow(a.Flow)
		if err != nil {
			return nil, err
		}
		res, err := r.sourceMapExporter(flowID, files.SourceAgent, c.who())
		if err != nil {
			return nil, err
		}
		dir := res.Dir
		if sub := res.CommonDir(); sub != "" {
			dir = filepath.Join(dir, filepath.FromSlash(sub))
		}
		out := map[string]any{
			"directory": dir,
			"root":      res.Dir,
			"batch":     res.Batch,
			"files":     res.Files,
			"app_files": res.CountKind(files.KindSourceMap),
			"unchanged": res.Unchanged,
			"bytes":     res.Bytes,
		}
		if len(res.Skipped) > 0 {
			out["skipped"] = res.Skipped
		}
		return out, nil
	},
}
