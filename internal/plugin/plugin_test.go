package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/replay"
	"reproxy/internal/target"
	"reproxy/internal/websocket"
)

// env is a throwaway engagement with real stores, and plugins written to
// its data directory's plugins folder.
func env(t *testing.T, plugins fstest.MapFS) (*Deps, *Manager) {
	t.Helper()
	dir := t.TempDir()
	for name, f := range plugins {
		path := filepath.Join(UserDir(dir), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ar, err := archive.Open(filepath.Join(dir, archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ar.Close() })
	fs := findings.NewStore()
	fs.SetTypes([]findings.Type{{ID: "credential", Name: "Credential", Color: "#aa0000"}})
	fs.Use(ar.DB())
	eps := entities.NewStore()
	eps.Use(ar.DB())
	targets, err := target.NewStore(filepath.Join(dir, "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic, _ := intercept.New(intercept.Settings{})
	fls := files.NewStore()
	if err := fls.Use(ar.DB(), filepath.Join(dir, "recovered")); err != nil {
		t.Fatal(err)
	}
	deps := &Deps{
		DataDir: dir, Store: capture.NewStore(), Intercept: ic, Findings: fs,
		Targets: targets, Entities: eps, Files: fls, EngagementDB: ar.DB,
		Send: func(context.Context, replay.Request) (replay.Response, error) {
			raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			return replay.Response{Raw: raw, Size: len(raw), StatusCode: 200, Status: "200 OK"}, nil
		},
		Log: slog.New(slog.DiscardHandler),
	}
	m := NewManager(deps)
	deps.Store.Subscribe(m.Dispatcher().OnFlow)
	fs.Subscribe(m.Dispatcher().OnFinding)
	eps.Subscribe(m.Dispatcher().OnEntity)
	fls.Subscribe(m.Dispatcher().OnFile)
	t.Cleanup(m.Close)
	return deps, m
}

func pluginFS(name, manifestJSON, mainTS string) fstest.MapFS {
	return fstest.MapFS{
		name + "/plugin.json": {Data: []byte(manifestJSON)},
		name + "/main.ts":     {Data: []byte(mainTS)},
	}
}

const tagger = `{
	"name": "tagger", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
	"finding_types": [{"name": "Token"}],
	"targets": [
		{"hook": "plugin.on_load", "function": "init"},
		{"hook": "traffic.on_flow_complete", "function": "scan", "match": {"content_types": ["application/json"], "status": ["2xx"]}},
		{"hook": "plugin.on_settings_change", "function": "changed"}
	],
	"settings": [{"key": "prefix", "default": "tok_"}]
}`

const taggerTS = `
import { findings, settings, storage, text, log, type Hooks } from "reproxy";
export async function init(e: Hooks["plugin.on_load"]) {
	await storage.set("loaded", e.reason);
}
export async function scan({ flow }: Hooks["traffic.on_flow_complete"]) {
	const body = flow.response.text();
	for (const m of text.scan(body, [settings.get<string>("prefix") + "[a-z0-9]+"])) {
		await findings.record({ value: m.match, types: ["Token"], notes: "token in a response", flowId: flow.id, location: "response_body" });
	}
}
export function changed(e: Hooks["plugin.on_settings_change"]) {
	log.info("prefix is now", e.settings.prefix);
}
`

func addFlow(store *capture.Store, url, contentType, body string) uint64 {
	host := strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0]
	path := "/" + strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[1]
	id := store.Add(capture.Flow{
		Method: "GET", Host: host, Path: path, Secure: true,
		Request: capture.Message{Raw: []byte("GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n")},
		Started: time.Now(),
	})
	resp := "HTTP/1.1 200 OK\r\nContent-Type: " + contentType + "\r\nContent-Length: " +
		itoa(len(body)) + "\r\n\r\n" + body
	store.Complete(id, capture.Message{Raw: []byte(resp), Size: int64(len(resp))}, 200, "200 OK", time.Now())
	return id
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPluginSeesMatchingTrafficAndRecordsFindings(t *testing.T) {
	deps, m := env(t, pluginFS("tagger", tagger, taggerTS))
	m.Rescan()
	m.Apply(Config{})
	info, _ := m.Get("tagger")
	if info.State != StateRunning {
		t.Fatalf("state = %s, problems %v", info.State, info.Problems)
	}
	// on_load ran and stored something.
	data, err := readFileStorage(deps.DataDir, "tagger")
	if err != nil || !strings.Contains(data, `"loaded":"startup"`) {
		t.Fatalf("storage = %q, %v", data, err)
	}
	// The declared type is in the catalog.
	if _, ok := deps.Findings.ResolveType("token"); !ok {
		t.Error("declared finding type missing")
	}

	addFlow(deps.Store, "https://app.test/a", "text/html", "tok_ignored") // filtered out: HTML
	id := addFlow(deps.Store, "https://app.test/api", "application/json", `{"t":"tok_abc123","u":"tok_abc123"}`)
	eventually(t, "a finding", func() bool { return deps.Findings.Len() == 1 })
	f := deps.Findings.List()[0]
	if f.Value != "tok_abc123" || f.Source != findings.SourcePlugin || f.Author != "tagger" ||
		len(f.Links) != 1 || f.Links[0].FlowID != id || f.Types[0] != "token" {
		t.Fatalf("finding = %+v", f)
	}
	eventually(t, "delivery counted", func() bool { info, _ := m.Get("tagger"); return info.Stats.Delivered >= 2 })

	// Settings: a change reaches the running plugin.
	m.Apply(Config{Overrides: map[string]Override{"tagger": {Settings: map[string]json.RawMessage{"prefix": json.RawMessage(`"key_"`)}}}})
	addFlow(deps.Store, "https://app.test/api2", "application/json", `{"k":"key_zz9"}`)
	eventually(t, "a second finding", func() bool { return deps.Findings.Len() == 2 })
	info, _ = m.Get("tagger")
	if !strings.Contains(logText(info.Log), "prefix is now key_") {
		t.Errorf("log = %v", logText(info.Log))
	}

	// Disabled: no more calls, routing empty, types withdrawn.
	m.Apply(Config{Overrides: map[string]Override{"tagger": {Status: manifest.Disabled}}})
	if m.disp.routes.Load() != nil {
		t.Error("routes left after disabling")
	}
	if _, ok := deps.Findings.ResolveType("token"); ok {
		t.Error("declared type kept after disabling")
	}
}

func logText(lines []LogLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Msg + "\n")
	}
	return b.String()
}

func readFileStorage(dataDir, name string) (string, error) {
	s := &fileStorage{path: filepath.Join(dataDir, "plugin-data", name, "storage.json")}
	if err := s.load(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s.values)
	return string(b), err
}

const mapwatch = `{
	"name": "mapwatch", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
	"finding_types": [{"name": "URL"}],
	"targets": [
		{"hook": "url.on_created", "function": "found", "match": {"hosts": ["*.app.test"]}},
		{"hook": "url.on_updated", "function": "changed"},
		{"hook": "entity.on_created", "function": "domain", "match": {"types": ["domain"]}},
		{"hook": "host.on_deleted", "function": "gone"}
	]
}`

const mapwatchTS = `
import { entities, findings, log, type Hooks } from "reproxy";
export async function found({ entity }: Hooks["url.on_created"]) {
	await findings.record({
		value: entity.url, types: ["URL"],
		notes: "in the Explorer, origin " + entity.origin + ", under " + entity.parent,
		flowId: entity.links.length ? entity.links[0].fromFlowId : entity.flowIds[0],
	});
}
export function changed({ entity }: Hooks["url.on_updated"]) {
	log.info("updated " + entity.url + " hits=" + entity.hits);
}
export async function domain({ entity }: Hooks["entity.on_created"]) {
	if (entity.type !== "domain") throw new Error("the filter let a " + entity.type + " through");
	log.info("domain " + entity.key + " suffix=" + entity.suffix);
	await entities.set(entity.key, "seen-by", "mapwatch");
	await entities.set(entity.key, "seen-by", "mapwatch"); // the same value: no update
}
export function gone({ entity }: Hooks["host.on_deleted"]) {
	log.info("deleted " + entity.key + " port=" + entity.port);
}
`

// TestEntityHooksRunWhenTheExplorerGrows covers the route a plugin takes
// when it watches the Entity Explorer instead of the traffic: the
// entities.Store subscriber, the typed and generic hooks, the filter, the
// entity the plugin is handed, and its key/value store.
func TestEntityHooksRunWhenTheExplorerGrows(t *testing.T) {
	deps, m := env(t, pluginFS("mapwatch", mapwatch, mapwatchTS))
	m.Rescan()
	m.Apply(Config{})
	if info, _ := m.Get("mapwatch"); info.State != StateRunning {
		t.Fatalf("state = %s, problems %v", info.State, info.Problems)
	}
	logged := func(what, text string) {
		t.Helper()
		eventually(t, what, func() bool {
			info, _ := m.Get("mapwatch")
			return strings.Contains(logText(info.Log), text)
		})
	}

	// A link found in a page: discovered, not visited.
	flowID := addFlow(deps.Store, "https://www.app.test/", "text/html", `<a href="/users/12">u</a>`)
	if _, _, err := deps.Entities.Discover(entities.Discovery{
		Address: entities.AddressOf("https", "www.app.test", "/users/12"),
		Link:    entities.Link{Source: entities.OriginLink, FromURL: "https://www.app.test/", FromFlowID: flowID},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a finding for the discovered URL", func() bool { return deps.Findings.Len() == 1 })
	f := deps.Findings.List()[0]
	if f.Value != "https://www.app.test/users/{id}" || f.Author != "mapwatch" || len(f.Links) != 1 ||
		f.Links[0].FlowID != flowID || !strings.Contains(f.Notes, "origin link, under https://www.app.test") {
		t.Fatalf("finding = %+v", f)
	}
	logged("the domain's creation", "domain app.test suffix=test")
	eventually(t, "the plugin's key/value pair", func() bool {
		d, _ := deps.Entities.Get("app.test")
		v, _ := d.Get("seen-by")
		return v == "mapwatch"
	})

	// A request to the same URL updates it rather than adding it.
	visit := entities.Visit{
		FlowID: flowID, Address: entities.AddressOf("https", "www.app.test", "/users/12"),
		Method: "GET", URL: "https://www.app.test/users/12", Status: 200, Seen: time.Now(),
	}
	if _, err := deps.Entities.Apply(entities.Batch{Visits: []entities.Visit{visit}}); err != nil {
		t.Fatal(err)
	}
	logged("the update", "updated https://www.app.test/users/{id} hits=1")

	// A host the filter turns away never reaches the plugin's URL hook,
	// though its domain reaches the generic one.
	if _, _, err := deps.Entities.Discover(entities.Discovery{
		Address: entities.AddressOf("https", "other.test", "/x"),
		Link:    entities.Link{Source: entities.OriginLink},
	}); err != nil {
		t.Fatal(err)
	}
	logged("the second domain", "domain other.test")
	if n := deps.Findings.Len(); n != 1 {
		t.Fatalf("findings = %d; the filtered URL reached the plugin", n)
	}

	// Deleting a host calls the host hook.
	if err := deps.Entities.Delete("https://other.test"); err != nil {
		t.Fatal(err)
	}
	logged("the deletion", "deleted https://other.test port=443")
}

// A plugin recovers files and the file hooks wake the plugins that scan
// them: the reason recovered files are core (see internal/files). The
// recoverer is not called for its own work, and a recovery of many files
// also calls the batch hook once, which is how a plugin that cannot keep up
// with the per-file events still gets the whole recovery.
func TestFileHooksCarryARecoveryToOtherPlugins(t *testing.T) {
	recoverer := `{"name": "recoverer", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"actions": [{"id": "recover", "label": "Recover", "function": "recover"}],
		"targets": [{"hook": "file.on_created", "function": "mine"}]}`
	recovererTS := `import { files, log } from "reproxy";
		export async function recover(e: any) {
			const res = await files.addAll([
				{ path: "app.test/main/src/api.ts", content: "const key = \"sk-live-123\";",
				  kind: "source-map", batch: "b1", batchLabel: e.flow.url, flowId: e.flow.id },
				{ path: "app.test/main/node_modules/vue/vue.js", content: "/* vue */",
				  kind: "dep", batch: "b1", batchLabel: e.flow.url, flowId: e.flow.id },
			]);
			return "recovered " + res.files;
		}
		export function mine(e: any) { log.info("mine " + e.file.path) }`

	// The scanner targets text files only, by kind, and reads the content
	// from disk the way a sensitive-info plugin would.
	scanner := `{"name": "scanner", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [
			{"hook": "file.on_created", "function": "scan", "match": {"types": ["source-map"], "content_types": ["text/*"]}},
			{"hook": "file.on_batch_complete", "function": "sweep"}]}`
	scannerTS := `import { files, findings, log } from "reproxy";
		export async function scan(e: any) {
			const { text } = await files.read(e.file);
			log.info("scan " + e.file.path + " " + e.file.mediaType);
			const m = text.match(/sk-live-[a-z0-9]+/);
			if (m) await findings.record({ value: m[0], types: ["credential"], notes: "a key in " + e.file.path });
		}
		export function sweep(e: any) {
			log.info("sweep " + e.batch.id + " " + e.batch.files + " " + files.list({ batch: e.batch.id }).length);
		}`

	plugins := pluginFS("recoverer", recoverer, recovererTS)
	for name, f := range pluginFS("scanner", scanner, scannerTS) {
		plugins[name] = f
	}
	deps, m := env(t, plugins)
	m.Rescan()
	m.Apply(Config{})
	for _, name := range []string{"recoverer", "scanner"} {
		if info, _ := m.Get(name); info.State != StateRunning {
			t.Fatalf("%s state = %s, problems %v", name, info.State, info.Problems)
		}
	}
	logged := func(plugin, what, text string) {
		t.Helper()
		eventually(t, what, func() bool {
			info, _ := m.Get(plugin)
			return strings.Contains(logText(info.Log), text)
		})
	}

	flowID := addFlow(deps.Store, "https://app.test/assets/main.js.map", "application/json", "{}")
	f, _ := deps.Store.Get(flowID)
	res, err := m.RunAction("recoverer", "recover", f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message != "recovered 2" {
		t.Fatalf("action said %q", res.Message)
	}

	// Both files are recorded, on disk and in the engagement.
	eventually(t, "both files recovered", func() bool { return deps.Files.Len() == 2 })
	rec, ok := deps.Files.ByPath("app.test/main/src/api.ts")
	if !ok || rec.Source != files.SourcePlugin || rec.Author != "recoverer" ||
		rec.FlowID != flowID || rec.MediaType != "text/typescript" || rec.Batch == "" {
		t.Fatalf("recorded = %+v, ok %v", rec, ok)
	}
	if data, err := deps.Files.Read(rec.ID, 0); err != nil || !strings.Contains(string(data), "sk-live-123") {
		t.Fatalf("on disk = %q, %v", data, err)
	}

	// The scanner was woken by the file it targets, read it and raised a
	// finding; the dependency its filter turns away never reached it.
	logged("scanner", "the scanner reading the recovered file", "scan app.test/main/src/api.ts text/typescript")
	eventually(t, "the finding from the recovered file", func() bool { return deps.Findings.Len() == 1 })
	if got := deps.Findings.List()[0]; got.Value != "sk-live-123" || got.Author != "scanner" {
		t.Fatalf("finding = %+v", got)
	}
	if info, _ := m.Get("scanner"); strings.Contains(logText(info.Log), "node_modules") {
		t.Error("the scanner was called for a file its filter turns away")
	}
	// The recovery as a whole reached the batch hook, and the batch's files
	// are all in the store by then.
	logged("scanner", "the batch hook", "sweep b1 2 2")
	// The plugin that recovered the files is not called for its own work.
	if info, _ := m.Get("recoverer"); strings.Contains(logText(info.Log), "mine ") {
		t.Error("the recoverer was called for the files it recovered itself")
	}

	// Recovering the same bytes again raises nothing, so a second run does
	// not set the scanner off again.
	if _, err := m.RunAction("recoverer", "recover", f); err != nil {
		t.Fatal(err)
	}
	if n := deps.Files.Len(); n != 2 {
		t.Fatalf("%d files after a second recovery", n)
	}
	if n := deps.Findings.Len(); n != 1 {
		t.Fatalf("%d findings after a second recovery", n)
	}
}

// Entity created and updated hooks only run for entities in a target,
// unless the dispatcher is told to send them all; deleted hooks always run.
func TestEntityHooksAreGatedToTargets(t *testing.T) {
	man := `{"name": "gate", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "entity.on_created", "function": "seen"}, {"hook": "entity.on_updated", "function": "seen"},
			{"hook": "entity.on_deleted", "function": "seen"}]}`
	deps, m := env(t, pluginFS("gate", man, `export function seen(e: any) { console.log(e.hook, e.entity.key) }`))
	if _, err := deps.Targets.Create(target.KindDomain, "app.test"); err != nil {
		t.Fatal(err)
	}
	deps.Entities.SetScope(func(host string) (string, bool) {
		mt, ok := deps.Targets.Set().MatchHost(host)
		return mt.ID, ok
	})
	m.Rescan()
	m.Apply(Config{})
	discover := func(host string) {
		t.Helper()
		if _, _, err := deps.Entities.Discover(entities.Discovery{
			Address: entities.AddressOf("https", host, "/x"), Link: entities.Link{Source: entities.OriginLink},
		}); err != nil {
			t.Fatal(err)
		}
	}
	log := func() string { info, _ := m.Get("gate"); return logText(info.Log) }

	discover("app.test")
	discover("cdn.other.test") // related: no target covers it
	eventually(t, "the target's URL", func() bool {
		return strings.Contains(log(), "entity.on_created https://app.test/x")
	})
	if strings.Contains(log(), "other.test") {
		t.Fatalf("an entity outside the targets reached the plugin:\n%s", log())
	}

	// Deleting is always heard.
	if err := deps.Entities.Delete("https://cdn.other.test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the deletion", func() bool {
		return strings.Contains(log(), "entity.on_deleted https://cdn.other.test")
	})

	// Every entity, once the setting is on.
	m.Dispatcher().SetAllEntities(true)
	discover("cdn.other.test")
	eventually(t, "the related URL", func() bool {
		return strings.Contains(log(), "entity.on_created https://cdn.other.test/x")
	})
}

// The match filter's types pick entity types on entity hooks, and only
// those.
func TestEntityTypesFilter(t *testing.T) {
	for _, c := range []struct {
		types []string
		ok    bool
	}{{[]string{"domain", "url"}, true}, {[]string{"endpoint"}, false}} {
		info, _ := manifest.Lookup(manifest.EntityOnCreated)
		_, err := (&manifest.Match{Types: c.types}).Compile(info)
		if (err == nil) != c.ok {
			t.Errorf("types %v: err = %v", c.types, err)
		}
	}
}

func TestProblemsAreReportedNotRun(t *testing.T) {
	bad := `{"name": "broken", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "traffic.on_flow_complete", "function": "missing"}]}`
	_, m := env(t, pluginFS("broken", bad, `export function other() {}`))
	m.Rescan()
	m.Apply(Config{})
	info, _ := m.Get("broken")
	if info.State != StateError || !strings.Contains(strings.Join(info.Problems, ";"), "doesn't export missing") {
		t.Fatalf("info = %+v", info)
	}
}

func TestAFailingPluginIsFaulted(t *testing.T) {
	man := `{"name": "thrower", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "traffic.on_flow_complete", "function": "boom"}, {"hook": "plugin.on_status_change", "function": "status"}]}`
	deps, m := env(t, pluginFS("thrower", man, `
		export function boom() { throw new Error("boom") }
		export function status(e: any) { console.log("status", e.to, e.reason) }`))
	m.Rescan()
	m.Apply(Config{})
	for i := 0; i < faultLimit; i++ {
		addFlow(deps.Store, "https://app.test/x", "text/plain", "x")
	}
	eventually(t, "the fault", func() bool { info, _ := m.Get("thrower"); return info.State == StateFaulted })
	info, _ := m.Get("thrower")
	if info.Stats.Errors < faultLimit || !strings.Contains(logText(info.Log), "status faulted fault") {
		t.Fatalf("stats %+v, log %s", info.Stats, logText(info.Log))
	}
	if m.disp.routes.Load() != nil {
		t.Error("a faulted plugin still routed")
	}
	// Reload tries again.
	if err := m.Reload("thrower"); err != nil {
		t.Fatal(err)
	}
	if info, _ := m.Get("thrower"); info.State != StateRunning {
		t.Errorf("after reload: %s", info.State)
	}
}

func TestOwnTrafficIsSkippedUnlessSelf(t *testing.T) {
	man := `{"name": "echo", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "traffic.on_flow_complete", "function": "seen", "match": {"sources": ["proxy", "plugin"]}}]}`
	deps, m := env(t, pluginFS("echo", man, `export function seen(e: any) { console.log("saw", e.flow.origin || "browser") }`))
	m.Rescan()
	m.Apply(Config{})
	own := deps.Store.Add(capture.Flow{Method: "GET", Host: "a", Path: "/", Source: capture.SourcePlugin, Origin: "echo"})
	deps.Store.Complete(own, capture.Message{}, 200, "200 OK", time.Now())
	other := deps.Store.Add(capture.Flow{Method: "GET", Host: "a", Path: "/", Source: capture.SourcePlugin, Origin: "other#2"})
	deps.Store.Complete(other, capture.Message{}, 200, "200 OK", time.Now())
	eventually(t, "the other plugin's flow", func() bool { info, _ := m.Get("echo"); return info.Stats.Delivered == 1 })
	time.Sleep(50 * time.Millisecond)
	info, _ := m.Get("echo")
	if log := logText(info.Log); strings.Contains(log, "saw echo") || !strings.Contains(log, "saw other#2") {
		t.Fatalf("log = %s", log)
	}
}

func TestInterceptStage(t *testing.T) {
	man := `{"name": "stamp", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "intercept.on_request_pending", "function": "stamp", "match": {"paths": ["/api/*"]}}]}`
	deps, m := env(t, pluginFS("stamp", man, `
		import { decision, edit, type PendingEvent } from "reproxy";
		export function stamp(e: PendingEvent) {
			if (e.flow.path === "/api/drop") return decision.drop();
			return decision.edit(edit.setHeader(e.message.editable(), "X-Plugin", "stamped"));
		}`))
	m.Rescan()
	m.Apply(Config{AllowRelease: true})
	ic := deps.Intercept
	ic.SetEnabled(true)

	hold := func(path string) (intercept.Decision, error) {
		raw := []byte("GET " + path + " HTTP/1.1\r\nHost: app.test\r\n\r\n")
		id := deps.Store.Add(capture.Flow{State: capture.StateHeldRequest, Method: "GET", Host: "app.test", Path: path,
			Request: capture.Message{Raw: raw}})
		go func() {
			eventually(t, "the hold", func() bool { _, ok := ic.IsHeld(id); return ok })
			ic.Forward(id, nil)
		}()
		return ic.Hold(context.Background(), id, intercept.Request, raw)
	}
	d, err := hold("/api/users")
	if err != nil || !d.Edited || !strings.Contains(string(d.Raw), "X-Plugin: stamped") {
		t.Fatalf("decision = %+v %q, %v", d, d.Raw, err)
	}
	if d, _ := hold("/api/drop"); d.Verdict != intercept.Drop {
		t.Fatalf("drop decision = %+v", d)
	}
	if d, _ := hold("/static/x"); d.Edited {
		t.Fatal("a message the filter turns away was edited")
	}
	// Releasing not allowed: the drop becomes a pass, and the user decides.
	m.Apply(Config{AllowRelease: false})
	if d, _ := hold("/api/drop"); d.Verdict != intercept.Forward {
		t.Fatalf("with release off = %+v", d)
	}
	// Disabled: the stage is gone.
	m.Apply(Config{Overrides: map[string]Override{"stamp": {Status: manifest.Disabled}}})
	if m.disp.wants(manifest.InterceptOnRequestPending) {
		t.Fatal("stage still wanted")
	}
}

func TestSendThroughProxyIsLabelledAndBounded(t *testing.T) {
	man := `{"name": "sender", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "traffic.on_flow_complete", "function": "go", "self": true, "match": {"sources": ["proxy", "plugin"]}}]}`
	deps, m := env(t, pluginFS("sender", man, `
		import { http, log } from "reproxy";
		export async function go(e: any) {
			try {
				const r = await http.send({ url: "https://app.test/again" }, { throughProxy: true });
				log.info("sent", r.status);
			} catch (err) { log.warn(String(err)); }
		}`))
	labels := make(chan string, 10)
	deps.Send = func(ctx context.Context, r replay.Request) (replay.Response, error) {
		if r.Dial != nil {
			r.Dial(ctx, "tcp", r.Addr)
		}
		raw := []byte("HTTP/1.1 204 No Content\r\n\r\n")
		return replay.Response{Raw: raw, StatusCode: 204, Status: "204 No Content"}, nil
	}
	deps.ProxyDial = func(_ context.Context, _, _, label string) (net.Conn, error) {
		labels <- label
		return nil, nil
	}
	m.Rescan()
	m.Apply(Config{})
	// A flow from the plugin itself at the limit: the next send is refused.
	id := deps.Store.Add(capture.Flow{Method: "GET", Host: "app.test", Path: "/", Source: capture.SourcePlugin, Origin: "sender#3"})
	deps.Store.Complete(id, capture.Message{}, 200, "200 OK", time.Now())
	// An ordinary flow: sent at depth 1.
	id = deps.Store.Add(capture.Flow{Method: "GET", Host: "app.test", Path: "/"})
	deps.Store.Complete(id, capture.Message{}, 200, "200 OK", time.Now())
	select {
	case l := <-labels:
		if l != "reproxy-plugin/sender/1" {
			t.Fatalf("label = %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never dialled")
	}
	eventually(t, "the refusal", func() bool {
		info, _ := m.Get("sender")
		return strings.Contains(logText(info.Log), "E_RECURSION")
	})
	if origin, ok := ParseLabel("reproxy-plugin/sender/2"); !ok || origin != "sender#2" {
		t.Errorf("ParseLabel = %q %v", origin, ok)
	}
}

func BenchmarkDispatchNoPlugins(b *testing.B) {
	d := newDispatcher(func() bool { return true })
	e := capture.Event{Kind: capture.FlowUpdated, Prev: capture.StatePending,
		Flow: capture.Flow{State: capture.StateComplete, Method: "GET", Host: "a", Path: "/"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d.OnFlow(e)
	}
}

func BenchmarkDispatchUnmatched(b *testing.B) {
	info, _ := manifest.Lookup(manifest.TrafficOnFlowComplete)
	m, _ := (&manifest.Match{Hosts: []string{"*.other.test"}, ContentTypes: []string{"application/json"}}).Compile(info)
	d := newDispatcher(func() bool { return true })
	rt := &routing{mask: bit(manifest.TrafficOnFlowComplete)}
	rt.byHook[hookIndex[manifest.TrafficOnFlowComplete]] = []route{{p: &Running{src: Source{Name: "x"}}, fn: "f", m: m}}
	d.routes.Store(rt)
	e := capture.Event{Kind: capture.FlowUpdated, Prev: capture.StatePending,
		Flow: capture.Flow{State: capture.StateComplete, Method: "GET", Host: "app.test", Path: "/",
			Response: capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n")}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d.OnFlow(e)
	}
}

// A target with hold: true has core hold what its filter accepts even with
// interception off, and only for the plugins: the user never sees it.
func TestHoldForPlugins(t *testing.T) {
	man := `{"name": "rewriter", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "intercept.on_response_pending", "function": "rewrite", "hold": true,
			"match": {"paths": ["/api/*"], "content_types": ["application/json"]}}]}`
	deps, m := env(t, pluginFS("rewriter", man, `
		import { decision, edit, type PendingEvent } from "reproxy";
		export function rewrite(e: PendingEvent) {
			if (e.flow.path === "/api/secret") return decision.drop();
			return decision.edit(edit.setBody(e.message.editable(), '{"admin":true}'));
		}`))
	m.Rescan()
	m.Apply(Config{AllowHold: true, AllowRelease: false})
	ic := deps.Intercept // interception stays off

	subject := func(path, ct string) intercept.Subject {
		return intercept.Subject{Host: "app.test", Path: path, ContentType: ct, Method: "GET", Status: 200}
	}
	if k := ic.Holds(intercept.Response, subject("/api/me", "application/json")); k != intercept.HoldPlugin {
		t.Fatalf("hold kind = %v", k)
	}
	if k := ic.Holds(intercept.Response, subject("/static/a.js", "application/json")); k != 0 {
		t.Fatalf("unmatched hold kind = %v", k)
	}
	if k := ic.Holds(intercept.Request, subject("/api/me", "application/json")); k != 0 {
		t.Fatalf("request hold kind = %v", k)
	}
	own := subject("/api/me", "application/json")
	own.Origin = "rewriter"
	if k := ic.Holds(intercept.Response, own); k != 0 {
		t.Fatal("a plugin held its own traffic")
	}

	resp := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	hold := func(path string) intercept.Decision {
		id := deps.Store.Add(capture.Flow{State: capture.StateHeldResponse, Method: "GET", Host: "app.test", Path: path})
		d, err := ic.HoldAs(context.Background(), id, intercept.Response, resp, intercept.HoldPlugin)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := hold("/api/me")
	if d.Verdict != intercept.Forward || !d.Edited || !strings.HasSuffix(string(d.Raw), `{"admin":true}`) ||
		!strings.Contains(string(d.Raw), "Content-Length: 14") {
		t.Fatalf("decision = %+v %q", d, d.Raw)
	}
	// A plugin-only hold may be dropped even with release off for the user's.
	if d := hold("/api/secret"); d.Verdict != intercept.Drop {
		t.Fatalf("drop = %+v", d)
	}
	if len(ic.Held()) != 0 {
		t.Fatal("a plugin hold reached the user's queue")
	}

	// The user's switch turns holding off.
	m.Apply(Config{AllowHold: false})
	if k := ic.Holds(intercept.Response, subject("/api/me", "application/json")); k != 0 {
		t.Fatalf("hold kind with holding disallowed = %v", k)
	}
}

// A plugin observes WebSocket messages, and with hold:true edits them.
func TestWebSocketObserveAndEdit(t *testing.T) {
	man := `{"name": "ws", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [
			{"hook": "websocket.on_message", "function": "saw", "match": {"directions": ["received"]}},
			{"hook": "websocket.on_message_pending", "function": "edit", "hold": true, "match": {"directions": ["sent"]}}
		]}`
	deps, m := env(t, pluginFS("ws", man, `
		import { log, decision, type Hooks, type WebSocketPendingEvent } from "reproxy";
		export function saw(e: Hooks["websocket.on_message"]) { log.info("saw", e.message.direction, e.message.text()); }
		export function edit(e: WebSocketPendingEvent) { return decision.edit(e.message.text().toUpperCase()); }`))
	deps.Store.Subscribe(m.Dispatcher().OnWSMessage)
	m.Rescan()
	m.Apply(Config{AllowHold: true})

	// A WebSocket flow.
	id := deps.Store.Add(capture.Flow{Method: "GET", Host: "app.test", Path: "/ws", Secure: true, State: capture.StateComplete, StatusCode: 101})

	// Edits is true for this connection (a hold ws target matches).
	if !m.WSEdits(intercept.Subject{Host: "app.test", Path: "/ws"}) {
		t.Fatal("WSEdits should be true")
	}
	// A sent message is edited.
	d := m.WSDecide(context.Background(), id, wsDir("sent"), wsType("text"), []byte("hello"))
	if string(d.Data) != "HELLO" {
		t.Fatalf("decide = %+v", d)
	}
	// A received message isn't held by the sent-only target.
	if d := m.WSDecide(context.Background(), id, wsDir("received"), wsType("text"), []byte("x")); d.Data != nil || d.Drop {
		t.Fatalf("received edited = %+v", d)
	}

	// Observe: recording a received message reaches the on_message hook.
	deps.Store.AddWSMessage(id, capture.WSMessage{Dir: capture.Received, Type: capture.WSText, Data: []byte("world")})
	deps.Store.AddWSMessage(id, capture.WSMessage{Dir: capture.Sent, Type: capture.WSText, Data: []byte("nope")})
	eventually(t, "the received message", func() bool {
		info, _ := m.Get("ws")
		return strings.Contains(logText(info.Log), "saw received world")
	})
	if info, _ := m.Get("ws"); strings.Contains(logText(info.Log), "saw sent nope") {
		t.Fatal("the direction filter let a sent message through to a received-only hook")
	}
}

type websocketDir = websocket.Direction
type websocketType = websocket.Type

func wsDir(s string) websocketDir {
	if s == "received" {
		return 1
	}
	return 0
}
func wsType(s string) websocketType {
	if s == "binary" {
		return 1
	}
	return 0
}
