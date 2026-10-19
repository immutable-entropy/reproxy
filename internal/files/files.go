// Package files is what the work recovered: the files a plugin, an agent or
// the user pulled out of the traffic and core wrote to disk.
//
// A recovered file is the original source a source map carries, a bundle
// taken apart, an asset downloaded for a closer look. The bytes live under
// the open engagement (<engagement>/recovered/…, 0600 like everything else
// holding traffic); this package owns the record of them: where each one
// came from, what produced it, which flow and which Explorer entity it
// belongs to, and what it hashes to.
//
// Why it is core rather than a plugin's business: deciding what to recover
// and producing the bytes is a plugin's job, but a plugin has no
// filesystem, and — the reason the object exists — a recovered file is
// something *other* plugins want to act on. Core writes the file, records
// it and emits the event, so a plugin that scans text for secrets can hook
// file.on_created and read recovered client source with the code it already
// has for response bodies, rather than every recovering plugin carrying its
// own copy of every scanner.
//
// Each file is deduplicated on its path (Store.Record): recovering the same
// path again with the same bytes is a no-op and raises no event, so a
// repeated recovery doesn't set every scanner off a second time. Different
// bytes at a path already recorded replace the file and raise Updated.
//
// Like findings, files belong to the open engagement and are kept in its
// capture.db (package archive owns that schema), they survive Clear
// Requests, and nothing here runs on the proxy's goroutine: a Store commits
// synchronously, at the pace a human, an agent or a plugin recovers things.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"reproxy/internal/elide"
)

// Source says who recovered a file. It is the vocabulary a plugin's match
// filter uses, so it matches findings.Source.
type Source string

const (
	// SourceUser is a file the user recovered themselves, from the traffic
	// list's menu.
	SourceUser Source = "user"
	// SourceAgent is a file an AI agent recovered through an MCP tool.
	SourceAgent Source = "agent"
	// SourcePlugin is a file a plugin recovered.
	SourcePlugin Source = "plugin"
)

// The kinds core itself records. Kind is free-form — a plugin labels what
// it recovered however it likes — but these are the labels a plugin's
// filter can count on, so a scanner can ask for the application's own
// source and not be handed every dependency in node_modules.
const (
	// KindSourceMap is the application's own source, out of a source map.
	KindSourceMap = "source-map"
	// KindSourceMapDependency is a dependency's source, out of the same map.
	KindSourceMapDependency = "source-map-dependency"
)

// Sources lists who can recover a file, for a tool schema and for a
// plugin's filter.
var Sources = []Source{SourceUser, SourceAgent, SourcePlugin}

func (s Source) valid() bool { return slices.Contains(Sources, s) }

