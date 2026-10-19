package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"reproxy/internal/cli"
	"reproxy/internal/elide"
	"reproxy/internal/files"
)

// The files namespace: what the work recovered. Like the findings commands
// these are adapters over files.Store — the same store the Files section,
// the file tools and a plugin's files.add write to — so a file recovered by
// a plugin is listed, read and deleted here exactly as one the user
// recovered from the traffic list.

func (a *App) fileCommands() []cli.Command {
	id := cli.Flag{Name: "id", Type: cli.FlagNumber, Placeholder: "id",
		Description: "the file's id, as \"files ls\" lists it"}
	path := cli.Flag{Name: "path", Type: cli.FlagString, Placeholder: "path",
		Description: "the file's path under the recovered files, instead of its id"}
	return []cli.Command{
		{
			Namespace: "files", Action: "ls",
			Summary: "the files the work has recovered",
			Description: "Each file is on disk under the engagement's recovered/ folder and recorded with where " +
				"it came from. Files recovered in one go share a batch (\"files batches\").",
			Flags: []cli.Flag{
				{Name: "batch", Type: cli.FlagString, Placeholder: "id",
					Description: "only the files one recovery produced"},
				{Name: "kind", Type: cli.FlagString, Placeholder: "label",
					Description: "only the files with this kind (source-map, source-map-dependency, a plugin's own)"},
				{Name: "source", Type: cli.FlagString, Placeholder: "who",
					Description: "only the files recovered by user, agent or plugin"},
				{Name: "author", Type: cli.FlagString, Placeholder: "name",
					Description: "only the files this plugin or agent session recovered"},
				{Name: "host", Type: cli.FlagString, Placeholder: "host",
					Description: "only the files recovered from this host"},
				{Name: "flow", Type: cli.FlagNumber, Placeholder: "id",
					Description: "only the files recovered from this request"},
				{Name: "dir", Type: cli.FlagString, Placeholder: "path",
					Description: "only the files under this directory"},
				{Name: "filter", Type: cli.FlagString, Placeholder: "text",
					Description: "keep the files whose path, notes, kind or source URL match"},
				{Name: "text", Type: cli.FlagBool, Description: "only the files that can be read as text"},
				limitFlag("50"),
			},
			Run: a.cmdFilesList,
		},
		{
			Namespace: "files", Action: "get",
			Summary: "one recovered file's record: where it came from and what it is",
			Flags:   []cli.Flag{id, path},
			Run:     a.cmdFilesGet,
		},
		{
			Namespace: "files", Action: "cat",
			Summary:     "print a recovered file",
			Description: "Text only; a font or an image is described rather than printed.",
			Flags: []cli.Flag{id, path, {Name: "bytes", Type: cli.FlagNumber, Placeholder: "n",
				Description: "the most to print (default: all of it)"}},
			Run: a.cmdFilesCat,
		},
		{
			Namespace: "files", Action: "batches",
			Summary:     "the recoveries the engagement holds",
			Description: "One row per recovery, newest first: unpacking a source map is one of them.",
			Flags:       []cli.Flag{limitFlag("50")},
			Run:         a.cmdFilesBatches,
		},
		{
			Namespace: "files", Action: "recover",
			Summary: "recover the source a response's source map carries",
			Description: "The same work as the traffic list's \"Export recovered sources…\": the map's embedded " +
				"source is written under the engagement and recorded, which is what calls the plugins that " +
				"scan recovered files. Pass a .map response, or a response whose body is itself a map.",
			Flags: []cli.Flag{flowFlag("whose source map to recover")},
			Run:   a.cmdFilesRecover,
		},
		{
			Namespace: "files", Action: "notes",
			Summary: "replace a recovered file's notes",
			Flags: []cli.Flag{id, path, {Name: "text", Type: cli.FlagString, Required: true, Placeholder: "text",
				Description: "the notes, or \"-\" for the piped input"}},
			Run: a.cmdFilesNotes,
		},
		{
			Namespace: "files", Action: "rm",
			Summary:     "delete a recovered file",
			Description: "The record and the bytes on disk both go.",
			Flags:       []cli.Flag{id, path, yesFlag("the file is deleted from disk")},
			Run:         a.cmdFilesRemove,
		},
		{
			Namespace: "files", Action: "clear",
			Summary: "delete every recovered file",
			Description: "The records and the bytes of every file reProxy recorded. Anything else in the " +
				"recovered/ folder is left alone.",
			Flags: []cli.Flag{yesFlag("every recovered file is deleted from disk")},
			Run:   a.cmdFilesClear,
		},
	}
}

func (a *App) cmdFilesList(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("recovered files"); err != nil {
		return cli.Result{}, err
	}
	q := files.Query{
		Batch: in.String("batch"), Kind: in.String("kind"), Author: in.String("author"),
		Host: in.String("host"), Dir: in.String("dir"), Text: in.String("filter"),
		TextOnly: in.Bool("text"), Limit: in.Int("limit"),
	}
	if s := in.String("source"); s != "" {
		q.Source = files.Source(s)
	}
	if id := in.Int("flow"); id > 0 {
		q.FlowID = uint64(id)
	}
	list := a.files.Select(q)
	rows := make([][]string, 0, len(list))
	for _, f := range list {
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10), bytesWord(f.Size), f.MediaType, f.Kind,
			string(f.Source), elide.Runes(f.Path, 70),
		})
	}
	return listing([]string{"ID", "SIZE", "TYPE", "KIND", "BY", "PATH"}, rows,
		"Nothing has been recovered yet.", list)
}

