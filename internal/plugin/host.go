package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/elide"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/filter"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/jsrt"
	"reproxy/internal/randid"
)

// installHost gives the runtime __reproxy_host: the functions the SDK
// wraps. They run on the plugin's goroutine; anything that waits (the
// network, the database) runs on another and settles a promise.
func (r *Running) installHost(vm *goja.Runtime) {
	h := vm.NewObject()
	set := func(name string, fn any) {
		if err := h.Set(name, fn); err != nil {
			panic(err)
		}
	}
	set("plugin", map[string]string{"name": r.src.Name, "version": r.man.Version})

	set("log", func(level, msg string) {
		r.logf(map[string]slog.Level{"debug": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError}[level], msg)
	})

	set("setting", func(key string) goja.Value {
		raw, ok := r.setting(key)
		if !ok {
			return goja.Undefined()
		}
		return jsonValue(vm, raw)
	})
	set("settings", func() goja.Value {
		out := vm.NewObject()
		for k, v := range *r.settings.Load() {
			out.Set(k, jsonValue(vm, v))
		}
		return out
	})

	set("storageGet", func(scope, key string) *goja.Promise {
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			raw, ok, err := r.store(scope).get(key)
			if err != nil || !ok {
				return func(*goja.Runtime) goja.Value { return goja.Undefined() }, err
			}
			return func(vm *goja.Runtime) goja.Value { return jsonValue(vm, raw) }, nil
		})
	})
	set("storageSet", func(scope, key string, value goja.Value) *goja.Promise {
		raw, err := json.Marshal(value.Export())
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, fmt.Errorf("storage: the value isn't JSON: %w", err)
			}
			return undefined, r.store(scope).set(key, raw)
		})
	})
	set("storageDelete", func(scope, key string) *goja.Promise {
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			return undefined, r.store(scope).delete(key)
		})
	})
	set("storageKeys", func(scope string) *goja.Promise {
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			keys, err := r.store(scope).keys()
			return func(vm *goja.Runtime) goja.Value { return vm.ToValue(nonNil(keys)) }, err
		})
	})

	set("flow", func(id int64) goja.Value {
		f, ok := r.deps.Store.Get(uint64(id))
		if !ok {
			return goja.Undefined()
		}
		return flowValue(vm, &f)
	})
	set("flows", func(q map[string]any) goja.Value {
		limit := clampLimit(q["limit"])
		host, _ := q["host"].(string)
		targetID, _ := q["targetId"].(string)
		source, _ := q["source"].(string)
		return r.flowList(vm, limit, func(f *capture.Flow) bool {
			return (targetID == "" || f.TargetID == targetID) &&
				(host == "" || strings.EqualFold(hostPart(f.Host), host)) &&
				(source == "" || sourceName(f.Source) == source)
		})
	})
	set("searchFlows", func(text string, regex bool, limit int) goja.Value {
		m, err := filter.Compile(text, regex)
		if err != nil {
			panic(r.rt.Error(err))
		}
		return r.flowList(vm, clampLimit(limit), func(f *capture.Flow) bool { return m.Match(f.SearchText()) })
	})

	set("recordFinding", func(d map[string]any) *goja.Promise {
		draft, err := r.findingDraft(d)
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, err
			}
			if !r.deps.Findings.Recording() {
				return nil, errors.New("E_NO_ENGAGEMENT: findings can't be recorded with no engagement open")
			}
			f, created, err := r.deps.Findings.Record(draft)
			if err != nil {
				return nil, err
			}
			return func(vm *goja.Runtime) goja.Value {
				o := vm.NewObject()
				o.Set("finding", findingValue(vm, f))
				o.Set("created", created)
				return o
			}, nil
		})
	})
	set("findings", func(q map[string]any) goja.Value {
		var query findings.Query
		query.Types = stringList(q["types"])
		if id, ok := q["flowId"].(int64); ok {
			query.FlowID = uint64(id)
		}
		list := r.deps.Findings.Select(query)
		out := make([]goja.Value, len(list))
		for i, f := range list {
			out[i] = findingValue(vm, f)
		}
		return vm.NewArray(toAny(out)...)
	})
	set("findingTypes", func() []map[string]string {
		types := r.deps.Findings.Types()
		out := make([]map[string]string, len(types))
		for i, t := range types {
			out[i] = map[string]string{"id": t.ID, "name": t.Name, "color": t.Color}
		}
		return out
	})

	set("addFile", func(d map[string]any) *goja.Promise {
		draft, err := r.fileDraft(d)
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, err
			}
			f, created, err := r.deps.Files.Record(draft)
			if err != nil {
				if errors.Is(err, files.ErrNoRoot) {
					return nil, errors.New("E_NO_ENGAGEMENT: files can't be recovered with nowhere to write them")
				}
				return nil, err
			}
			return func(vm *goja.Runtime) goja.Value {
				o := vm.NewObject()
				o.Set("file", fileValue(vm, f))
				o.Set("created", created)
				return o
			}, nil
		})
	})
	set("addFiles", func(list []goja.Value) *goja.Promise {
		drafts, err := r.fileDrafts(list)
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, err
			}
			res, err := r.deps.Files.RecordAll(drafts, files.Limits{})
			if err != nil && res.Files == 0 {
				if errors.Is(err, files.ErrNoRoot) {
					return nil, errors.New("E_NO_ENGAGEMENT: files can't be recovered with nowhere to write them")
				}
				return nil, err
			}
			return func(vm *goja.Runtime) goja.Value { return recoveryValue(vm, res) }, nil
		})
	})
	set("files", func(q map[string]any) goja.Value {
		query := files.Query{
			Batch: str(q["batch"]), Kind: str(q["kind"]), Author: str(q["author"]),
			Host: str(q["host"]), Entity: str(q["entity"]), Dir: str(q["dir"]),
			Text: str(q["text"]), Limit: clampLimit(q["limit"]),
		}
		query.Source = files.Source(str(q["source"]))
		query.TextOnly, _ = q["textOnly"].(bool)
		if id, ok := q["flowId"].(int64); ok && id > 0 {
			query.FlowID = uint64(id)
		}
		list := r.deps.Files.Select(query)
		out := make([]any, len(list))
		for i, f := range list {
			out[i] = fileValue(vm, f)
		}
		return vm.NewArray(out...)
	})
	set("fileBatches", func() goja.Value {
		list := r.deps.Files.Batches()
		out := make([]any, len(list))
		for i, b := range list {
			out[i] = batchValue(vm, b)
		}
		return vm.NewArray(out...)
	})
	set("readFile", func(ref goja.Value, max int) *goja.Promise {
		id, path := fileRef(ref)
		limit := int64(max)
		if limit <= 0 || limit > maxReadBytes {
			limit = maxReadBytes
		}
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			f, ok := files.File{}, false
			if id != 0 {
				f, ok = r.deps.Files.Get(id)
			} else {
				f, ok = r.deps.Files.ByPath(path)
			}
			if !ok {
				return nil, fmt.Errorf("files.read: no recovered file %s", fileLabel(id, path))
			}
			// One byte over the limit, so a file exactly as long as it is
			// not reported as cut short.
			data, err := r.deps.Files.Read(f.ID, limit+1)
			if err != nil {
				return nil, fmt.Errorf("files.read: %w", err)
			}
			truncated := int64(len(data)) > limit
			// elide.Prefix counts bytes but never splits a rune: a
			// recovered file is arbitrary bytes, and half a rune is
			// invalid UTF-8 by the time it reaches the plugin.
			text := elide.Prefix(string(data), int(limit))
			return func(vm *goja.Runtime) goja.Value {
				o := vm.NewObject()
				o.Set("file", fileValue(vm, f))
				o.Set("text", strings.ToValidUTF8(text, "\ufffd"))
				o.Set("truncated", truncated)
				return o
			}, nil
		})
	})

	set("targets", func() []map[string]any {
		list := r.deps.Targets.List()
		out := make([]map[string]any, len(list))
		for i, t := range list {
			out[i] = targetValue(t)
		}
		return out
	})
	set("matchTarget", func(raw string) goja.Value {
		m, ok := r.matchTarget(raw)
		if !ok {
			return goja.Undefined()
		}
		return vm.ToValue(map[string]string{"id": m.ID, "name": m.Name, "color": m.Color})
	})

	set("addUrl", func(d map[string]any) *goja.Promise {
		disc, err := r.discovery(d)
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, err
			}
			e, created, err := r.deps.Entities.Discover(disc)
			if err != nil && e.Key == "" {
				return nil, err
			}
			return func(vm *goja.Runtime) goja.Value {
				o := vm.NewObject()
				o.Set("entity", entityValue(vm, e))
				o.Set("created", created)
				return o
			}, nil
		})
	})
	set("addHost", func(d map[string]any) *goja.Promise {
		disc, err := r.hostDiscovery(d)
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			if err != nil {
				return nil, err
			}
			e, created, err := r.deps.Entities.DiscoverHost(disc)
			if err != nil && e.Key == "" {
				return nil, fmt.Errorf("entities.addHost: %w", err)
			}
			return func(vm *goja.Runtime) goja.Value {
				o := vm.NewObject()
				o.Set("entity", entityValue(vm, e))
				o.Set("created", created)
				return o
			}, nil
		})
	})
	set("entity", func(keyOrURL string) goja.Value {
		e, ok := lookupEntity(r.deps.Entities, keyOrURL)
		if !ok {
			return goja.Undefined()
		}
		return entityValue(vm, e)
	})
	set("entities", func(q map[string]any) goja.Value {
		targetID, _ := q["targetId"].(string)
		host, _ := q["host"].(string)
		typ, _ := q["type"].(string)
		parent, hasParent := q["parent"].(string)
		var out []any
		for _, e := range r.deps.Entities.ByTarget(targetID) {
			switch {
			case typ != "" && string(e.Type) != typ:
			case host != "" && !strings.EqualFold(e.Host, host):
			case hasParent && e.Parent != parent:
			default:
				out = append(out, entityValue(vm, e))
			}
		}
		return vm.NewArray(out...)
	})
	set("setEntityKV", func(key, name string, value goja.Value) *goja.Promise {
		v := value.String()
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			e, err := r.deps.Entities.SetKV(key, name, v)
			if err != nil && e.Key == "" {
				return nil, fmt.Errorf("entities.set: %w", err)
			}
			return func(vm *goja.Runtime) goja.Value { return entityValue(vm, e) }, nil
		})
	})
	set("deleteEntityKV", func(key, name string) *goja.Promise {
		return r.async(func() (func(*goja.Runtime) goja.Value, error) {
			e, err := r.deps.Entities.DeleteKV(key, name)
			if err != nil && e.Key == "" {
				return nil, fmt.Errorf("entities.unset: %w", err)
			}
			return func(vm *goja.Runtime) goja.Value { return entityValue(vm, e) }, nil
		})
	})

	set("send", func(req map[string]any, opts map[string]any) *goja.Promise { return r.send(req, opts) })

	set("held", func() goja.Value {
		var out []any
		for _, m := range r.deps.Intercept.Held() {
			out = append(out, map[string]any{
				"flowId": m.FlowID, "direction": m.Dir.String(),
				"message": messageValue(vm, m.Raw, int64(len(m.Raw)), false, ""),
			})
		}
		return vm.NewArray(out...)
	})
	set("forward", func(id int64, text goja.Value) error {
		var edited *string
		if text != nil && !goja.IsUndefined(text) && !goja.IsNull(text) {
			s := text.String()
			edited = &s
		}
		return r.deps.Intercept.Forward(uint64(id), edited)
	})
	set("drop", func(id int64) error { return r.deps.Intercept.Drop(uint64(id)) })

	set("scan", func(text string, patterns []goja.Value, max int) goja.Value { return r.scan(vm, text, patterns, max) })
	set("urls", func(text, base, contentType string, limit int) ([]string, error) {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("urls: base %q is not an absolute URL", base)
		}
		mt := contentType
		if mt == "" {
			mt = "text/html"
		}
		var out []string
		for _, found := range entities.ExtractURLs(mt, []byte(text), u, limit) {
			out = append(out, found.String())
		}
		return nonNil(out), nil
	})

	vm.Set("__reproxy_host", h)
}

