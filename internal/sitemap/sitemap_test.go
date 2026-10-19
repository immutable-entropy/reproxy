package sitemap

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"reflect"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
)

// TestHierarchy pins the shape the Explorer draws: a domain at the root,
// the name host under it, the origins under that, and the URLs' paths
// under each origin.
func TestHierarchy(t *testing.T) {
	m := build(
		flow(1, "GET", "app.test", "/", "", "", 200, "Content-Type: text/html\r\n", ""),
		flow(2, "GET", "app.test", "/users/12", "", "", 200, "", ""),
	)
	if got := names(m.Roots); !reflect.DeepEqual(got, []string{"app.test"}) {
		t.Fatalf("roots = %v, want the domain alone", got)
	}
	domain := m.Roots[0]
	if domain.Type != TypeDomain {
		t.Errorf("root type = %q", domain.Type)
	}
	if got := names(domain.Children); !reflect.DeepEqual(got, []string{"app.test"}) {
		t.Fatalf("under the domain = %v, want the name host", got)
	}
	name := domain.Children[0]
	if name.Type != TypeHost || name.Scheme != "" {
		t.Errorf("name host = type %q scheme %q, want a host with no scheme", name.Type, name.Scheme)
	}
	if got := names(name.Children); !reflect.DeepEqual(got, []string{"https://app.test"}) {
		t.Fatalf("under the name host = %v, want the origin", got)
	}
	origin := name.Children[0]
	if origin.Type != TypeHost || origin.Scheme != "https" {
		t.Errorf("origin = type %q scheme %q", origin.Type, origin.Scheme)
	}
	if got := names(origin.Children); !reflect.DeepEqual(got, []string{"/", "users"}) {
		t.Fatalf("under the origin = %v", got)
	}
}

// TestEveryEntityIsFindableByItsKey is the invariant the Explorer relies
// on: it remembers the selected node as an entity key and looks it up with
// Find after each rebuild, so a node reachable in the tree but filed under
// a different key would lose the selection. The site root is the awkward
// one — the store keys the origin "https://host" and the URL at "/"
// "https://host/", a single character apart.
func TestEveryEntityIsFindableByItsKey(t *testing.T) {
	flows := []capture.Flow{
		flow(1, "GET", "app.test", "/", "", "", 200, "Content-Type: text/html\r\n",
			`<a href="/home">h</a><a href="/users/12">u</a>`),
		flow(2, "GET", "app.test", "/home", "", "", 200, "", ""),
		flow(3, "GET", "app.test", "/users/12", "", "", 200, "", ""),
		flow(4, "POST", "app.test", "/api/v1/orders", "", "", 201, "", ""),
	}
	var b entities.Batch
	for _, f := range flows {
		if v, links, ok := entities.Parse(f); ok {
			b.Visits = append(b.Visits, v)
			b.Discoveries = append(b.Discoveries, links...)
		}
	}
	s := entities.NewStore()
	s.SetScope(func(host string) (string, bool) { return "t1", host == "app.test" })
	s.Apply(b)

	list := s.Mapped("")
	m := Build(list)
	if len(list) == 0 {
		t.Fatal("the store produced no entities")
	}
	for _, e := range list {
		n := m.Find(e.Key)
		if n == nil {
			t.Errorf("%s %q is not in the tree", e.Type, e.Key)
			continue
		}
		if n.Entity == nil {
			t.Errorf("%s %q found a node with no entity", e.Type, e.Key)
			continue
		}
		if n.Entity.Key != e.Key {
			t.Errorf("%q found the node for %q", e.Key, n.Entity.Key)
		}
		if n.Type != e.Type {
			t.Errorf("%q node type = %q, want %q", e.Key, n.Type, e.Type)
		}
		if n.Key != e.Key {
			t.Errorf("node for %q has Key %q", e.Key, n.Key)
		}
	}
}