// File is one recovered file: the record, not the bytes. Read the bytes
// with Store.Read.
type File struct {
	// ID is the file's row in the engagement's database, so it is stable
	// across sessions.
	ID uint64 `json:"id"`
	// Path is where the file is, relative to Store.Root, always
	// slash-separated and always inside the root (see SafeRel). It is the
	// dedup key: one record per path.
	Path string `json:"path"`
	// Size is the file's length in bytes and SHA256 its hash, so a
	// recovery that recovers the same path twice can tell whether anything
	// changed.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// MediaType is what the file is, worked out from its name and its bytes
	// (see MediaType). A plugin's content_types filter tests it.
	MediaType string `json:"media_type"`
	// Kind is a free-form label for what produced the file —
	// "source-map", "download" — so a plugin can target just the kind it
	// cares about.
	Kind string `json:"kind,omitempty"`
	// Source is who recovered it; Author names the plugin or agent session.
	Source Source `json:"source"`
	Author string `json:"author,omitempty"`
	// Batch groups the files one recovery produced: unpacking a source map
	// writes thousands of files in one go, and that is one batch (see
	// Store.RecordAll). It is empty for a file recovered on its own.
	Batch string `json:"batch,omitempty"`
	// BatchLabel is what the batch reads as: the map's URL, say.
	BatchLabel string `json:"batch_label,omitempty"`
	// Notes is free-form text about this file.
	Notes string `json:"notes,omitempty"`
	// FlowID is the flow the file was recovered from, 0 when none is known,
	// and Method/URL/Host a snapshot of it taken when the file was
	// recorded — so a recovered file still says where it came from after
	// the traffic has been cleared. FlowID stops resolving then.
	FlowID uint64 `json:"flow_id,omitempty"`
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	Host   string `json:"host,omitempty"`
	// Entity is the key of the Explorer entity the file belongs to — the
	// URL it was recovered from, usually — or empty.
	Entity  string    `json:"entity,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Name is the file's base name.
func (f File) Name() string {
	if i := strings.LastIndex(f.Path, "/"); i >= 0 {
		return f.Path[i+1:]
	}
	return f.Path
}

// Dir is the directory the file sits in, relative to the root, or "" at the
// top.
func (f File) Dir() string {
	i := strings.LastIndex(f.Path, "/")
	if i < 0 {
		return ""
	}
	return f.Path[:i]
}

// Ext is the file's extension, lowercase, with its dot, or "".
func (f File) Ext() string { return strings.ToLower(path.Ext(f.Path)) }

// Text reports whether the file is text, so it can be shown in an editor or
// handed to a scanner.
func (f File) Text() bool { return isTextMedia(f.MediaType) }

// Summary is the one-line description of a file, for a list row or tool
// output.
func (f File) Summary() string { return elide.Runes(f.Path, 160) }

// SearchText is what a text filter matches a file against: its path, its
// notes, its kind and where it came from. It lives here so the Files
// filter, tool output and plugins share one notion of a file's searchable
// content.
func (f File) SearchText() string {
	var b strings.Builder
	b.WriteString(f.Path)
	b.WriteByte('\n')
	b.WriteString(f.Notes)
	b.WriteByte('\n')
	b.WriteString(f.Kind)
	b.WriteByte('\n')
	b.WriteString(f.URL)
	b.WriteByte('\n')
	b.WriteString(f.BatchLabel)
	return b.String()
}

// Batch is one recovery: the files it produced, summarised. The UI groups a
// list by it and a plugin that was flooded with per-file events drains a
// batch with Store.Select (see Query.Batch).
type Batch struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Files and Bytes are what the batch holds now — a file deleted since
	// is not counted.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Kind, Source, Author, Host and FlowID come from the batch's files,
	// which share them.
	Kind    string    `json:"kind,omitempty"`
	Source  Source    `json:"source"`
	Author  string    `json:"author,omitempty"`
	Host    string    `json:"host,omitempty"`
	FlowID  uint64    `json:"flow_id,omitempty"`
	Created time.Time `json:"created"`
}

// Draft is a file being recorded. It is the one input to Store.Record,
// whichever route is recovering: the user's menu item, an agent's tool or a
// plugin's files.add.
type Draft struct {
	// Path is where to put the file, relative to the store's root. It comes
	// from a source map, a response or a plugin, so it is untrusted: Record
	// runs it through SafeRel and refuses a draft with nothing safe left.
	// Callers that lay out a tree put the prefix here too
	// ("app.test/main/src/api.ts").
	Path string
	// Content is the bytes to write.
	Content []byte
	// Kind labels what produced the file; Notes is free-form text.
	Kind  string
	Notes string
	// Source and Author say who is recovering this. An empty Source is
	// treated as SourceUser.
	Source Source
	Author string
	// Batch and BatchLabel group this file with the rest of one recovery.
	// Store.RecordAll fills them in; a lone Record may set them or leave
	// them empty.
	Batch      string
	BatchLabel string
	// FlowID, Method, URL and Host say which request the file came out of;
	// Entity is the Explorer entity it belongs to. All are optional.
	FlowID uint64
	Method string
	URL    string
	Host   string
	Entity string
}

// normalized tidies a draft before it is recorded.
func (d Draft) normalized() Draft {
	d.Kind = strings.TrimSpace(d.Kind)
	d.Notes = strings.TrimRight(d.Notes, " \t\r\n")
	d.Author = strings.TrimSpace(d.Author)
	d.Batch = strings.TrimSpace(d.Batch)
	d.BatchLabel = strings.TrimSpace(d.BatchLabel)
	d.Host = strings.TrimSpace(d.Host)
	d.Entity = strings.TrimSpace(d.Entity)
	if d.Source == "" {
		d.Source = SourceUser
	}
	return d
}

// validate reports whether the draft can be recorded. The path is checked
// by Record, which needs the safe form anyway.
func (d Draft) validate() error {
	if !d.Source.valid() {
		return fmt.Errorf("unknown file source %q", d.Source)
	}
	if d.Source != SourceUser && d.Author == "" {
		return fmt.Errorf("a file recovered by %s must say which one (author)", d.Source)
	}
	return nil
}

// Result reports what a recovery wrote. It is what the traffic list's menu
// item, the MCP tool and a plugin's files.add report back.
type Result struct {
	// Batch is the recovery's ID, and Dir the directory its files were
	// written under (the store's root).
	Batch string `json:"batch,omitempty"`
	Dir   string `json:"dir"`
	// Files is how many files the recovery wrote or replaced, Unchanged how
	// many were already recorded with the same bytes, and Bytes the total
	// written.
	Files     int   `json:"files"`
	Unchanged int   `json:"unchanged"`
	Bytes     int64 `json:"bytes"`
	// Skipped names what was not written, with why: an unsafe path, or a
	// limit reached.
	Skipped []string `json:"skipped,omitempty"`
	// Recorded are the files as they now stand, in the order they were
	// written.
	Recorded []File `json:"recorded,omitempty"`
}

// CommonDir is the deepest directory every file of a recovery sits under,
// relative to Dir, or "" when they share none. It is what a message tells
// the user, since a recovery lays its own tree out under the engagement's
// recovered files and that tree, not the root, is where their work is.
func (r Result) CommonDir() string {
	if len(r.Recorded) == 0 {
		return ""
	}
	common := strings.Split(r.Recorded[0].Dir(), "/")
	for _, f := range r.Recorded[1:] {
		segs := strings.Split(f.Dir(), "/")
		n := min(len(common), len(segs))
		i := 0
		for i < n && common[i] == segs[i] {
			i++
		}
		common = common[:i]
		if len(common) == 0 {
			return ""
		}
	}
	return strings.Join(common, "/")
}

// CountKind is how many of the files a recovery recorded carry a kind, for
// a message that says how much of a recovery is the application's own code
// (KindSourceMap) rather than a dependency's.
func (r Result) CountKind(kind string) int {
	n := 0
	for _, f := range r.Recorded {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// Limits bound one RecordAll so a hostile or huge recovery cannot fill the
// disk. The zero value means the defaults.
type Limits struct {
	MaxFiles int   // at most this many files in one recovery
	MaxBytes int64 // at most this many bytes in one recovery
}

const (
	defaultMaxFiles = 20000
	defaultMaxBytes = 512 << 20 // 512 MiB
	// maxSegment bounds one path segment, under the filesystem's own limit.
	maxSegment = 200
)

func (l Limits) withDefaults() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = defaultMaxFiles
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBytes
	}
	return l
}

var illegal = regexp.MustCompile(`[<>:"|?*\x00-\x1f]`)

// SafeRel turns a path from a source map, a URL or a plugin into a relative
// path that stays under the store's root, or returns ok=false if there is
// nothing safe to write. It strips any scheme/authority ("webpack://app/…",
// "file://…"), a leading NUL (a bundler's virtual-module marker), a Windows
// drive letter, and the query/hash; collapses "." and ".." so they cannot
// climb above the root; and replaces characters that are illegal in a
// filename. The result is always relative, slash-separated, and contains no
// "..".
//
// It is exported because a caller that lays out a tree ("<host>/<bundle>")
// wants the same rule for the segments it adds; Record applies it again
// regardless, since the path is untrusted whoever cleaned it.
func SafeRel(src string) (string, bool) {
	s := src
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:] // drop scheme and the "//"; keep the authority as a folder
	}
	s = strings.TrimLeft(s, "\x00")
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "//")
	// A Windows drive letter ("C:/Users/…") after slash-normalisation.
	if len(s) >= 2 && s[1] == ':' && isLetter(s[0]) {
		s = s[2:]
	}
	// The leading slash makes a leading ".." a no-op at the root, so Clean
	// cannot produce a path that climbs out.
	clean := strings.TrimPrefix(path.Clean("/"+s), "/")
	if clean == "" || clean == "." {
		return "", false
	}
	segs := strings.Split(clean, "/")
	for i, seg := range segs {
		if seg == ".." { // defensive; Clean above has removed these
			return "", false
		}
		seg = illegal.ReplaceAllString(seg, "_")
		seg = strings.TrimRight(seg, " .") // Windows forbids a trailing dot or space
		if seg == "" {
			seg = "_"
		}
		if len(seg) > maxSegment {
			seg = elide.Prefix(seg, maxSegment)
		}
		segs[i] = seg
	}
	return strings.Join(segs, "/"), true
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hash is a file's content hash, as lowercase hex.
func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// byExt is what a recovered file's extension says it is. It is a table
// rather than mime.TypeByExtension because that reads the system's
// mime.types — so it differs between machines, and calls a TypeScript
// module video/mp2t, which would send every recovered .ts file past a
// plugin filtering on text/*.
var byExt = map[string]string{
	".js": "text/javascript", ".mjs": "text/javascript", ".cjs": "text/javascript",
	".jsx": "text/jsx", ".ts": "text/typescript", ".tsx": "text/tsx",
	".json": "application/json", ".map": "application/json", ".jsonc": "application/json",
	".css": "text/css", ".scss": "text/x-scss", ".sass": "text/x-sass", ".less": "text/x-less",
	".html": "text/html", ".htm": "text/html", ".vue": "text/x-vue", ".svelte": "text/x-svelte",
	".xml": "text/xml", ".svg": "image/svg+xml", ".txt": "text/plain", ".md": "text/markdown",
	".yml": "application/yaml", ".yaml": "application/yaml", ".toml": "application/toml",
	".csv": "text/csv", ".env": "text/plain", ".ini": "text/plain", ".cfg": "text/plain",
	".sh": "text/x-shellscript", ".sql": "application/sql", ".graphql": "application/graphql",
	".php": "text/x-php", ".py": "text/x-python", ".rb": "text/x-ruby", ".go": "text/x-go",
	".java": "text/x-java", ".cs": "text/x-csharp", ".wasm": "application/wasm",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".ico": "image/vnd.microsoft.icon", ".woff": "font/woff",
	".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf", ".pdf": "application/pdf",
	".zip": "application/zip", ".gz": "application/gzip",
}

