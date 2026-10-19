package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reproxy/internal/elide"
	"reproxy/internal/files"
)

// The file tools expose what the work recovered (package files): the
// original source a map carried, a bundle taken apart, anything else a
// plugin, an agent or the user pulled out of the traffic and core wrote to
// disk. Like the findings and entity tools they act on the local process's
// store, because recovered files belong to one engagement.
//
// Reading recovered client source is most of what an agent wants them for —
// an app's routes, its client-side checks, the URLs it calls — so get_file
// hands back the text, bounded, rather than only saying where the file is.

// defaultFileBytes is how much of a file get_file returns when the caller
// doesn't say, and maxFileBytes the most it will ever return: enough for a
// module of real source, not enough to fill a context window with a bundle.
const (
	defaultFileBytes = 64 << 10  // 64 KiB
	maxFileBytes     = 512 << 10 // 512 KiB
)

// fileOut is a recovered file as the model sees it.
type fileOut struct {
	ID         uint64    `json:"id"`
	Path       string    `json:"path"`
	Bytes      int64     `json:"bytes"`
	MediaType  string    `json:"media_type"`
	Text       bool      `json:"text"`
	Kind       string    `json:"kind,omitempty"`
	Source     string    `json:"source"`
	Author     string    `json:"author,omitempty"`
	Batch      string    `json:"batch,omitempty"`
	BatchLabel string    `json:"batch_label,omitempty"`
	Notes      string    `json:"notes,omitempty"`
	Host       string    `json:"host,omitempty"`
	URL        string    `json:"url,omitempty"`
	Flow       string    `json:"flow,omitempty"`
	Entity     string    `json:"entity,omitempty"`
	SHA256     string    `json:"sha256"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

func toFileOut(instanceID string, f files.File) fileOut {
	out := fileOut{
		ID: f.ID, Path: f.Path, Bytes: f.Size, MediaType: f.MediaType, Text: f.Text(),
		Kind: f.Kind, Source: string(f.Source), Author: f.Author,
		Batch: f.Batch, BatchLabel: f.BatchLabel, Notes: f.Notes,
		Host: f.Host, URL: f.URL, Entity: f.Entity, SHA256: f.SHA256,
		Created: f.Created, Updated: f.Updated,
	}
	if f.FlowID != 0 {
		out.Flow = flowHandle(instanceID, f.FlowID)
	}
	return out
}

// FileStore is what the file tools need from this process's recovered files
// (see internal/files). Like findings and entities they belong to the open
// engagement, so the tools act on the local store rather than routing
// through an Instance.
type FileStore interface {
	// Select returns the recovered files matching a query, newest first.
	Select(files.Query) []files.File
	// Get returns one file's record by ID.
	Get(id uint64) (files.File, bool)
	// ByPath returns one file's record by its path under the root.
	ByPath(path string) (files.File, bool)
	// Read returns a file's bytes, at most max of them.
	Read(id uint64, max int64) ([]byte, error)
	// Batches summarises the recoveries the store holds, newest first.
	Batches() []files.Batch
	// Root is the directory the files are written under.
	Root() string
}

// SetFiles wires the recovered-file store the file tools act on. Call it
// once, after New; without it the tools report that this process has none.
func (r *Registry) SetFiles(store FileStore) { r.files = store }

// fileStore returns the local recovered files, or an error when there are
// none.
func (r *Registry) fileStore() (FileStore, error) {
	if r.files == nil {
		return nil, ErrNoFiles
	}
	return r.files, nil
}

var listFiles = &Tool{
	Name: "list_files",
	Description: "List the files the work has recovered: the original source a JavaScript source map " +
		"carried (export_source_map, or the spa-recon plugin), and anything else a plugin, an agent or " +
		"the user pulled out of the traffic. Each file is on disk under the open engagement and is " +
		"recorded with where it came from — the flow, the host, the Explorer entity. Use it to find an " +
		"application's un-minified client source, then get_file to read one. Files recovered in one go " +
		"share a batch, so batch narrows a list to one recovery.",
	Schema: object(map[string]any{
		"batch":     strField("Keep only the files one recovery produced (a batch id from a file or from this tool)."),
		"kind":      strField("Keep only files with this kind label: source-map (the application's own source), source-map-dependency, or whatever a plugin recorded."),
		"source":    strField("Keep only files recovered by: user, agent or plugin."),
		"author":    strField("Keep only files recovered by this plugin or agent session."),
		"host":      strField("Keep only files recovered from this host."),
		"flow":      strField("Keep only files recovered from this flow, as a flow handle from list_flows."),
		"entity":    strField("Keep only files belonging to this Explorer entity key."),
		"dir":       strField("Keep only files under this directory, relative to the recovered-files root."),
		"text":      strField("Keep only files whose path, notes, kind or source URL contain this."),
		"text_only": boolField("true keeps only files that can be read as text (source, JSON, CSS…), leaving out fonts and images."),
		"limit":     intField("The most files to return; the default is 100."),
	}),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Batch    string `json:"batch"`
			Kind     string `json:"kind"`
			Source   string `json:"source"`
			Author   string `json:"author"`
			Host     string `json:"host"`
			Flow     string `json:"flow"`
			Entity   string `json:"entity"`
			Dir      string `json:"dir"`
			Text     string `json:"text"`
			TextOnly bool   `json:"text_only"`
			Limit    int    `json:"limit"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		store, err := r.fileStore()
		if err != nil {
			return nil, err
		}
		q := files.Query{
			Batch: a.Batch, Kind: a.Kind, Source: files.Source(a.Source), Author: a.Author,
			Host: a.Host, Entity: a.Entity, Dir: a.Dir, Text: a.Text, TextOnly: a.TextOnly,
		}
		if a.Flow != "" {
			_, flowID, err := r.localFlow(a.Flow)
			if err != nil {
				return nil, err
			}
			q.FlowID = flowID
		}
		matched := store.Select(q)
		limit := a.Limit
		if limit <= 0 {
			limit = 100
		}
		local := r.router.LocalID()
		out := make([]fileOut, 0, min(limit, len(matched)))
		for _, f := range matched {
			if len(out) >= limit {
				break
			}
			out = append(out, toFileOut(local, f))
		}
		return map[string]any{
			"files":     out,
			"matched":   len(matched),
			"more":      len(matched) > len(out),
			"directory": store.Root(),
			"batches":   store.Batches(),
		}, nil
	},
}