// bytesWord is a size as a column: bytes under a kibibyte, else KiB or MiB.
func bytesWord(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + "B"
	case n < 1024*1024:
		return strconv.FormatInt((n+512)/1024, 10) + "K"
	default:
		return strconv.FormatInt((n+512*1024)/(1024*1024), 10) + "M"
	}
}

// file reads the recovered file a command names, by id or by path.
func (a *App) file(in *cli.Input) (files.File, error) {
	if err := a.engagementOpen("recovered files"); err != nil {
		return files.File{}, err
	}
	if id := in.Int("id"); id > 0 {
		f, ok := a.files.Get(uint64(id))
		if !ok {
			return files.File{}, fmt.Errorf("no recovered file %d; \"reproxy files ls\" lists them", id)
		}
		return f, nil
	}
	path := strings.TrimSpace(in.String("path"))
	if path == "" {
		return files.File{}, fmt.Errorf("give the file's --id or its --path")
	}
	f, ok := a.files.ByPath(path)
	if !ok {
		return files.File{}, fmt.Errorf("no recovered file %q; \"reproxy files ls\" lists them", path)
	}
	return f, nil
}

func (a *App) cmdFilesGet(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.file(in)
	if err != nil {
		return cli.Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", f.ID, f.Path)
	about := []string{f.MediaType, bytesWord(f.Size), "recovered by " + string(f.Source)}
	if f.Author != "" {
		about[len(about)-1] += " " + f.Author
	}
	if f.Kind != "" {
		about = append(about, f.Kind)
	}
	fmt.Fprintf(&b, "%s\n", strings.Join(about, " · "))
	fmt.Fprintf(&b, "on disk  %s\n", filepath.Join(a.files.Root(), filepath.FromSlash(f.Path)))
	if f.URL != "" {
		fmt.Fprintf(&b, "from     %s %s", f.Method, f.URL)
		if f.FlowID != 0 {
			fmt.Fprintf(&b, " (flow %d)", f.FlowID)
		}
		b.WriteByte('\n')
	}
	if f.Entity != "" {
		fmt.Fprintf(&b, "entity   %s\n", f.Entity)
	}
	if f.Batch != "" {
		fmt.Fprintf(&b, "recovery %s", f.Batch)
		if f.BatchLabel != "" {
			fmt.Fprintf(&b, " (%s)", f.BatchLabel)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "sha256   %s\n", f.SHA256)
	if f.Notes != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(f.Notes, "\n"))
	}
	return cli.Data(b.String(), f)
}

func (a *App) cmdFilesCat(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.file(in)
	if err != nil {
		return cli.Result{}, err
	}
	if !f.Text() {
		return cli.Text("%s is %s, not text: it is on disk at %s\n",
			f.Path, f.MediaType, filepath.Join(a.files.Root(), filepath.FromSlash(f.Path))), nil
	}
	limit := int64(in.Int("bytes"))
	data, err := a.files.Read(f.ID, limit)
	if err != nil {
		return cli.Result{}, err
	}
	text := string(data)
	if limit > 0 {
		// Cut at a character boundary: a recovered file is arbitrary bytes.
		text = elide.Prefix(text, int(limit))
	}
	return cli.Data(text, map[string]any{"file": f, "text": text})
}

func (a *App) cmdFilesBatches(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("recovered files"); err != nil {
		return cli.Result{}, err
	}
	list := a.files.Batches()
	if limit := in.Int("limit"); limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	rows := make([][]string, 0, len(list))
	for _, b := range list {
		label := b.Label
		if label == "" {
			label = "—"
		}
		rows = append(rows, []string{
			b.ID, strconv.Itoa(b.Files), bytesWord(b.Bytes), string(b.Source),
			since(b.Created), elide.Runes(label, 60),
		})
	}
	return listing([]string{"BATCH", "FILES", "SIZE", "BY", "AGE", "FROM"}, rows,
		"Nothing has been recovered yet.", list)
}

func (a *App) cmdFilesRecover(_ context.Context, in *cli.Input) (cli.Result, error) {
	id := uint64(in.Int("flow"))
	res, err := a.ExportSourceMap(id, files.SourceUser, "")
	if err != nil {
		return cli.Result{}, err
	}
	dir := res.Dir
	if sub := res.CommonDir(); sub != "" {
		dir = filepath.Join(dir, filepath.FromSlash(sub))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Recovered %d files (%d the app's own) to %s\n",
		res.Files, res.CountKind(files.KindSourceMap), dir)
	if res.Unchanged > 0 {
		fmt.Fprintf(&b, "%d were already recovered, unchanged\n", res.Unchanged)
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintf(&b, "%d skipped as unsafe or over the limit\n", len(res.Skipped))
	}
	if res.Batch != "" {
		fmt.Fprintf(&b, "recovery %s\n", res.Batch)
	}
	return cli.Data(b.String(), res)
}

func (a *App) cmdFilesNotes(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.file(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.files.SetNotes(f.ID, in.Text("text")); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Notes set on %s\n", f.Path), nil
}

func (a *App) cmdFilesRemove(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.file(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := confirmed(in, fmt.Sprintf("%s would be deleted from disk", f.Path)); err != nil {
		return cli.Result{}, err
	}
	if err := a.files.Remove(f.ID); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Deleted %s\n", f.Path), nil
}

func (a *App) cmdFilesClear(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("recovered files"); err != nil {
		return cli.Result{}, err
	}
	n := a.files.Len()
	if err := confirmed(in, fmt.Sprintf("%d recovered file(s) would be deleted from disk", n)); err != nil {
		return cli.Result{}, err
	}
	if err := a.files.Clear(); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Deleted %d recovered file(s)\n", n), nil
}
