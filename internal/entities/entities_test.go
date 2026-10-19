package entities

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
)

// flow builds a finished flow from a raw request and response.
func flow(id uint64, method, host, path string, status int, respHead, respBody string) capture.Flow {
	f := capture.Flow{
		ID: id, State: capture.StateComplete, Method: method, Host: host, Path: path, Secure: true,
		Request: capture.Message{Raw: []byte(method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n")},
		Started: time.Unix(int64(id), 0),
	}
	if status != 0 {
		text := fmt.Sprintf("%d Status", status)
		f.StatusCode, f.StatusText = status, text
		f.Response = capture.Message{Raw: []byte("HTTP/1.1 " + text + "\r\n" + respHead +
			fmt.Sprintf("Content-Length: %d\r\n\r\n", len(respBody)) + respBody)}
	}
	return f
}

// openDB opens an engagement archive and returns its database.
func openDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	a, err := archive.Open(filepath.Join(dir, archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a.DB()
}

func visit(t *testing.T, f capture.Flow) Batch {
	t.Helper()
	v, links, ok := Parse(f)
	if !ok {
		t.Fatalf("flow %d not parsed", f.ID)
	}
	return Batch{Visits: []Visit{v}, Discoveries: links}
}

func rows(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIsIdentifier(t *testing.T) {
	for seg, want := range map[string]bool{
		"42": true, "3f2b8c1e-0a4d-4b7e-9c2a-1d5e6f7a8b9c": true,
		"5f1d7a9b3c2e4d6f8a0b1c2d": true, "eyJhbGciOiJIUzI1NiJ9abc": true,
		"users": false, "v2": false, "deadbeefdeadbeef": false, // hex but no digit
		"main.js": false, "": false,
	} {
		if got := isIdentifier(seg); got != want {
			t.Errorf("isIdentifier(%q) = %v, want %v", seg, got, want)
		}
	}
}

func TestAddressNormalises(t *testing.T) {
	for _, c := range []struct{ scheme, host, path, key string }{
		{"https", "App.Test:443", "/users/12/", "https://app.test/users/{id}"},
		{"http", "app.test:80", "", "http://app.test/"},
		{"http", "app.test:8080", "//a//b", "http://app.test:8080/a/b"},
		{"https", "app.test", "/o/7/i/3f2b8c1e-0a4d-4b7e-9c2a-1d5e6f7a8b9c", "https://app.test/o/{id}/i/{id2}"},
		{"https", "app.test", "/o/{orderId}/i/%7BitemId%7D", "https://app.test/o/{id}/i/{id2}"},
	} {
		if got := AddressOf(c.scheme, c.host, c.path).Key(); got != c.key {
			t.Errorf("AddressOf(%q, %q, %q) = %q, want %q", c.scheme, c.host, c.path, got, c.key)
		}
	}
}

// Requests to /users/12 and /users/13 are one URL, one row, however many
// batches and writers they arrive in.
func TestOneRowPerURL(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	if err := s.Use(db); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Apply(visit(t, flow(uint64(i+1), "GET", "app.test", fmt.Sprintf("/users/%d", i+10), 200, "", ""))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := rows(t, db, `SELECT count(*) FROM entities WHERE type = 'url'`); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if n := rows(t, db, `SELECT count(*) FROM entities`); n != 4 {
		t.Fatalf("entities = %d, want 4: the URL, its origin, its name and its domain", n)
	}
	e, ok := s.Get("https://app.test/users/{id}")
	if !ok || e.Hits != 8 || len(e.URLAttrs.Sightings) != 8 || e.ID == 0 {
		t.Fatalf("URL = %+v", e)
	}
	for _, key := range []string{"https://app.test", "//app.test", "app.test"} {
		if up, _ := s.Get(key); up.Hits != 8 {
			t.Errorf("%s hits = %d, want 8", key, up.Hits)
		}
	}
	if n := rows(t, db, `SELECT count(*) FROM entity_flows`); n != 8 {
		t.Errorf("sightings = %d, want 8", n)
	}

	// A second store on the same file (another process) merges into the
	// same row rather than adding one.
	other := NewStore()
	other.Use(db)
	other.Apply(visit(t, flow(20, "POST", "app.test", "/users/99", 201, "", "")))
	if n := rows(t, db, `SELECT count(*) FROM entities`); n != 4 {
		t.Fatalf("rows after a second writer = %d, want 4", n)
	}
	if e2, _ := other.Get(e.Key); e2.ID != e.ID {
		t.Errorf("second writer's ID %d, want %d", e2.ID, e.ID)
	}
}

// The store is kept in the engagement: reopening it brings back every
// entity with its attributes, sightings and links.
func TestEntitiesSurviveReopening(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	s := NewStore()
	s.Use(db)
	s.Apply(visit(t, flow(1, "GET", "app.test", "/home?tab=1", 200,
		"Content-Type: text/html\r\n", `<a href="/about">a</a>`)))
	if _, created, err := s.Discover(Discovery{
		Address: AddressOf("https", "app.test", "/api/v1/orders/5"),
		Link:    Link{Source: PluginOrigin("mapper"), FromURL: "https://app.test/home", FromFlowID: 1, Detail: "js:fetch"},
	}); err != nil || !created {
		t.Fatalf("discover: created %v, %v", created, err)
	}

	again := NewStore()
	if err := again.Use(db); err != nil {
		t.Fatal(err)
	}
	if again.Len() != 6 {
		t.Fatalf("entities after reopening = %d, want 6", again.Len())
	}
	home, _ := again.Get("https://app.test/home")
	if ops := home.Operations(); len(ops) != 1 || ops[0].Query[0].Name != "tab" || len(home.URLAttrs.Sightings) != 1 ||
		!reflect.DeepEqual(home.Kinds(), []Kind{KindHTML}) {
		t.Errorf("home = %+v", home)
	}
	about, _ := again.Get("https://app.test/about")
	if about.Visited() || about.Origin != OriginLink || !reflect.DeepEqual(about.LinkedFrom(), []string{"https://app.test/home"}) {
		t.Errorf("about = %+v", about)
	}
	orders, _ := again.Get("https://app.test/api/v1/orders/{id}")
	if orders.Origin != "plugin:mapper" || len(orders.Links) != 1 || orders.Links[0].Detail != "js:fetch" {
		t.Errorf("orders = %+v", orders)
	}
}

// A link already known adds nothing and tells nobody.
func TestRepeatedDiscoveryIsQuiet(t *testing.T) {
	s := NewStore()
	d := Discovery{Address: AddressOf("https", "app.test", "/x"), Link: Link{Source: OriginLink, FromURL: "https://app.test/"}}
	if ev, _ := s.Apply(Batch{Discoveries: []Discovery{d}}); len(ev) != 4 || ev[3].Kind != Created || ev[3].Entity.Type != TypeURL {
		t.Fatalf("first discovery events = %+v", ev)
	}
	if ev, _ := s.Apply(Batch{Discoveries: []Discovery{d}}); len(ev) != 0 {
		t.Fatalf("repeated discovery events = %+v", ev)
	}
	if _, created, _ := s.Discover(d); created {
		t.Error("Discover of a known URL reported it created")
	}
}

func TestSightingsAreCapped(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	s.Use(db)
	for i := 1; i <= maxSightings+10; i++ {
		s.Apply(visit(t, flow(uint64(i), "GET", "app.test", "/a", 200, "", "")))
	}
	e, _ := s.Get("https://app.test/a")
	sg := e.URLAttrs.Sightings
	if e.Hits != maxSightings+10 || len(sg) != maxSightings || sg[0].FlowID != 11 {
		t.Fatalf("hits %d, sightings %d from %d", e.Hits, len(sg), sg[0].FlowID)
	}
	if n := rows(t, db, `SELECT count(*) FROM entity_flows`); n != maxSightings {
		t.Errorf("sighting rows = %d, want %d", n, maxSightings)
	}
}

func TestClearEmptiesTheStore(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	s.Use(db)
	s.Apply(visit(t, flow(1, "GET", "app.test", "/a", 200, "", "")))
	var got []EventKind
	s.Subscribe(func(e Event) { got = append(got, e.Kind) })
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 || rows(t, db, `SELECT count(*) FROM entities`) != 0 || rows(t, db, `SELECT count(*) FROM entity_flows`) != 0 {
		t.Error("store not emptied")
	}
	if !reflect.DeepEqual(got, []EventKind{Deleted, Deleted, Deleted, Deleted, Cleared}) {
		t.Errorf("events = %v", got)
	}
}

// The indexer only queues on the store's goroutine, indexes each finished
// flow once, and ignores the traffic being cleared.
func TestIndexerFollowsTheStore(t *testing.T) {
	s := NewStore()
	x := NewIndexer(s)
	defer x.Close()
	store := capture.NewStore()
	store.Subscribe(x.Record)

	pending := flow(0, "GET", "app.test", "/a", 0, "", "")
	pending.State = capture.StatePending
	id := store.Add(pending)
	x.Flush()
	if s.Len() != 0 {
		t.Fatal("a pending flow was indexed")
	}
	done := flow(id, "GET", "app.test", "/a", 200, "Content-Type: text/html\r\n", `<a href="/b">b</a>`)
	store.Complete(id, done.Response, 200, "200 OK", time.Now())
	store.Update(id, func(f *capture.Flow) {}) // an edit afterwards: not indexed again
	x.Flush()
	a, ok := s.Get("https://app.test/a")
	if !ok || a.Hits != 1 {
		t.Fatalf("a = %+v", a)
	}
	if b, ok := s.Get("https://app.test/b"); !ok || b.Visited() {
		t.Fatalf("b = %+v", b)
	}

	store.Clear()
	x.Flush()
	if s.Len() != 5 {
		t.Errorf("clearing the traffic changed the store: %d entities", s.Len())
	}
}
