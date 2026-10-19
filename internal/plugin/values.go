package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/bodyview"
	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/target"
)

// The values handed to plugin code are built on the plugin's own goroutine,
// when its function is about to be called — never on the goroutine that
// raised the event. A message's body is only decoded if the plugin asks for
// it.

// sourceName is how plugin.json and the SDK name a flow's source.
func sourceName(s capture.Source) string {
	if s == capture.SourceProxy {
		return "proxy"
	}
	return string(s)
}

var stateNames = map[capture.FlowState]string{
	capture.StatePending:      "pending",
	capture.StateComplete:     "complete",
	capture.StateFailed:       "failed",
	capture.StateHeldRequest:  "held_request",
	capture.StateHeldResponse: "held_response",
	capture.StateDropped:      "dropped",
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// flowValue is the SDK's Flow.
func flowValue(vm *goja.Runtime, f *capture.Flow) goja.Value {
	o := vm.NewObject()
	scheme := "http"
	if f.Secure {
		scheme = "https"
	}
	path, query, _ := strings.Cut(f.Path, "?")
	o.Set("id", f.ID)
	o.Set("method", f.Method)
	o.Set("url", f.URL())
	o.Set("scheme", scheme)
	o.Set("host", f.Host)
	o.Set("path", path)
	o.Set("query", query)
	o.Set("state", stateNames[f.State])
	o.Set("status", f.StatusCode)
	o.Set("statusText", f.StatusText)
	o.Set("targetId", f.TargetID)
	o.Set("source", sourceName(f.Source))
	o.Set("origin", f.Origin)
	o.Set("started", millis(f.Started))
	o.Set("durationMs", f.Duration.Milliseconds())
	o.Set("error", f.Error)
	o.Set("edited", f.Edited())
	o.Set("request", messageValue(vm, f.Request.Raw, f.Request.Size, f.Request.Truncated, ""))
	o.Set("response", messageValue(vm, f.Response.Raw, f.Response.Size, f.Response.Truncated, f.Method))
	return o
}

// messageValue is the SDK's Message: one side of an exchange. method is
// the request's, for reading a response (HEAD has no body); "" for a
// request.
func messageValue(vm *goja.Runtime, raw []byte, size int64, truncated bool, method string) goja.Value {
	o := vm.NewObject()
	o.Set("size", size)
	o.Set("truncated", truncated)
	head, body, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
	var decoded *bodyview.Rendered
	decode := func() *bodyview.Rendered {
		if decoded == nil {
			r := bodyview.Decode(body, bodyview.ParseHeaders(head), truncated)
			decoded = &r
		}
		return decoded
	}
	o.Set("empty", len(raw) == 0)
	o.Set("head", func() string { return strings.ToValidUTF8(string(head), "�") })
	o.Set("startLine", func() string {
		line, _, _ := bytes.Cut(head, []byte("\r\n"))
		return string(line)
	})
	o.Set("headers", func() []map[string]string { return headerList(head) })
	o.Set("header", func(name string) goja.Value {
		for _, h := range headerList(head) {
			if strings.EqualFold(h["name"], name) {
				return vm.ToValue(h["value"])
			}
		}
		return goja.Undefined()
	})
	o.Set("contentType", func() string { return manifest.ContentType(raw) })
	o.Set("isBinary", func() bool { return decode().Binary })
	o.Set("text", func() string {
		if r := decode(); !r.Binary {
			return r.Text
		}
		return ""
	})
	o.Set("json", func() goja.Value {
		r := decode()
		if r.Binary || len(r.Text) == 0 {
			return goja.Undefined()
		}
		var v any
		if json.Unmarshal([]byte(r.Text), &v) != nil {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})
	o.Set("raw", func() goja.Value { return bytesValue(vm, raw) })
	o.Set("body", func() goja.Value { return bytesValue(vm, decode().Decoded) })
	o.Set("editable", func() string { return intercept.EditableText(raw) })
	_ = method
	return o
}

// bytesValue is a Uint8Array over a copy of b.
func bytesValue(vm *goja.Runtime, b []byte) goja.Value {
	ab := vm.NewArrayBuffer(append([]byte(nil), b...))
	ctor := vm.Get("Uint8Array")
	v, err := vm.New(ctor, vm.ToValue(ab))
	if err != nil {
		return vm.ToValue(ab)
	}
	return v
}

// headerList reads the header lines of a message head, in order.
func headerList(head []byte) []map[string]string {
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	out := []map[string]string{}
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		out = append(out, map[string]string{"name": strings.TrimSpace(name), "value": strings.TrimSpace(value)})
	}
	return out
}

// responseValue is what http.send resolves with: the response as a Message,
// with its status and timing alongside.
func responseValue(vm *goja.Runtime, raw []byte, status int, statusText string, d time.Duration, flowID uint64, method string) goja.Value {
	o := messageValue(vm, raw, int64(len(raw)), false, method).ToObject(vm)
	o.Set("status", status)
	o.Set("statusText", statusText)
	o.Set("ok", status >= 200 && status < 300)
	o.Set("durationMs", d.Milliseconds())
	o.Set("flowId", flowID)
	return o
}

// findingValue is the SDK's Finding.
func findingValue(vm *goja.Runtime, f findings.Finding) goja.Value {
	links := make([]map[string]any, len(f.Links))
	for i, l := range f.Links {
		links[i] = map[string]any{
			"flowId": l.FlowID, "method": l.Method, "url": l.URL, "location": l.Location,
			"detail": l.Detail, "added": millis(l.Added),
		}
	}
	fields := make([]map[string]string, len(f.Fields))
	for i, fl := range f.Fields {
		fields[i] = map[string]string{"key": fl.Key, "value": fl.Value}
	}
	return vm.ToValue(map[string]any{
		"id": f.ID, "value": f.Value, "types": nonNil(f.Types), "notes": f.Notes, "fields": fields,
		"source": string(f.Source), "author": f.Author, "viewed": f.Viewed, "hidden": f.Hidden, "links": links,
		"created": millis(f.Created), "updated": millis(f.Updated),
	})
}

// fileValue is the SDK's RecoveredFile: the record of a file the work
// recovered, not its bytes — those are read with files.read, which goes to
// disk on a goroutine of its own.
func fileValue(vm *goja.Runtime, f files.File) goja.Value {
	return vm.ToValue(map[string]any{
		"id": f.ID, "path": f.Path, "name": f.Name(), "dir": f.Dir(),
		"size": f.Size, "sha256": f.SHA256, "mediaType": f.MediaType, "text": f.Text(),
		"kind": f.Kind, "source": string(f.Source), "author": f.Author,
		"batch": f.Batch, "batchLabel": f.BatchLabel, "notes": f.Notes,
		"flowId": f.FlowID, "method": f.Method, "url": f.URL, "host": f.Host,
		"entity": f.Entity, "created": millis(f.Created), "updated": millis(f.Updated),
	})
}

// recoveryValue is the SDK's Recovery: what one files.addAll wrote.
func recoveryValue(vm *goja.Runtime, res files.Result) goja.Value {
	recorded := make([]any, len(res.Recorded))
	for i, f := range res.Recorded {
		recorded[i] = fileValue(vm, f)
	}
	o := vm.NewObject()
	o.Set("batch", res.Batch)
	o.Set("dir", res.Dir)
	o.Set("files", res.Files)
	o.Set("unchanged", res.Unchanged)
	o.Set("bytes", res.Bytes)
	o.Set("skipped", nonNil(res.Skipped))
	o.Set("recorded", vm.NewArray(recorded...))
	return o
}

// batchValue is the SDK's FileBatch: one recovery, summarised.
func batchValue(vm *goja.Runtime, b files.Batch) goja.Value {
	return vm.ToValue(map[string]any{
		"id": b.ID, "label": b.Label, "files": b.Files, "bytes": b.Bytes,
		"kind": b.Kind, "source": string(b.Source), "author": b.Author,
		"host": b.Host, "flowId": b.FlowID, "created": millis(b.Created),
	})
}

// entityValue is the SDK's Entity: what all types share, and the
// attributes of its own.
func entityValue(vm *goja.Runtime, e entities.Entity) goja.Value {
	links := make([]map[string]any, len(e.Links))
	for i, l := range e.Links {
		links[i] = map[string]any{"source": l.Source, "fromUrl": l.FromURL, "fromFlowId": l.FromFlowID, "detail": l.Detail}
	}
	kv := make(map[string]any, len(e.KV))
	for _, p := range e.KV {
		kv[p.Key] = p.Value
	}
	o := map[string]any{
		"id": e.ID, "type": string(e.Type), "key": e.Key, "parent": e.Parent, "label": e.Label(),
		"scheme": e.Scheme, "host": e.Host, "path": e.Path,
		"targetId": e.TargetID, "origin": e.Origin, "visited": e.Visited(), "hits": e.Hits,
		"links": links, "kv": kv, "firstSeen": millis(e.FirstSeen), "lastSeen": millis(e.LastSeen),
	}
	switch {
	case e.DomainAttrs != nil:
		o["suffix"], o["icann"] = e.DomainAttrs.Suffix, e.DomainAttrs.ICANN
	case e.HostAttrs != nil:
		o["port"], o["ip"], o["servers"] = e.HostAttrs.Port, e.HostAttrs.IP, nonNil(e.HostAttrs.Servers)
		o["addresses"] = nonNil(e.HostAttrs.Addresses)
	case e.URLAttrs != nil:
		ops := make([]map[string]any, len(e.URLAttrs.Operations))
		for i, op := range e.URLAttrs.Operations {
			statuses := make([]int, len(op.Responses))
			for j, r := range op.Responses {
				statuses[j] = r.Status
			}
			ops[i] = map[string]any{
				"method": op.Method, "requests": op.Requests, "query": paramNames(op.Query),
				"body": paramNames(op.Body), "statuses": statuses,
			}
		}
		kinds := make([]string, len(e.URLAttrs.Kinds))
		for i, k := range e.URLAttrs.Kinds {
			kinds[i] = string(k)
		}
		o["url"], o["operations"], o["examples"], o["kinds"], o["flowIds"] =
			e.Label(), ops, nonNil(e.URLAttrs.Examples), kinds, e.FlowIDs()
	}
	return vm.ToValue(o)
}

func paramNames(ps []*entities.Param) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// targetValue is the SDK's Target.
func targetValue(t target.Target) map[string]any {
	return map[string]any{
		"id": t.ID, "kind": string(t.Kind), "value": t.Value, "name": t.Label(), "color": t.Color,
		"enabled": t.Enabled,
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// headLine is the request line and Host of a raw request, for building a
// flow from one.
func headLine(raw []byte) (method, path, host string) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		line, _, _ := bytes.Cut(raw, []byte("\r\n"))
		parts := strings.Fields(string(line))
		if len(parts) >= 2 {
			return parts[0], parts[1], ""
		}
		return "", "", ""
	}
	return req.Method, req.RequestURI, req.Host
}

// wsMessageValue is the SDK's WebSocketMessage.
func wsMessageValue(vm *goja.Runtime, f *capture.Flow, m capture.WSMessage) goja.Value {
	o := vm.NewObject()
	o.Set("seq", m.Seq)
	o.Set("direction", m.Dir.String())
	o.Set("type", m.Type.String())
	o.Set("size", len(m.Data))
	o.Set("truncated", m.Truncated)
	o.Set("edited", m.Edited)
	o.Set("dropped", m.Dropped)
	data := m.Data
	o.Set("text", func() string {
		if m.Type == capture.WSBinary {
			return ""
		}
		return string(data)
	})
	o.Set("data", func() goja.Value { return bytesValue(vm, data) })
	return o
}

// wsEvent is the object a websocket.on_message hook is called with.
func wsEvent(vm *goja.Runtime, hook manifest.Hook, f *capture.Flow, m capture.WSMessage) goja.Value {
	o := vm.NewObject()
	o.Set("hook", string(hook))
	o.Set("flow", flowValue(vm, f))
	o.Set("message", wsMessageValue(vm, f, m))
	return o
}

// wsConnValue is the event for websocket.on_open / on_close.
func wsConnValue(vm *goja.Runtime, hook manifest.Hook, f *capture.Flow) goja.Value {
	o := vm.NewObject()
	o.Set("hook", string(hook))
	o.Set("flow", flowValue(vm, f))
	return o
}