var undefined = func(*goja.Runtime) goja.Value { return goja.Undefined() }

// async runs work on a goroutine of its own and returns a promise that
// settles with its result on the plugin's. The result is built on the
// plugin's goroutine (values belong to the runtime), with the depth the
// call was made at restored, so a send after an await still knows how deep
// it is.
func (r *Running) async(work func() (func(*goja.Runtime) goja.Value, error)) *goja.Promise {
	vm := r.rt.VM()
	p, resolve, reject := vm.NewPromise()
	depth := r.depth
	go func() {
		build, err := work()
		r.rt.Post(func(rt *jsrt.Runtime) {
			saved := r.depth
			r.depth = depth
			defer func() { r.depth = saved }()
			if err != nil {
				reject(rt.Error(err))
				return
			}
			resolve(build(rt.VM()))
		})
	}()
	return p
}

func (r *Running) flowList(vm *goja.Runtime, limit int, keep func(*capture.Flow) bool) goja.Value {
	all := r.deps.Store.List()
	var out []any
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- { // newest first
		if keep(&all[i]) {
			out = append(out, flowValue(vm, &all[i]))
		}
	}
	return vm.NewArray(out...)
}

func clampLimit(v any) int {
	n := 200
	switch x := v.(type) {
	case int64:
		n = int(x)
	case int:
		n = x
	case float64:
		n = int(x)
	}
	if n <= 0 {
		n = 200
	}
	if n > 1000 {
		n = 1000
	}
	return n
}

