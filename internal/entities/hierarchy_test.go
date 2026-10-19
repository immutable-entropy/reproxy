package entities

import (
	"errors"
	"reflect"
	"testing"
)

func TestRegistrableDomain(t *testing.T) {
	for host, want := range map[string]string{
		"api.example.com": "example.com", "Example.COM:8443": "example.com", "a.b.example.co.uk": "example.co.uk",
		"app.test": "app.test", "localhost": "", "10.0.0.5": "", "[::1]:80": "", "co.uk": "", "": "",
	} {
		if got := RegistrableDomain(host); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", host, got, want)
		}
	}
}

// A request puts its URL in the store with the origin, name host and
// domain it sits under, each with its own attributes, and counts the hit
// on all of them.
func TestURLsBringTheirHostAndDomain(t *testing.T) {
	s := NewStore()
	f := flow(1, "GET", "api.example.com:8443", "/users/7", 200, "Server: nginx\r\n", "")
	evs, err := s.Apply(visit(t, f))
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, e := range evs {
		if e.Kind == Created {
			created = append(created, string(e.Entity.Type)+" "+e.Entity.Key)
		}
	}
	if want := []string{"domain example.com", "host //api.example.com", "host https://api.example.com:8443", "url https://api.example.com:8443/users/{id}"}; !reflect.DeepEqual(created, want) {
		t.Fatalf("created %v, want %v (parents first)", created, want)
	}

	d, _ := s.Get("example.com")
	if d.Type != TypeDomain || d.Parent != "" || d.DomainAttrs.Suffix != "com" || !d.DomainAttrs.ICANN || d.Hits != 1 || d.Label() != "example.com" {
		t.Errorf("domain = %+v %+v", d, d.DomainAttrs)
	}
	name, _ := s.Get("//api.example.com")
	if !name.IsName() || name.Parent != "example.com" || name.Label() != "api.example.com" || name.Hits != 1 || name.HostAttrs.Port != 0 {
		t.Errorf("name host = %+v %+v", name, name.HostAttrs)
	}
	h, _ := s.Get("https://api.example.com:8443")
	if h.Type != TypeHost || h.Parent != "//api.example.com" || h.HostAttrs.Port != 8443 || h.HostAttrs.IP ||
		!reflect.DeepEqual(h.HostAttrs.Servers, []string{"nginx"}) || h.Hits != 1 || !h.Visited() {
		t.Errorf("host = %+v %+v", h, h.HostAttrs)
	}
	u, _ := s.Get("https://api.example.com:8443/users/{id}")
	if u.Type != TypeURL || u.Parent != "https://api.example.com:8443" || u.Label() != u.Key {
		t.Errorf("url = %+v", u)
	}
	if kids := s.Children("example.com"); len(kids) != 1 || kids[0].Key != name.Key {
		t.Errorf("children of the domain = %+v", kids)
	}
}

// An origin whose name has no registrable domain sits under a name host
// that is a root.
func TestAddressesGroupTheirOrigins(t *testing.T) {
	s := NewStore()
	s.Apply(visit(t, flow(1, "GET", "10.0.0.5", "/", 200, "", "")))
	s.Apply(visit(t, flow(2, "GET", "localhost:3000", "/", 200, "", "")))
	for key, parent := range map[string]string{
		"//10.0.0.5": "", "https://10.0.0.5": "//10.0.0.5", "https://10.0.0.5/": "https://10.0.0.5",
		"//localhost": "", "https://localhost:3000": "//localhost",
	} {
		e, ok := s.Get(key)
		if !ok || e.Parent != parent || e.Hits != 1 {
			t.Errorf("%s: %+v (ok %v), want parent %q and one hit", key, e, ok, parent)
		}
	}
	if a, _ := s.Get("//10.0.0.5"); a.Type != TypeHost || a.Scheme != "" || !a.HostAttrs.IP || a.HostAttrs.Port != 0 {
		t.Errorf("address-only host = %+v %+v", a, a.HostAttrs)
	}
}