// TestPathPrefixesHaveNoEntity: a segment nothing was requested at is a
// node so the tree keeps its shape, but it is not an entity.
func TestPathPrefixesHaveNoEntity(t *testing.T) {
	m := build(flow(1, "GET", "app.test", "/api/v1/orders", "", "", 200, "", ""))
	for _, key := range []string{"https://app.test/api", "https://app.test/api/v1"} {
		n := m.Find(key)
		if n == nil {
			t.Fatalf("%s missing", key)
		}
		if n.Entity != nil {
			t.Errorf("%s should be a bare prefix", key)
		}
		if !n.IsURL() {
			t.Errorf("%s should be a URL node", key)
		}
		if n.Visited() || n.Discovered() {
			t.Errorf("%s: a prefix is neither visited nor discovered", key)
		}
		if n.Label() != key {
			t.Errorf("%s label = %q", key, n.Label())
		}
	}
	leaf := m.Find("https://app.test/api/v1/orders")
	if leaf == nil || leaf.Entity == nil || !leaf.Visited() {
		t.Fatalf("the requested URL should be visited: %+v", leaf)
	}
	if leaf.Name != "orders" || leaf.Path != "/api/v1/orders" {
		t.Errorf("leaf name = %q path = %q", leaf.Name, leaf.Path)
	}
}

func TestTemplatesIdentifiers(t *testing.T) {
	m := build(
		flow(1, "GET", "app.test", "/users/12", "", "", 200, "", ""),
		flow(2, "GET", "app.test", "/users/13?full=1", "", "", 404, "", ""),
		flow(3, "DELETE", "app.test", "/users/13", "", "", 204, "", ""),
		flow(4, "GET", "app.test", "/users/me", "", "", 200, "", ""),
		flow(5, "GET", "app.test", "/files/3f2b8c1e-0a4d-4b7e-9c2a-1d5e6f7a8b9c/raw", "", "", 200, "", ""),
	)
	users := m.Find("https://app.test/users")
	if users == nil {
		t.Fatal("/users missing")
	}
	if got := names(users.Children); !reflect.DeepEqual(got, []string{"me", "{id}"}) {
		t.Fatalf("users children = %v (literal before templated)", got)
	}
	if users.Entity != nil {
		t.Error("/users was never requested, so it should have no entity")
	}

	byID := m.Find("https://app.test/users/{id}")
	if byID == nil || byID.Entity == nil {
		t.Fatal("/users/{id} missing")
	}
	ops := byID.Entity.Operations()
	if len(ops) != 2 || ops[0].Method != "GET" || ops[1].Method != "DELETE" {
		t.Fatalf("operations = %+v", ops)
	}
	if !reflect.DeepEqual(byID.Entity.FlowIDs(), []uint64{1, 2, 3}) {
		t.Errorf("flow IDs = %v", byID.Entity.FlowIDs())
	}
	if p := byID.Entity.URLAttrs.PathParams; len(p) != 1 || p[0].Type != "integer" ||
		!reflect.DeepEqual(p[0].Examples, []string{"12", "13"}) {
		t.Errorf("path params = %+v", p)
	}
	if m.Find("https://app.test/files/{id}/raw") == nil {
		t.Error("UUID segment not templated")
	}
}

// TestTotalsCountEachRequestOnce: a domain's and host's own Hits already
// count everything under them, so the tree sums the URLs instead.
func TestTotalsCountEachRequestOnce(t *testing.T) {
	m := build(
		flow(1, "GET", "app.test", "/a", "", "", 200, "", ""),
		flow(2, "GET", "app.test", "/a", "", "", 200, "", ""),
		flow(3, "GET", "app.test", "/b/c", "", "", 200, "", ""),
	)
	if got := m.Roots[0].Total; got != 3 {
		t.Errorf("domain total = %d, want 3", got)
	}
	if got := m.Find("https://app.test/a").Total; got != 2 {
		t.Errorf("/a total = %d, want 2", got)
	}
	if got := m.Find("https://app.test/b").Total; got != 1 {
		t.Errorf("/b (a prefix) total = %d, want the one below it", got)
	}
}

