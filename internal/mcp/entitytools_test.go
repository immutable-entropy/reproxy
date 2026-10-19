package mcp

import (
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
)

// seedURL records a visited URL in the fixture's Explorer from a flow.
func seedURL(t *testing.T, f *fixture, method, rawURL string, status int) uint64 {
	t.Helper()
	host := rawURL[len("https://"):]
	path := "/"
	if i := indexByte(host, '/'); i >= 0 {
		host, path = host[:i], host[i:]
	}
	id := f.store.Add(capture.Flow{
		Method: method, Host: host, Path: path, Secure: true, State: capture.StateComplete,
		StatusCode: status, StatusText: "ok",
		Request:  capture.Message{Raw: []byte(method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n")},
		Response: capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n")},
		Started:  time.Now(),
	})
	v, _, ok := entities.Parse(capture.Flow{ID: id, Method: method, Host: host, Path: path, Secure: true, StatusCode: status})
	if !ok {
		t.Fatal("parse failed")
	}
	f.entities.Apply(entities.Batch{Visits: []entities.Visit{v}})
	return id
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func TestEntityTools(t *testing.T) {
	f := newFixture(t)
	seedURL(t, f, "GET", "https://app.test/users/12", 200)
	seedURL(t, f, "GET", "https://app.test/users/13", 200)
	seedURL(t, f, "GET", "https://app.test/health", 200)

	var list struct {
		Entities []entityOut `json:"entities"`
		Matched  int         `json:"matched"`
	}
	if err := f.call(t, auto, "list_entities", map[string]any{"host": "app.test", "type": "url"}, &list); err != nil {
		t.Fatal(err)
	}
	if list.Matched != 2 {
		t.Fatalf("URLs = %d (want /users/{id} and /health)", list.Matched)
	}
	if err := f.call(t, auto, "list_entities", map[string]any{"type": "domain"}, &list); err != nil {
		t.Fatal(err)
	}
	if list.Matched != 1 || list.Entities[0].Key != "app.test" || list.Entities[0].Hits != 3 || list.Entities[0].Suffix != "test" {
		t.Fatalf("domains = %+v", list.Entities)
	}
	if err := f.call(t, auto, "list_entities", map[string]any{"type": "endpoint"}, &list); err == nil {
		t.Error("an unknown type was accepted")
	}

	var got struct {
		Entity entityOut `json:"entity"`
	}
	if err := f.call(t, auto, "get_entity", map[string]any{"url": "https://app.test/users/999"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Entity.Key != "https://app.test/users/{id}" || got.Entity.Hits != 2 || len(got.Entity.Flows) != 2 ||
		got.Entity.Parent != "https://app.test" || got.Entity.Type != "url" {
		t.Fatalf("URL = %+v", got.Entity)
	}
	if err := f.call(t, auto, "get_entity", map[string]any{"key": "https://app.test"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Entity.Type != "host" || got.Entity.Port != 443 || len(got.Entity.Children) != 2 {
		t.Fatalf("host = %+v", got.Entity)
	}

	// list_flows filtered by an entity returns the requests under it.
	var flows struct {
		Flows []map[string]any `json:"flows"`
	}
	for key, want := range map[string]int{"https://app.test/users/{id}": 2, "app.test": 3} {
		if err := f.call(t, auto, "list_flows", map[string]any{"entity": key}, &flows); err != nil {
			t.Fatal(err)
		}
		if len(flows.Flows) != want {
			t.Errorf("flows under %s = %d, want %d", key, len(flows.Flows), want)
		}
	}

	// add_url puts a discovered URL in the Explorer.
	var added struct {
		Entity  entityOut `json:"entity"`
		Created bool      `json:"created"`
	}
	if err := f.call(t, auto, "add_url", map[string]any{"url": "https://app.test/admin/secret", "detail": "found in a script"}, &added); err != nil {
		t.Fatal(err)
	}
	if !added.Created || added.Entity.Visited || added.Entity.Origin != "agent" {
		t.Fatalf("added = %+v", added)
	}

	// set_entity_kv sets and removes keys.
	if err := f.call(t, auto, "set_entity_kv", map[string]any{"key": "https://app.test", "set": map[string]string{"waf": "none", "auth": "cookie"}}, &got); err != nil {
		t.Fatal(err)
	}
	got.Entity = entityOut{} // decoding into it again would merge the maps
	if err := f.call(t, auto, "set_entity_kv", map[string]any{"key": "https://app.test", "set": map[string]string{"waf": "akamai"}, "unset": []string{"auth"}}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entity.KV) != 1 || got.Entity.KV["waf"] != "akamai" {
		t.Fatalf("kv = %+v", got.Entity.KV)
	}
	if h, _ := f.entities.Get("https://app.test"); len(h.KV) != 1 {
		t.Errorf("stored kv = %+v", h.KV)
	}
}