// findingDraft turns the SDK's FindingDraft into a findings.Draft, as this
// plugin's.
func (r *Running) findingDraft(d map[string]any) (findings.Draft, error) {
	value, _ := d["value"].(string)
	notes, _ := d["notes"].(string)
	if strings.TrimSpace(notes) == "" {
		return findings.Draft{}, errors.New("findings.record: notes are required — say why the plugin raised it")
	}
	draft := findings.Draft{
		Value: value, Notes: notes, Types: stringList(d["types"]),
		Source: findings.SourcePlugin, Author: r.src.Name,
	}
	draft.Hidden, _ = d["hidden"].(bool)
	switch fields := d["fields"].(type) {
	case []any:
		for _, f := range fields {
			if m, ok := f.(map[string]any); ok {
				k, _ := m["key"].(string)
				v, _ := m["value"].(string)
				draft.Fields = append(draft.Fields, findings.Field{Key: k, Value: v})
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			draft.Fields = append(draft.Fields, findings.Field{Key: k, Value: fmt.Sprint(fields[k])})
		}
	}
	if id, ok := d["flowId"].(int64); ok && id > 0 {
		draft.Link.FlowID = uint64(id)
		if f, ok := r.deps.Store.Get(uint64(id)); ok {
			draft.Link.Method, draft.Link.URL = f.Method, f.URL()
		}
	}
	draft.Link.Location, _ = d["location"].(string)
	draft.Link.Detail, _ = d["detail"].(string)
	// Link the finding to any recovered files it named, by id. The path is
	// looked up now so the sighting carries it even if the file is replaced.
	if ids, ok := d["files"].([]any); ok && r.deps.Files != nil {
		for _, raw := range ids {
			var id uint64
			switch n := raw.(type) {
			case int64:
				if n > 0 {
					id = uint64(n)
				}
			case float64:
				if n > 0 {
					id = uint64(n)
				}
			}
			if id == 0 {
				continue
			}
			if f, ok := r.deps.Files.Get(id); ok {
				draft.Links = append(draft.Links, findings.RecoveredFileLink(f.Path))
			}
		}
	}
	return draft, nil
}

// maxReadBytes bounds files.read: enough for a module of real source, not
// enough for a plugin to pull a whole build into its runtime in one call.
const maxReadBytes = 8 << 20 // 8 MiB

// fileDraft turns the SDK's FileDraft into a files.Draft, as this plugin's.
// The source and the author are core's to set, whatever the plugin passed:
// a file says who recovered it.
func (r *Running) fileDraft(d map[string]any) (files.Draft, error) {
	path, _ := d["path"].(string)
	if strings.TrimSpace(path) == "" {
		return files.Draft{}, errors.New("files.add: path is required")
	}
	content, err := contentBytes(d["content"])
	if err != nil {
		return files.Draft{}, err
	}
	draft := files.Draft{
		Path: path, Content: content, Source: files.SourcePlugin, Author: r.src.Name,
		Kind: str(d["kind"]), Notes: str(d["notes"]),
		Batch: str(d["batch"]), BatchLabel: str(d["batchLabel"]),
		Method: str(d["method"]), URL: str(d["url"]), Host: str(d["host"]),
		Entity: str(d["entity"]),
	}
	if id, ok := d["flowId"].(int64); ok && id > 0 {
		draft.FlowID = uint64(id)
		if f, ok := r.deps.Store.Get(uint64(id)); ok {
			if draft.Method == "" {
				draft.Method = f.Method
			}
			if draft.URL == "" {
				draft.URL = f.URL()
			}
			if draft.Host == "" {
				draft.Host = f.Host
			}
		}
	}
	return draft, nil
}

// fileDrafts turns a whole recovery's drafts into files.Drafts, as this
// plugin's. They share one batch — the first one given, or one core makes —
// so the recovery ends with a single file.on_batch_complete.
func (r *Running) fileDrafts(list []goja.Value) ([]files.Draft, error) {
	if len(list) == 0 {
		return nil, errors.New("files.addAll: nothing to recover")
	}
	out := make([]files.Draft, 0, len(list))
	for i, v := range list {
		m, ok := v.Export().(map[string]any)
		if !ok {
			return nil, fmt.Errorf("files.addAll: file %d is not a file to recover", i+1)
		}
		d, err := r.fileDraft(m)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if out[0].Batch == "" {
		batch := randid.Hex(8)
		for i := range out {
			out[i].Batch = batch
		}
	}
	return out, nil
}

// contentBytes is what a plugin asked to be written: a string, or a byte
// array (Uint8Array, or a plain array of numbers, which is how goja exports
// one).
func contentBytes(v any) ([]byte, error) {
	switch c := v.(type) {
	case nil:
		return nil, errors.New("files.add: content is required")
	case string:
		if len(c) > maxWriteBytes {
			return nil, fmt.Errorf("files.add: content is %d bytes, over the %d-byte limit", len(c), maxWriteBytes)
		}
		return []byte(c), nil
	case []byte:
		if len(c) > maxWriteBytes {
			return nil, fmt.Errorf("files.add: content is %d bytes, over the %d-byte limit", len(c), maxWriteBytes)
		}
		return c, nil
	case []any:
		if len(c) > maxWriteBytes {
			return nil, fmt.Errorf("files.add: content is %d bytes, over the %d-byte limit", len(c), maxWriteBytes)
		}
		out := make([]byte, len(c))
		for i, b := range c {
			n, ok := b.(int64)
			if !ok {
				return nil, errors.New("files.add: content must be a string or bytes")
			}
			out[i] = byte(n)
		}
		return out, nil
	}
	return nil, errors.New("files.add: content must be a string or bytes")
}

// maxWriteBytes bounds one files.add, so a plugin cannot write a file
// larger than anything it could have read in the first place.
const maxWriteBytes = 64 << 20 // 64 MiB

// fileRef reads what files.read was given: a file's id, its path, or a
// RecoveredFile.
func fileRef(v goja.Value) (id uint64, path string) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return 0, ""
	}
	switch x := v.Export().(type) {
	case int64:
		if x > 0 {
			return uint64(x), ""
		}
	case float64:
		if x > 0 {
			return uint64(x), ""
		}
	case string:
		return 0, x
	case map[string]any:
		if n, ok := x["id"].(int64); ok && n > 0 {
			return uint64(n), ""
		}
		if p, ok := x["path"].(string); ok {
			return 0, p
		}
	}
	return 0, ""
}

