package sitemap

import "reproxy/internal/entities"

// Kind is the type of content a URL serves, for filtering the tree.
type Kind = entities.Kind

// The kinds, re-exported so the tree's users needn't import entities for
// them.
const (
	KindHTML       = entities.KindHTML
	KindJavaScript = entities.KindJavaScript
	KindCSS        = entities.KindCSS
	KindJSON       = entities.KindJSON
	KindXML        = entities.KindXML
	KindImage      = entities.KindImage
	KindFont       = entities.KindFont
	KindMedia      = entities.KindMedia
	KindFile       = entities.KindFile
	KindText       = entities.KindText
	KindOther      = entities.KindOther
)

// Kinds lists every kind, in the order the UI offers them.
var Kinds = entities.Kinds

// KindCounts counts the URLs serving each kind of content (a URL serving
// two kinds counts once for each).
func (m *Map) KindCounts() map[Kind]int {
	counts := make(map[Kind]int)
	m.each(func(n *Node) {
		if n.Entity != nil {
			for _, k := range n.Entity.Kinds() {
				counts[k]++
			}
		}
	})
	return counts
}

// TypeCounts counts the entities of each type in the tree.
func (m *Map) TypeCounts() map[Type]int {
	counts := make(map[Type]int)
	m.each(func(n *Node) {
		if n.Entity != nil {
			counts[n.Type]++
		}
	})
	return counts
}

func (m *Map) each(fn func(*Node)) {
	if m == nil {
		return
	}
	var walk func(n *Node)
	walk = func(n *Node) {
		fn(n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range m.Roots {
		walk(r)
	}
}

// Filter says what part of a tree to show. A nil func shows everything.
type Filter struct {
	// Kind says whether URLs serving a type of content are shown.
	Kind func(Kind) bool
	// Type says whether entities of a type are shown.
	Type func(Type) bool
}

// Filter returns the part of m that f shows.
//
// A URL is kept when any of its kinds is shown. A path prefix whose own
// URL is hidden but which leads to shown ones stays as a plain prefix (no
// Entity), so the tree keeps its shape; anything else hidden is dropped.
// A domain or host emptied by the content filter is dropped too; one that
// never had anything under it is kept.
//
// Hiding a type takes its nodes out of the tree and lifts what was under
// them into their place: hiding domains makes hosts the roots; hiding
// hosts puts their paths under the domain, each top-level one named with
// its host (api.example.com/users) so it still says where it is. Hiding
// URLs drops every path.
//
// Request totals count only what is shown. Nodes are copied; Entities are
// shared with m, so look details up in m (Find works the same on both).
func (m *Map) Filter(f Filter) *Map {
	showKind := f.Kind
	if showKind == nil {
		showKind = func(Kind) bool { return true }
	}
	showType := f.Type
	if showType == nil {
		showType = func(Type) bool { return true }
	}
	kindActive := false
	for _, k := range Kinds {
		if !showKind(k) {
			kindActive = true
		}
	}
	out := &Map{byKey: make(map[string]*Node)}

	// keep returns what n becomes in the filtered tree: itself, what was
	// under it (when its type is hidden), or nothing. lifted is set for a
	// top-level path whose host is hidden.
	var keep func(n *Node, lifted bool) []*Node
	keep = func(n *Node, lifted bool) []*Node {
		if n.IsURL() {
			if !showType(TypeURL) {
				return nil
			}
			c := *n
			c.Children = nil
			if lifted {
				c.Name = n.Host + n.Path
			}
			if c.Entity != nil && !anyShown(c.Entity.Kinds(), showKind) {
				c.Entity = nil
			}
			for _, child := range n.Children {
				c.Children = append(c.Children, keep(child, false)...)
			}
			if c.Entity == nil && len(c.Children) == 0 {
				return nil
			}
			out.byKey[c.Key] = &c
			return []*Node{&c}
		}

		hidden := !showType(n.Type)
		var kids []*Node
		for _, child := range n.Children {
			kids = append(kids, keep(child, hidden && n.Type == TypeHost && child.IsURL())...)
		}
		if len(kids) == 0 && len(n.Children) > 0 && kindActive && showType(TypeURL) {
			return nil // the content filter hid everything under it
		}
		if hidden {
			return kids
		}
		c := *n
		c.Children = kids
		out.byKey[c.Key] = &c
		return []*Node{&c}
	}
	for _, r := range m.Roots {
		out.Roots = append(out.Roots, keep(r, false)...)
	}
	sortNodes(out.Roots)
	for _, r := range out.Roots {
		finishNode(r)
	}
	return out
}

func anyShown(kinds []Kind, show func(Kind) bool) bool {
	for _, k := range kinds {
		if show(k) {
			return true
		}
	}
	return false
}