// MediaType is what a recovered file is: its extension's type, or, for a
// name that says nothing, whether its bytes are text. A plugin's
// content_types filter tests this, so a scanner can ask for text/* and
// never be handed a font.
func MediaType(name string, content []byte) string {
	if t, ok := byExt[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	if len(content) == 0 {
		return "text/plain"
	}
	head := content
	if len(head) > 1024 {
		head = head[:1024]
	}
	if isTextBytes(head) {
		return "text/plain"
	}
	return "application/octet-stream"
}

// isTextBytes reports whether a prefix looks like text: valid UTF-8 (give
// or take a rune cut off at the end of the prefix) with no NUL and few
// control bytes.
func isTextBytes(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return false
		}
	}
	trimmed := head
	for len(trimmed) > 0 && !utf8.Valid(trimmed) && len(head)-len(trimmed) < utf8.UTFMax {
		trimmed = trimmed[:len(trimmed)-1] // a rune the prefix cut in half
	}
	return utf8.Valid(trimmed)
}

// isTextMedia reports whether a media type is something an editor or a
// scanner can read as text.
func isTextMedia(mt string) bool {
	mt = strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/yaml", "application/toml", "application/sql",
		"application/graphql", "application/javascript", "application/xml",
		"application/x-www-form-urlencoded", "image/svg+xml":
		return true
	}
	return false
}