// fileLabel names the file a call asked for, for an error message.
func fileLabel(id uint64, path string) string {
	if id != 0 {
		return fmt.Sprint(id)
	}
	if path == "" {
		return "(none given)"
	}
	return path
}

// str reads a string field of a plain object a plugin passed.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// lookupEntity finds an entity by its key, or else the URL a concrete URL
// falls under.
func lookupEntity(store *entities.Store, keyOrURL string) (entities.Entity, bool) {
	if e, ok := store.Get(keyOrURL); ok {
		return e, true
	}
	// A bare name or address is its name host's (//name), unless a domain
	// has that key.
	if !strings.Contains(keyOrURL, "/") {
		if e, ok := store.Get(entities.HostKey("", keyOrURL)); ok {
			return e, true
		}
	}
	u, err := url.Parse(keyOrURL)
	if err != nil || u.Host == "" {
		return entities.Entity{}, false
	}
	return store.Lookup(u.Scheme, u.Host, u.Path)
}

// discovery turns the SDK's UrlDraft into an entities.Discovery, as this
// plugin's.
func (r *Running) discovery(d map[string]any) (entities.Discovery, error) {
	raw, _ := d["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return entities.Discovery{}, fmt.Errorf("entities.addUrl: %q is not an absolute http(s) URL", raw)
	}
	disc := entities.Discovery{
		Address: entities.AddressOf(u.Scheme, u.Host, u.Path),
		Link:    entities.Link{Source: entities.PluginOrigin(r.src.Name)},
	}
	if m, ok := r.matchTarget(raw); ok {
		disc.TargetID = m.ID
	}
	disc.Link.Detail, _ = d["detail"].(string)
	if id, ok := d["flowId"].(int64); ok && id > 0 {
		disc.Link.FromFlowID = uint64(id)
		if f, ok := r.deps.Store.Get(uint64(id)); ok {
			from := entities.AddressOf(map[bool]string{true: "https", false: "http"}[f.Secure], f.Host, pathOnly(f.Path))
			disc.Link.FromURL = from.Scheme + "://" + from.Host + from.Concrete
			if disc.TargetID == "" {
				disc.TargetID = f.TargetID
			}
		}
	}
	// from names where the plugin learnt of the URL when no flow did: a
	// lookup made for a target, say. A URL on a host no target covers is
	// Related when from is on one that a target does.
	if rawFrom, _ := d["from"].(string); rawFrom != "" {
		f, err := url.Parse(rawFrom)
		if err != nil || f.Host == "" || (f.Scheme != "http" && f.Scheme != "https") {
			return entities.Discovery{}, fmt.Errorf("entities.addUrl: from %q is not an absolute http(s) URL", rawFrom)
		}
		from := entities.AddressOf(f.Scheme, f.Host, f.Path)
		disc.Link.FromURL = from.Scheme + "://" + from.Host + from.Concrete
	}
	return disc, nil
}