func TestDiscoveredLinks(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`<a href="/about">About</a> <a href="team?x=1">Team</a>
		<img src="https://cdn.other.test/logo.png"> <a href="mailto:a@b.test">mail</a>
		<form action="/search"></form> <a href="https://app.test/users/99">u</a>`))
	zw.Close()

	m := build(
		flow(1, "GET", "app.test", "/home/", "", "", 200,
			"Content-Type: text/html\r\nContent-Encoding: gzip\r\n", gz.String()),
		flow(2, "GET", "app.test", "/about", "", "", 200, "", ""),
		flow(3, "GET", "app.test", "/main.js", "", "", 200,
			"Content-Type: application/javascript\r\n", `fetch("/api/v2/orders"); x = '/'; re = /a/g;`),
	)
	for _, c := range []struct {
		key        string
		discovered bool
	}{
		{"https://app.test/about", false}, // linked and visited: visited wins
		{"https://app.test/home/team", true},
		{"https://app.test/search", true},
		{"https://app.test/users/{id}", true},
		{"https://app.test/api/v2/orders", true},
	} {
		n := m.Find(c.key)
		if n == nil {
			t.Errorf("%s missing", c.key)
			continue
		}
		if n.Discovered() != c.discovered {
			t.Errorf("%s discovered = %v, want %v", c.key, n.Discovered(), c.discovered)
		}
	}
	if n := m.Find("https://app.test/search"); n != nil &&
		!reflect.DeepEqual(n.Entity.LinkedFrom(), []string{"https://app.test/home"}) {
		t.Errorf("linked from = %v", n.Entity.LinkedFrom())
	}
	// The other host is related, not drawn, so it is not in the tree.
	for _, r := range m.Roots {
		if r.Name != "app.test" {
			t.Errorf("other host %q in the tree", r.Name)
		}
	}
}

func TestSkipsConnect(t *testing.T) {
	m := build(capture.Flow{ID: 1, Method: "CONNECT", Host: "app.test:443", Path: "app.test:443"})
	if len(m.Roots) != 0 {
		t.Errorf("CONNECT mapped: %v", names(m.Roots))
	}
}

// TestBuildInventsAMissingOrigin: a URL whose origin is not in the list
// still reaches the tree, under a bare origin node.
func TestBuildInventsAMissingOrigin(t *testing.T) {
	m := Build([]entities.Entity{{
		Type: TypeURL, Key: "https://lone.test/x", Parent: "https://lone.test",
		Scheme: "https", Host: "lone.test", Path: "/x", Hits: 1,
	}})
	if len(m.Roots) != 1 {
		t.Fatalf("roots = %v", names(m.Roots))
	}
	origin := m.Roots[0]
	if origin.Name != "https://lone.test" || origin.Type != TypeHost || origin.Entity != nil {
		t.Fatalf("origin = %+v", origin)
	}
	if origin.Label() != "https://lone.test" {
		t.Errorf("invented origin label = %q", origin.Label())
	}
	if n := m.Find("https://lone.test/x"); n == nil || n.Entity == nil {
		t.Error("the URL did not reach the tree")
	}
	if origin.Total != 1 {
		t.Errorf("invented origin total = %d", origin.Total)
	}
}

func TestFindOnANilMap(t *testing.T) {
	var m *Map
	if m.Find("anything") != nil {
		t.Error("Find on a nil map should be nil")
	}
	if c := m.KindCounts(); len(c) != 0 {
		t.Errorf("KindCounts on a nil map = %v", c)
	}
	if c := m.TypeCounts(); len(c) != 0 {
		t.Errorf("TypeCounts on a nil map = %v", c)
	}
}

// ---- test helpers ----------------------------------------------------------

// build makes a tree from flows the way the app does: through the entities
// store, with one target (app.test), so what the pages link to elsewhere
// is related rather than drawn.
func build(flows ...capture.Flow) *Map {
	var b entities.Batch
	for _, f := range flows {
		if v, links, ok := entities.Parse(f); ok {
			b.Visits = append(b.Visits, v)
			b.Discoveries = append(b.Discoveries, links...)
		}
	}
	s := entities.NewStore()
	s.SetScope(func(host string) (string, bool) { return "t1", host == "app.test" })
	s.Apply(b)
	return Build(s.Mapped(""))
}

// flow builds a finished flow from a raw request and response.
func flow(id uint64, method, host, path, reqHead, reqBody string, status int, respHead, respBody string) capture.Flow {
	req := method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\n" + reqHead
	if reqBody != "" {
		req += fmt.Sprintf("Content-Length: %d\r\n", len(reqBody))
	}
	req += "\r\n" + reqBody
	f := capture.Flow{
		ID: id, State: capture.StateComplete, Method: method, Host: host, Path: path, Secure: true,
		Request: capture.Message{Raw: []byte(req)},
	}
	if status != 0 {
		text := fmt.Sprintf("%d Status", status)
		f.StatusCode, f.StatusText = status, text
		resp := "HTTP/1.1 " + text + "\r\n" + respHead + fmt.Sprintf("Content-Length: %d\r\n\r\n", len(respBody)) + respBody
		f.Response = capture.Message{Raw: []byte(resp)}
	}
	return f
}

func names(nodes []*Node) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