// A discovered URL brings a discovered host and domain: nothing visited.
func TestDiscoveredParentsAreNotVisited(t *testing.T) {
	s := NewStore()
	s.Discover(Discovery{Address: AddressOf("https", "cdn.example.net", "/a.js"), Link: Link{Source: OriginLink}})
	for _, key := range []string{"example.net", "//cdn.example.net", "https://cdn.example.net", "https://cdn.example.net/a.js"} {
		if e, ok := s.Get(key); !ok || e.Visited() || e.Origin != OriginLink {
			t.Errorf("%s = %+v", key, e)
		}
	}
	s.Apply(visit(t, flow(1, "GET", "cdn.example.net", "/b.js", 200, "", "")))
	if d, _ := s.Get("example.net"); !d.Visited() {
		t.Error("a request under the domain left it unvisited")
	}
	if a, _ := s.Get("https://cdn.example.net/a.js"); a.Visited() {
		t.Error("a request to a sibling visited the URL")
	}
}

func TestKeyValueStore(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	s.Use(db)
	s.Apply(visit(t, flow(1, "GET", "app.test", "/a", 200, "", "")))
	var updates int
	s.Subscribe(func(e Event) {
		if e.Kind == Updated && e.Entity.Key == "https://app.test" {
			updates++
		}
	})
	if _, err := s.SetKV("https://app.test", "waf", "cloudflare"); err != nil {
		t.Fatal(err)
	}
	s.SetKV("https://app.test", "owner", "team-a")
	s.SetKV("https://app.test", "waf", "akamai")
	if _, err := s.SetKV("https://app.test", "", "x"); err == nil {
		t.Error("a pair with no key was set")
	}
	if _, err := s.SetKV("https://nowhere.test", "a", "b"); err == nil {
		t.Error("a pair was set on a missing entity")
	}
	again := NewStore()
	again.Use(db)
	h, _ := again.Get("https://app.test")
	if want := []Pair{{"waf", "akamai"}, {"owner", "team-a"}}; !reflect.DeepEqual(h.KV, want) {
		t.Errorf("kv after reopening = %+v, want %+v", h.KV, want)
	}
	if v, ok := h.Get("owner"); !ok || v != "team-a" {
		t.Errorf("Get(owner) = %q, %v", v, ok)
	}
	if updates != 3 {
		t.Errorf("%d updates, want 3", updates)
	}
	s.DeleteKV("https://app.test", "waf")
	e, _ := s.ReplaceKV("https://app.test", []Pair{{"a", "1"}, {"", "dropped"}, {"b", "2"}, {"a", "3"}})
	if want := []Pair{{"a", "3"}, {"b", "2"}}; !reflect.DeepEqual(e.KV, want) {
		t.Errorf("replaced kv = %+v, want %+v", e.KV, want)
	}
}

// Deleting an entity takes everything under it, and its hits off what it
// sat under.
func TestDeleteTakesTheSubtree(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	s.Use(db)
	s.Apply(visit(t, flow(1, "GET", "a.example.com", "/x", 200, "", "")))
	s.Apply(visit(t, flow(2, "GET", "a.example.com", "/y", 200, "", "")))
	s.Apply(visit(t, flow(3, "GET", "b.example.com", "/z", 200, "", "")))
	var got []string
	s.Subscribe(func(e Event) {
		got = append(got, map[EventKind]string{Deleted: "-", Updated: "~"}[e.Kind]+e.Entity.Key)
	})
	if err := s.Delete("https://a.example.com"); err != nil {
		t.Fatal(err)
	}
	// The name host stays: it is a host in its own right, with nothing
	// requested under it any more.
	want := []string{"-https://a.example.com/y", "-https://a.example.com/x", "-https://a.example.com", "~//a.example.com", "~example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	if d, _ := s.Get("example.com"); d.Hits != 1 {
		t.Errorf("domain hits = %d, want 1", d.Hits)
	}
	if n := rows(t, db, `SELECT count(*) FROM entities`); n != 5 {
		t.Errorf("%d rows left, want 5", n)
	}
	if n := rows(t, db, `SELECT count(*) FROM entity_flows`); n != 1 {
		t.Errorf("%d sightings left, want 1", n)
	}
	if err := s.Delete("https://a.example.com"); err == nil {
		t.Error("deleting a missing entity succeeded")
	}
}

