package sitemap

import (
	"reflect"
	"testing"
)

func kindsTree() *Map {
	return build(
		flow(1, "GET", "app.test", "/", "", "", 200, "Content-Type: text/html; charset=utf-8\r\n",
			`<link href="/static/site.css"><img src="/static/logo.svg"><a href="/report.pdf">r</a>`),
		flow(2, "GET", "app.test", "/static/app.js", "", "", 200, "Content-Type: application/octet-stream\r\n", ""),
		flow(3, "GET", "app.test", "/api/users", "", "", 200, "Content-Type: application/vnd.api+json\r\n", "{}"),
		flow(4, "GET", "app.test", "/feed", "", "", 200, "Content-Type: application/rss+xml\r\n", ""),
		flow(5, "GET", "app.test", "/pending", "", "", 0, "", ""),
	)
}

func TestKinds(t *testing.T) {
	m := kindsTree()
	for key, want := range map[string]Kind{
		"https://app.test/":                KindHTML,
		"https://app.test/static/app.js":   KindJavaScript, // octet-stream: by extension
		"https://app.test/static/site.css": KindCSS,        // discovered: by extension
		"https://app.test/static/logo.svg": KindImage,
		"https://app.test/report.pdf":      KindFile,
		"https://app.test/api/users":       KindJSON,
		"https://app.test/feed":            KindXML,
		"https://app.test/pending":         KindOther,
	} {
		n := m.Find(key)
		if n == nil || n.Entity == nil {
			t.Errorf("%s missing", key)
			continue
		}
		if got := n.Entity.Kinds(); !reflect.DeepEqual(got, []Kind{want}) {
			t.Errorf("%s kinds = %v, want %v", key, got, want)
		}
	}
	if c := m.KindCounts(); c[KindImage] != 1 || c[KindHTML] != 1 {
		t.Errorf("kind counts = %v", c)
	}
	if c := m.TypeCounts(); c[TypeDomain] != 1 || c[TypeHost] != 2 {
		t.Errorf("type counts = %v, want one domain and two hosts (name and origin)", c)
	}
}

// TestFilterByKind: hiding a content type drops the URLs serving it, but
// keeps a path that leads to shown ones, as a bare prefix.
func TestFilterByKind(t *testing.T) {
	m := kindsTree()
	f := m.Filter(Filter{Kind: func(k Kind) bool { return k != KindHTML && k != KindImage }})

	// /static is a bare prefix leading to app.js and site.css, both shown,
	// so it stays and keeps its shape.
	static := f.Find("https://app.test/static")
	if static == nil {
		t.Fatal("/static should stay, as a prefix leading to shown URLs")
	}
	if static.Entity != nil {
		t.Error("/static was never requested, so it has no entity either way")
	}
	if got := names(static.Children); !reflect.DeepEqual(got, []string{"app.js", "site.css"}) {
		t.Errorf("under /static = %v, want the two shown URLs", got)
	}
	if f.Find("https://app.test/static/logo.svg") != nil {
		t.Error("hidden image kept")
	}
	if f.Find("https://app.test/static/site.css") == nil {
		t.Error("shown CSS dropped")
	}
	// The site root served HTML and has nothing under it — in this tree
	// "/" is a leaf beside the other top-level paths, not their parent —
	// so it goes entirely.
	if f.Find("https://app.test/") != nil {
		t.Error("the site root served only HTML, so it should be gone")
	}
	// The original is untouched: Filter copies nodes.
	if m.Find("https://app.test/").Entity == nil {
		t.Error("Filter changed the original tree")
	}
	if f.Roots[0].Total != 4 { // app.js, users, feed, pending
		t.Errorf("filtered total = %d, want 4", f.Roots[0].Total)
	}
}

func TestFilterHidingEverything(t *testing.T) {
	m := kindsTree()
	if f := m.Filter(Filter{Kind: func(Kind) bool { return false }}); len(f.Roots) != 0 {
		t.Errorf("roots left = %v", names(f.Roots))
	}
	if f := m.Filter(Filter{Type: func(Type) bool { return false }}); len(f.Roots) != 0 {
		t.Errorf("hiding every type left %v", names(f.Roots))
	}
}

func TestFilterNilFuncsShowEverything(t *testing.T) {
	m := kindsTree()
	f := m.Filter(Filter{})
	if len(f.Roots) != len(m.Roots) {
		t.Fatalf("roots = %d, want %d", len(f.Roots), len(m.Roots))
	}
	if f.Roots[0].Total != m.Roots[0].Total {
		t.Errorf("total = %d, want %d", f.Roots[0].Total, m.Roots[0].Total)
	}
}

// TestFilterByTypeLifts: hiding a type takes its nodes out and lifts what
// was under them into their place.
func TestFilterByTypeLifts(t *testing.T) {
	m := kindsTree()

	// Hiding domains makes the hosts the roots.
	noDomains := m.Filter(Filter{Type: func(t Type) bool { return t != TypeDomain }})
	if got := names(noDomains.Roots); !reflect.DeepEqual(got, []string{"app.test"}) {
		t.Fatalf("with domains hidden, roots = %v, want the name host", got)
	}
	if noDomains.Roots[0].Type != TypeHost {
		t.Errorf("root type = %q", noDomains.Roots[0].Type)
	}

	// Hiding hosts puts the paths under the domain, each top-level one
	// named with its host so it still says where it is.
	noHosts := m.Filter(Filter{Type: func(t Type) bool { return t != TypeHost }})
	if got := names(noHosts.Roots); !reflect.DeepEqual(got, []string{"app.test"}) {
		t.Fatalf("with hosts hidden, roots = %v, want the domain", got)
	}
	var lifted []string
	for _, c := range noHosts.Roots[0].Children {
		lifted = append(lifted, c.Name)
	}
	for _, name := range lifted {
		if len(name) < len("app.test") || name[:len("app.test")] != "app.test" {
			t.Errorf("lifted path %q should be named with its host", name)
		}
	}

	// Hiding URLs drops every path.
	noURLs := m.Filter(Filter{Type: func(t Type) bool { return t != TypeURL }})
	var walk func(*Node)
	walk = func(n *Node) {
		if n.IsURL() {
			t.Errorf("URL %q survived", n.Key)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range noURLs.Roots {
		walk(r)
	}
	if len(noURLs.Roots) != 1 {
		t.Errorf("roots = %v, want the domain", names(noURLs.Roots))
	}
}