// hostDiscovery turns the SDK's HostDraft into an entities.HostDiscovery,
// as this plugin's. The name and addresses are checked by the store.
func (r *Running) hostDiscovery(d map[string]any) (entities.HostDiscovery, error) {
	disc := entities.HostDiscovery{Link: entities.Link{Source: entities.PluginOrigin(r.src.Name)}}
	disc.Name, _ = d["host"].(string)
	disc.Link.Detail, _ = d["detail"].(string)
	if list, ok := d["addresses"].([]any); ok {
		for _, a := range list {
			s, ok := a.(string)
			if !ok {
				return disc, fmt.Errorf("entities.addHost: addresses must be strings")
			}
			disc.Addresses = append(disc.Addresses, s)
		}
	}
	if rawFrom, _ := d["from"].(string); rawFrom != "" {
		f, err := url.Parse(rawFrom)
		if err != nil || f.Host == "" || (f.Scheme != "http" && f.Scheme != "https") {
			return disc, fmt.Errorf("entities.addHost: from %q is not an absolute http(s) URL", rawFrom)
		}
		from := entities.AddressOf(f.Scheme, f.Host, f.Path)
		disc.Link.FromURL = from.Scheme + "://" + from.Host + from.Concrete
	}
	return disc, nil
}

func (r *Running) matchTarget(raw string) (targetMatch, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return targetMatch{}, false
	}
	set := r.deps.Targets.Set()
	ip, _ := netip.ParseAddr(u.Hostname())
	m, ok := set.Match(u.Hostname(), ip, u.RequestURI())
	return targetMatch{ID: m.ID, Name: m.Name, Color: m.Color}, ok
}