// A target's domain or address is in the store, drawn in the Explorer, and
// let go when the target goes as long as nothing came of it.
func TestTargetsSeedEntities(t *testing.T) {
	db := openDB(t, t.TempDir())
	s := NewStore()
	s.Use(db)
	s.SetScope(scopeOf("example.com", "10.0.0.5"))
	var got []string
	s.Subscribe(func(e Event) {
		if e.Kind == Created || e.Kind == Deleted {
			got = append(got, map[EventKind]string{Created: "+", Deleted: "-"}[e.Kind]+e.Entity.Key)
		}
	})
	seeds := []Seed{{TypeDomain, "example.com", "t:example.com"}, {TypeHost, "10.0.0.5", "t:10.0.0.5"}}
	if err := s.SyncTargets(seeds); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sortedCopy(got), []string{"+//10.0.0.5", "+example.com"}) {
		t.Errorf("events %v", got)
	}
	d, _ := s.Get("example.com")
	if d.Origin != OriginTarget || d.Visited() || d.TargetID != "t:example.com" || !s.Seeded("example.com") {
		t.Errorf("seeded domain = %+v", d)
	}
	var drawn []string
	for _, e := range s.Mapped("") {
		drawn = append(drawn, e.Key+"@"+e.TargetID)
	}
	if !reflect.DeepEqual(sortedCopy(drawn), []string{"//10.0.0.5@t:10.0.0.5", "example.com@t:example.com"}) {
		t.Errorf("drawn %v", drawn)
	}

	// Syncing again changes nothing; traffic under the address keeps it
	// when its target goes, the idle domain is let go.
	got = nil
	s.SyncTargets(seeds)
	if len(got) != 0 {
		t.Errorf("a repeated sync emitted %v", got)
	}
	s.Apply(visit(t, flow(1, "GET", "10.0.0.5", "/", 200, "", "")))
	if a, _ := s.Get("https://10.0.0.5"); a.Parent != "//10.0.0.5" {
		t.Errorf("origin on a seeded address sits under %q", a.Parent)
	}
	got = nil
	s.SyncTargets(nil)
	if !reflect.DeepEqual(got, []string{"-example.com"}) {
		t.Errorf("events after the targets went: %v", got)
	}

	// Another engagement gets the seeds put in as it opens.
	s.SyncTargets(seeds)
	other := NewStore()
	other.SyncTargets(seeds)
	if err := other.Use(openDB(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Get("example.com"); !ok {
		t.Error("seed missing after opening another engagement")
	}
	// Clear puts them straight back.
	s.Clear()
	if _, ok := s.Get("example.com"); !ok {
		t.Error("seed missing after Clear")
	}
}

// An archive that kept only URLs gets their hosts and domains when it is
// opened, with the hits below each.
func TestOpeningAnOldArchiveCompletesTheHierarchy(t *testing.T) {
	db := openDB(t, t.TempDir())
	if _, err := db.Exec(`INSERT INTO entities (key, type, parent, scheme, host, path, target_id, visited, origin, attrs, kv, hits, first_seen, last_seen)
		VALUES ('https://api.example.com/a', 'url', '', 'https', 'api.example.com', '/a', 't1', 1, 'traffic', '{}', '[]', 4, 1, 5),
		       ('https://api.example.com/b', 'url', '', 'https', 'api.example.com', '/b', 't1', 0, 'link', '{}', '[]', 0, 2, 2)`); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	var news int
	s.Subscribe(func(e Event) {
		if e.Kind != Reset {
			news++
		}
	})
	if err := s.Use(db); err != nil {
		t.Fatal(err)
	}
	if news != 0 {
		t.Errorf("completing the hierarchy emitted %d events", news)
	}
	for key, hits := range map[string]int{"example.com": 4, "//api.example.com": 4, "https://api.example.com": 4} {
		e, ok := s.Get(key)
		if !ok || e.Hits != hits || e.Origin != OriginTraffic {
			t.Errorf("%s = %+v, want %d hits", key, e, hits)
		}
	}
	if u, _ := s.Get("https://api.example.com/a"); u.Parent != "https://api.example.com" {
		t.Errorf("URL parent = %q", u.Parent)
	}
	if n := rows(t, db, `SELECT count(*) FROM entities`); n != 5 {
		t.Errorf("%d rows, want 5", n)
	}
}

// An origin an older archive kept directly under its domain is moved under
// a name host for it when the archive is opened.
func TestOpeningAnOldArchiveMovesOriginsUnderTheirName(t *testing.T) {
	db := openDB(t, t.TempDir())
	if _, err := db.Exec(`INSERT INTO entities (key, type, parent, scheme, host, path, target_id, visited, origin, attrs, kv, hits, first_seen, last_seen)
		VALUES ('example.com', 'domain', '', '', 'example.com', '', 't1', 1, 'traffic', '{}', '[]', 3, 1, 5),
		       ('https://api.example.com', 'host', 'example.com', 'https', 'api.example.com', '', 't1', 1, 'traffic', '{"port":443}', '[]', 3, 1, 5),
		       ('https://api.example.com/a', 'url', 'https://api.example.com', 'https', 'api.example.com', '/a', 't1', 1, 'traffic', '{}', '[]', 3, 1, 5)`); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	if err := s.Use(db); err != nil {
		t.Fatal(err)
	}
	name, ok := s.Get("//api.example.com")
	if !ok || name.Parent != "example.com" || name.Hits != 3 {
		t.Errorf("name host = %+v", name)
	}
	if o, _ := s.Get("https://api.example.com"); o.Parent != "//api.example.com" {
		t.Errorf("origin parent = %q", o.Parent)
	}
	var parent string
	if err := db.QueryRow(`SELECT parent FROM entities WHERE key = 'https://api.example.com'`).Scan(&parent); err != nil || parent != "//api.example.com" {
		t.Errorf("stored parent = %q, %v", parent, err)
	}
}

// A host found without a URL is a name host of its own, with the
// addresses it resolves to as hosts too; out of scope, they are Related.
func TestDiscoverHost(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("example.com"))
	link := Link{Source: PluginOrigin("dns"), FromURL: "https://example.com/"}
	e, created, err := s.DiscoverHost(HostDiscovery{Name: "Shop.Example.com.", Link: link, Addresses: []string{"192.0.2.20", "2001:DB8::1"}})
	if err != nil || !created {
		t.Fatalf("discover: %v, created %v", err, created)
	}
	if e.Key != "//shop.example.com" || e.Parent != "example.com" || e.Label() != "shop.example.com" ||
		!reflect.DeepEqual(e.HostAttrs.Addresses, []string{"192.0.2.20", "2001:db8::1"}) || len(e.Links) != 1 {
		t.Errorf("name host = %+v %+v", e, e.HostAttrs)
	}
	ip, ok := s.Get("//192.0.2.20")
	if !ok || !ip.HostAttrs.IP || ip.Parent != "" || len(ip.Links) != 1 {
		t.Errorf("address = %+v", ip)
	}
	var related []string
	for _, r := range s.Related() {
		related = append(related, r.Host)
	}
	if want := []string{"192.0.2.20", "2001:db8::1", "shop.example.com"}; !reflect.DeepEqual(sortedCopy(related), want) {
		t.Errorf("related = %v, want %v", related, want)
	}

	// Again: nothing new, nothing announced.
	var events int
	s.Subscribe(func(Event) { events++ })
	if _, created, _ := s.DiscoverHost(HostDiscovery{Name: "shop.example.com", Link: link, Addresses: []string{"192.0.2.20"}}); created || events != 0 {
		t.Errorf("rediscovery: created %v, %d events", created, events)
	}
	for _, bad := range []string{"", "https://x.test", "x.test:443", "a b.test", "-x.test"} {
		if _, _, err := s.DiscoverHost(HostDiscovery{Name: bad}); !errors.Is(err, ErrBadHost) {
			t.Errorf("DiscoverHost(%q) = %v", bad, err)
		}
	}
	if _, _, err := s.DiscoverHost(HostDiscovery{Name: "x.test", Addresses: []string{"nope"}}); !errors.Is(err, ErrBadHost) {
		t.Errorf("a bad address was taken: %v", err)
	}

	// A host a target covers is drawn even with nothing under it.
	s.SetScope(scopeOf("example.com", "shop.example.com"))
	if _, ok := find(s.Mapped(""), "//shop.example.com"); !ok {
		t.Error("a covered host found without a URL isn't drawn")
	}
}

// Each lifecycle event says whether its entity belongs to a target: a
// target covers its host, or it is what a target stands for.
func TestEventsSayWhetherTheEntityIsInATarget(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("api.example.com"))
	got := map[string]bool{}
	s.Subscribe(func(e Event) {
		if e.Kind == Created || e.Kind == Updated {
			got[e.Entity.Key] = e.InTarget
		}
	})
	if err := s.SyncTargets([]Seed{{TypeDomain, "example.com", "t:example.com"}}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.example.com", "cdn.other.test"} {
		if _, _, err := s.Discover(Discovery{Address: AddressOf("https", host, "/x"), Link: Link{Source: OriginLink}}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{
		"example.com": true, "//api.example.com": true, "https://api.example.com": true, "https://api.example.com/x": true,
		"other.test": false, "//cdn.other.test": false, "https://cdn.other.test": false, "https://cdn.other.test/x": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InTarget = %v, want %v", got, want)
	}
}