var getFile = &Tool{
	Name: "get_file",
	Description: "Read one recovered file: its record and its text. Identify it by the id or the path " +
		"list_files gave. Text is returned up to max_bytes (64 KiB by default, 512 KiB at most) and the " +
		"result says whether it was cut short; a file that is not text (a font, an image) is described " +
		"but not returned. Use it to read an application's own source: its routes, its client-side " +
		"checks, the endpoints it calls and anything left in it that shouldn't ship.",
	Schema: object(map[string]any{
		"id":        intField("The file's id, from list_files."),
		"path":      strField("The file's path under the recovered-files root, instead of its id."),
		"max_bytes": intField("The most text to return; the default is 65536 and the most is 524288."),
	}),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			ID       uint64 `json:"id"`
			Path     string `json:"path"`
			MaxBytes int64  `json:"max_bytes"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.ID == 0 && a.Path == "" {
			return nil, errors.New("pass the file's id or its path")
		}
		store, err := r.fileStore()
		if err != nil {
			return nil, err
		}
		f, ok := files.File{}, false
		if a.ID != 0 {
			f, ok = store.Get(a.ID)
		} else {
			f, ok = store.ByPath(a.Path)
		}
		if !ok {
			return nil, fmt.Errorf("no recovered file %s", elide.Runes(fileRef(a.ID, a.Path), 120))
		}

		out := map[string]any{"file": toFileOut(r.router.LocalID(), f)}
		if !f.Text() {
			out["text"] = nil
			out["note"] = "this file is not text (" + f.MediaType + "), so its bytes are not returned"
			return out, nil
		}
		limit := a.MaxBytes
		if limit <= 0 {
			limit = defaultFileBytes
		}
		if limit > maxFileBytes {
			limit = maxFileBytes
		}
		// One byte over the limit, so a file exactly as long as the limit
		// isn't reported as cut short.
		data, err := store.Read(f.ID, limit+1)
		if err != nil {
			return nil, err
		}
		truncated := int64(len(data)) > limit
		// elide.Prefix counts bytes but never splits a rune: recovered
		// source is arbitrary bytes, and a half rune is invalid UTF-8 the
		// model would be handed as a replacement character.
		out["text"] = elide.Prefix(string(data), int(limit))
		out["truncated"] = truncated
		return out, nil
	},
}

// fileRef names the file a caller asked for, for an error message.
func fileRef(id uint64, path string) string {
	if id != 0 {
		return fmt.Sprintf("%d", id)
	}
	return path
}
