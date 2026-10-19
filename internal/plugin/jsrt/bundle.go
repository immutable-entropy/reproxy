// Package jsrt runs plugin TypeScript in-process: esbuild (its Go API, so
// there is no Node) bundles a plugin's entry and everything it imports into
// one script, and goja — a JavaScript engine in pure Go, so core still
// cross-compiles — runs it, one runtime per plugin on a goroutine of its
// own.
//
// The bundler is the sandbox's front door. A plugin can import only its own
// files (relative paths that stay inside its directory) and the virtual
// modules it is handed — the SDK. Node's built-ins, packages and anything
// outside the directory fail to bundle. The runtime then gives the script no
// filesystem, process or network: whatever it can do, it does through the
// host functions the SDK wraps.
//
// It depends on nothing internal.
package jsrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// GlobalName is the global a bundle's exports are assigned to.
const GlobalName = "__plugin"

// Target is the language level bundles are lowered to: what goja runs
// natively (async functions included), with newer syntax such as for await
// rewritten by esbuild.
const Target = api.ES2017

// Bundle is a plugin script ready to run.
type Bundle struct {
	// JS evaluates to the plugin's exports in the global GlobalName. It
	// carries an inline source map, so stack traces name the plugin's own
	// .ts files and lines.
	JS string
	// Files are the plugin's files the bundle was built from, relative to
	// its root, sorted.
	Files []string
}

// sdkNamespace and pluginNamespace keep the two kinds of module apart in
// esbuild: virtual SDK modules, and files of the plugin.
const (
	sdkNamespace    = "reproxy-sdk"
	pluginNamespace = "plugin"
)

// Build bundles entry, a path in fsys, with everything it imports. modules
// are the virtual modules it may import by name, as TypeScript source (the
// SDK: "reproxy", "reproxy/testing").
func Build(fsys fs.FS, entry string, modules map[string]string) (*Bundle, error) {
	res, files, err := build(fsys, entry, modules, api.FormatIIFE, false)
	if err != nil {
		return nil, err
	}
	return &Bundle{JS: string(res.OutputFiles[0].Contents), Files: files}, nil
}