type targetMatch struct{ ID, Name, Color string }

func pathOnly(p string) string {
	path, _, _ := strings.Cut(p, "?")
	return path
}

func hostPart(host string) string {
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.Contains(h, "[") {
		return h
	}
	return host
}

// scan runs RE2 patterns over text, in Go.
func (r *Running) scan(vm *goja.Runtime, text string, patterns []goja.Value, max int) goja.Value {
	if max <= 0 || max > 10000 {
		max = 10000
	}
	var out []any
	for _, p := range patterns {
		name, src := p.String(), p.String()
		if obj, ok := p.(*goja.Object); ok {
			src = obj.Get("pattern").String()
			name = src
			if n := obj.Get("name"); n != nil && !goja.IsUndefined(n) {
				name = n.String()
			}
		}
		re, err := r.regex(src)
		if err != nil {
			panic(r.rt.Error(fmt.Errorf("text.scan: pattern %q: %w", src, err)))
		}
		for _, loc := range re.FindAllStringSubmatchIndex(text, max-len(out)) {
			groups := make([]string, 0, len(loc)/2-1)
			for g := 2; g < len(loc); g += 2 {
				if loc[g] >= 0 {
					groups = append(groups, text[loc[g]:loc[g+1]])
				} else {
					groups = append(groups, "")
				}
			}
			out = append(out, map[string]any{
				"pattern": name, "match": text[loc[0]:loc[1]], "index": runeIndex(text, loc[0]), "groups": groups,
			})
		}
		if len(out) >= max {
			break
		}
	}
	return vm.NewArray(out...)
}

// regex compiles a pattern once per plugin.
func (r *Running) regex(src string) (*regexp.Regexp, error) {
	if re, ok := r.regexes[src]; ok {
		return re, nil
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, err
	}
	if len(r.regexes) < 1000 {
		r.regexes[src] = re
	}
	return re, nil
}

// runeIndex converts a byte offset into the UTF-16 index JavaScript strings
// use.
func runeIndex(s string, byteOff int) int {
	n := 0
	for _, c := range s[:byteOff] {
		if c >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func jsonValue(vm *goja.Runtime, raw json.RawMessage) goja.Value {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return goja.Undefined()
	}
	return vm.ToValue(v)
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toAny(vs []goja.Value) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = v
	}
	return out
}

// directionName is how the SDK names an intercept direction.
func directionName(d intercept.Direction) string { return d.String() }
