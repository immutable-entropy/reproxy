// Package sitemap arranges the engagement's entities (package entities) as
// the tree the Explorer draws: domains at the top, the hosts under each,
// and under a host the paths of its URLs, one node per segment.
//
// It only arranges what the entities store already holds — it discovers
// nothing and reaches nothing — so it is presentation, not
// intercept/edit/replay machinery, and a plugin candidate like filter and
// syntaxcheck. The tree is rebuilt from the store's rows on demand (never
// on the proxy's hot path), and has no Qt dependency, so an agent can read
// the same tree the UI shows.
package sitemap

import (
	"sort"
	"strings"

	"reproxy/internal/entities"
)

// Map is the tree built from a set of entities: one root per domain (and
// per host that has no registrable domain — an address or a single label).
type Map struct {
	Roots []*Node
	byKey map[string]*Node
}

// Find returns the node with the given Key, or nil.
func (m *Map) Find(key string) *Node {
	if m == nil {
		return nil
	}
	return m.byKey[key]
}

// Node is one entity in the tree, or one segment of a URL's path.
type Node struct {
	// Key identifies the node across rebuilds: its entity's key, or, for a
	// path prefix, scheme://host + Path, the key the URL there would have.
	Key string
	// Name is the node as shown: the domain or address, the origin
	// (https://host), or — for a URL — the one path segment, "{id}" when
	// it is templated.
	Name string
	// Type is the entity's type; a path prefix is TypeURL with no Entity.
	Type Type
	// Scheme and Host are where the node is. A domain has neither; a name
	// host has Host alone; an origin and a URL have both.
	Scheme string
	Host   string
	// Path is the templated path from the host, e.g. /users/{id}/orders.
	// It is empty for anything but a URL.
	Path string
	// TargetID is the target the node is drawn under (see Store.Drawn).
	TargetID string

	Children []*Node
	// Entity is nil for a path prefix: nothing was requested or linked to
	// at exactly this path, it only leads to deeper ones. It is also nil
	// for an origin Build had to invent (see Build).
	Entity *Entity
	// Total counts the requests made to this node and everything below it.
	Total int
}

// IsURL reports whether the node is a URL or a path prefix: what sits
// under a host.
func (n *Node) IsURL() bool { return n.Type == entities.TypeURL }

// Label is how the node is named in full: the domain or address, the
// origin, or the templated URL (https://host/users/{id}).
func (n *Node) Label() string {
	if n.Entity != nil {
		return n.Entity.Label()
	}
	if n.IsURL() {
		return n.Scheme + "://" + n.Host + n.Path
	}
	return n.Name
}

// Visited reports whether any request was made to exactly this entity.
func (n *Node) Visited() bool { return n.Entity != nil && n.Entity.Visited() }

// Discovered reports whether this entity was only linked to or named by a
// target, never requested.
func (n *Node) Discovered() bool { return n.Entity != nil && !n.Visited() }

// The entity's parts, as the tree shows them.
type (
	Entity    = entities.Entity
	Operation = entities.Operation
	Response  = entities.Response
	Param     = entities.Param
	Type      = entities.Type
)

// The entity types, re-exported so the tree's users needn't import
// entities for them.
const (
	TypeDomain = entities.TypeDomain
	TypeHost   = entities.TypeHost
	TypeURL    = entities.TypeURL
)

// Types lists every type, outermost first.
var Types = entities.Types

// Build arranges entities into a tree. An entity whose parent isn't among
// them is a root; a URL whose host isn't gets a plain node for its origin.
//
// The entities carry the hierarchy themselves (Entity.Parent: URL →
// origin → name host → domain), so Build follows that rather than working
// it out again: two passes, because a parent may come after its child in
// the list.
func Build(list []entities.Entity) *Map {
	m := &Map{byKey: make(map[string]*Node)}

	// The entities themselves, bar the URLs: the frame the paths hang on.
	var frame []*Node
	for i := range list {
		e := &list[i]
		if e.Type == entities.TypeURL {
			continue
		}
		n := &Node{Key: e.Key, Name: e.Label(), Type: e.Type, Scheme: e.Scheme, Host: e.Host,
			TargetID: e.TargetID, Entity: e}
		m.byKey[n.Key] = n
		frame = append(frame, n)
	}
	for _, n := range frame {
		if p := m.byKey[n.Entity.Parent]; p != nil && n.Entity.Parent != "" {
			p.Children = append(p.Children, n)
		} else {
			m.Roots = append(m.Roots, n)
		}
	}

	// Then the URLs, each split into path segments under its origin.
	for i := range list {
		e := &list[i]
		if e.Type != entities.TypeURL {
			continue
		}
		n := m.walk(m.origin(e), e.Path)
		n.Entity = e
		n.TargetID = e.TargetID
	}

	sortNodes(m.Roots)
	for _, r := range m.Roots {
		finishNode(r)
	}
	return m
}

// origin returns the node for e's host, inventing a plain one (no Entity)
// and making it a root when the store's rows didn't include it — a tree
// missing a URL would be worse than one with a bare origin in it.
func (m *Map) origin(e *entities.Entity) *Node {
	key := entities.HostKey(e.Scheme, e.Host)
	if n, ok := m.byKey[key]; ok {
		return n
	}
	n := &Node{Key: key, Name: e.Scheme + "://" + e.Host, Type: entities.TypeHost,
		Scheme: e.Scheme, Host: e.Host, TargetID: e.TargetID}
	m.byKey[key] = n
	m.Roots = append(m.Roots, n)
	return n
}

// walk returns the node for path under host, creating it and any missing
// path prefixes on the way. The path is already templated.
//
// The site root is a node of its own ("/" under the origin) rather than
// the origin itself: the store keys them apart (https://host for the
// origin, https://host/ for the URL), and Find is called with an entity's
// own key.
func (m *Map) walk(host *Node, path string) *Node {
	segments := splitPath(path)
	if len(segments) == 0 {
		return m.prefix(host, host, "/", "/")
	}
	n := host
	at := ""
	for _, name := range segments {
		at += "/" + name
		n = m.prefix(n, host, name, at)
	}
	return n
}

// prefix returns the node at path under parent, adding it if it is not
// there yet. host gives the scheme and host every node below it carries.
func (m *Map) prefix(parent, host *Node, name, path string) *Node {
	key := host.Scheme + "://" + host.Host + path
	if n, ok := m.byKey[key]; ok {
		return n
	}
	n := &Node{Key: key, Name: name, Type: entities.TypeURL,
		Scheme: host.Scheme, Host: host.Host, Path: path}
	m.byKey[key] = n
	parent.Children = append(parent.Children, n)
	return n
}

func splitPath(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// finishNode totals requests and sorts children, depth first. Only a URL's
// own hits are counted: a domain's or host's Hits already counts
// everything under it, so adding them would count each request twice.
func finishNode(n *Node) int {
	total := 0
	if n.Entity != nil && n.IsURL() {
		total = n.Entity.Hits
	}
	sortNodes(n.Children)
	for _, c := range n.Children {
		total += finishNode(c)
	}
	n.Total = total
	return total
}

// sortNodes orders siblings by name, case-insensitively, with templated
// segments after the literal ones.
func sortNodes(nodes []*Node) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i].Name, nodes[j].Name
		pa, pb := strings.HasPrefix(a, "{"), strings.HasPrefix(b, "{")
		if pa != pb {
			return pb
		}
		la, lb := strings.ToLower(a), strings.ToLower(b)
		if la != lb {
			return la < lb
		}
		return a < b
	})
}