// Query selects files for a list or for tool output. The zero value matches
// everything.
type Query struct {
	// Batch keeps only the files one recovery produced.
	Batch string
	// Kind, Source and Author keep only the files with that provenance.
	Kind   string
	Source Source
	Author string
	// Host keeps only the files recovered from that host, and FlowID only
	// those from that flow; Entity only those belonging to that Explorer
	// entity.
	Host   string
	FlowID uint64
	Entity string
	// Dir keeps only the files under that directory, relative to the root.
	Dir string
	// Text keeps only the files whose SearchText contains it, case
	// insensitively.
	Text string
	// TextOnly keeps only the files an editor or a scanner can read.
	TextOnly bool
	// Limit caps the result; 0 means every match.
	Limit int
}

func (q Query) match(f File) bool {
	switch {
	case q.Batch != "" && f.Batch != q.Batch:
		return false
	case q.Kind != "" && !strings.EqualFold(f.Kind, q.Kind):
		return false
	case q.Source != "" && f.Source != q.Source:
		return false
	case q.Author != "" && !strings.EqualFold(f.Author, q.Author):
		return false
	case q.Host != "" && !strings.EqualFold(f.Host, q.Host):
		return false
	case q.FlowID != 0 && f.FlowID != q.FlowID:
		return false
	case q.Entity != "" && f.Entity != q.Entity:
		return false
	case q.TextOnly && !f.Text():
		return false
	case q.Dir != "" && !underDir(f.Path, q.Dir):
		return false
	case q.Text != "" && !strings.Contains(strings.ToLower(f.SearchText()), strings.ToLower(q.Text)):
		return false
	}
	return true
}

// underDir reports whether a file's path is inside dir.
func underDir(filePath, dir string) bool {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return true
	}
	return strings.HasPrefix(filePath, dir+"/")
}

// sortBatches orders batches newest first, so the most recent recovery is
// at the top of a list.
func sortBatches(list []Batch) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Created.After(list[j].Created) })
}