// Exports lists what entry exports, without running any of it — how a
// plugin's targets are checked against its functions while it is disabled.
func Exports(fsys fs.FS, entry string, modules map[string]string) ([]string, error) {
	res, _, err := build(fsys, entry, modules, api.FormatESModule, true)
	if err != nil {
		return nil, err
	}
	var meta struct {
		Outputs map[string]struct {
			Exports []string `json:"exports"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(res.Metafile), &meta); err != nil {
		return nil, fmt.Errorf("reading the bundle's exports: %w", err)
	}
	for name, out := range meta.Outputs {
		if strings.HasSuffix(name, ".js") {
			return out.Exports, nil
		}
	}
	return nil, nil
}

func build(fsys fs.FS, entry string, modules map[string]string, format api.Format, metafile bool) (api.BuildResult, []string, error) {
	files := &fileList{}
	opts := api.BuildOptions{
		EntryPoints: []string{entry},
		Bundle:      true,
		Write:       false,
		Outfile:     "plugin.js",
		Format:      format,
		Target:      Target,
		Platform:    api.PlatformNeutral,
		Charset:     api.CharsetUTF8,
		LogLevel:    api.LogLevelSilent,
		Metafile:    metafile,
		Plugins:     []api.Plugin{sandbox(fsys, modules, files)},
	}
	if format == api.FormatIIFE {
		opts.GlobalName = GlobalName
		opts.Sourcemap = api.SourceMapInline
	}
	res := api.Build(opts)
	if len(res.Errors) > 0 {
		return res, nil, buildError(res.Errors)
	}
	if len(res.OutputFiles) == 0 {
		return res, nil, errors.New("bundling produced nothing")
	}
	return res, files.sorted(), nil
}

// fileList collects the files loaded, which esbuild does concurrently.
type fileList struct {
	mu    sync.Mutex
	names []string
}

func (l *fileList) add(name string) {
	l.mu.Lock()
	l.names = append(l.names, name)
	l.mu.Unlock()
}

func (l *fileList) sorted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	sort.Strings(l.names)
	return l.names
}

// sandbox resolves and loads every module itself, so nothing esbuild would
// otherwise find on disk — node_modules, Node's built-ins, a path outside the
// plugin — can be reached.
func sandbox(fsys fs.FS, modules map[string]string, files *fileList) api.Plugin {
	return api.Plugin{
		Name: "reproxy-sandbox",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if _, ok := modules[args.Path]; ok {
					return api.OnResolveResult{Path: args.Path, Namespace: sdkNamespace}, nil
				}
				var p string
				switch {
				case args.Kind == api.ResolveEntryPoint:
					p = path.Clean(strings.TrimPrefix(args.Path, "./"))
				case args.Namespace == pluginNamespace && (strings.HasPrefix(args.Path, "./") || strings.HasPrefix(args.Path, "../")):
					p = path.Join(path.Dir(args.Importer), args.Path)
				case args.Namespace == sdkNamespace && strings.HasPrefix(args.Path, "./"):
					// A virtual module importing the plugin's files (the test
					// runner's entry): relative to the plugin's root.
					p = path.Clean(args.Path)
				default:
					return api.OnResolveResult{}, fmt.Errorf("can't import %q: a plugin can import its own files (\"./...\") and the reproxy SDK, nothing else", args.Path)
				}
				if p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
					return api.OnResolveResult{}, fmt.Errorf("can't import %q: it is outside the plugin's directory", args.Path)
				}
				found, err := resolveFile(fsys, p)
				if err != nil {
					return api.OnResolveResult{}, fmt.Errorf("can't import %q: %w", args.Path, err)
				}
				return api.OnResolveResult{Path: found, Namespace: pluginNamespace}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: sdkNamespace}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				src := modules[args.Path]
				return api.OnLoadResult{Contents: &src, Loader: api.LoaderTS}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: pluginNamespace}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				data, err := fs.ReadFile(fsys, args.Path)
				if err != nil {
					return api.OnLoadResult{}, err
				}
				files.add(args.Path)
				src := string(data)
				return api.OnLoadResult{Contents: &src, Loader: loaderFor(args.Path)}, nil
			})
		},
	}
}

// resolveFile finds the file an import names, trying the extensions a
// TypeScript import leaves off.
func resolveFile(fsys fs.FS, p string) (string, error) {
	for _, c := range []string{p, p + ".ts", p + ".js", p + ".json", p + "/index.ts", p + "/index.js"} {
		if st, err := fs.Stat(fsys, c); err == nil && !st.IsDir() {
			if loaderFor(c) == api.LoaderNone {
				return "", fmt.Errorf("%s is not a .ts, .js or .json file", c)
			}
			return c, nil
		}
	}
	return "", errors.New("no such file")
}

func loaderFor(name string) api.Loader {
	switch path.Ext(name) {
	case ".ts", ".mts":
		return api.LoaderTS
	case ".js", ".mjs":
		return api.LoaderJS
	case ".json":
		return api.LoaderJSON
	}
	return api.LoaderNone
}

// BuildError is a failed bundle: every message esbuild reported.
type BuildError struct {
	Messages []string
}

func (e *BuildError) Error() string { return strings.Join(e.Messages, "\n") }

func buildError(msgs []api.Message) error {
	e := &BuildError{}
	for _, m := range msgs {
		text := m.Text
		if loc := m.Location; loc != nil {
			file := strings.TrimPrefix(loc.File, pluginNamespace+":")
			text = fmt.Sprintf("%s:%d:%d: %s", file, loc.Line, loc.Column+1, m.Text)
		}
		e.Messages = append(e.Messages, text)
	}
	return e
}
