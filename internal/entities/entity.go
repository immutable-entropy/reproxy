// Package entities is the engagement's Entity Explorer: the domains, hosts
// and URLs the work has turned up, each visited (traffic was sent to it) or
// only discovered (something referred to it, or a target named it).
//
// Every entity has a type — Domain, Host or URL — with attributes of its
// own, and what all of them share: an identity (Key), a place in the
// hierarchy (Parent), the target it belongs to, what first put it there,
// how many requests reached it, what linked to it, and a key/value store
// for whatever the user, an agent or a plugin wants to keep on it. They
// nest as a strict chain:
//
//	Domain  example.com               the registrable domain (eTLD+1)
//	└ Host  https://api.example.com   an origin: scheme, name and port
//	  └ URL https://api.example.com/users/{id}
//
// A host whose name has no registrable domain — an IP address, a single
// label such as localhost — hangs instead under an address-only Host for
// that name (key "10.0.0.5", no scheme), which is also what an IP target
// creates.
//
// A URL is identified by its scheme, host and templated path. Path
// segments that look like identifiers (numbers, UUIDs, long hex or token
// strings) are folded into one templated segment, so /users/12 and
// /users/13 are one URL, /users/{id}, with an OpenAPI-style summary of the
// methods used on it, their parameters and the responses seen. Keys are
// unique in the engagement's capture.db, so however many writers there are
// — the indexer following traffic, a plugin adding what it found, the
// targets — an entity is only ever one row.
//
// The Explorer is drawn from this store, never from the traffic, so it
// outlives Clear Requests just as findings do. Traffic reaches it through
// the Indexer, a capture.Store subscriber that only queues on the proxy's
// goroutine and parses on its own; plugins reach it through Store.Discover.
// Putting a URL in the store puts its Host and Domain there too.
//
// Every response is read for what it refers to, on any host. What lands on
// a host no target covers isn't drawn in the Explorer: it makes that host
// Related (see related.go), until the user promotes it to a target and its
// entities are drawn at once.
//
// It is Qt-free core: the UI draws it (through package sitemap), agents and
// plugins read and extend it.
package entities

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Limits on how much detail is kept per entity.
const (
	maxExamples  = 5   // concrete paths, and values per parameter
	maxLinks     = 50  // what discovered an entity
	maxSightings = 200 // the most recent requests made to a URL
	maxServers   = 5   // Server header values kept per host
	maxKV        = 200 // key/value pairs per entity
	maxAddresses = 32  // addresses kept per name host
)

// Origins of an entity: what first put it in the store. A plugin's is
// "plugin:<name>" (see PluginOrigin).
const (
	OriginTraffic = "traffic" // a request was made to it
	OriginLink    = "link"    // a response linked to it
	// OriginReferer is a link learnt from a request's Referer or Origin: a
	// page on another host led to it.
	OriginReferer = "referer"
	// OriginTarget is an entity created for a target: the domain or
	// address the user put in scope.
	OriginTarget = "target"
)

// PluginOrigin is the origin of what a plugin adds.
func PluginOrigin(name string) string { return "plugin:" + name }

// Entity is one domain, host or URL.
type Entity struct {
	// ID is the entity's row in the engagement's capture.db.
	ID   uint64
	Type Type
	// Key identifies the entity: the name for a domain or a name host,
	// scheme://host for an origin, scheme://host + Path for a URL.
	Key string
	// Parent is the key of the entity it sits under, "" for a root.
	Parent string

	// Scheme, Host and Path are where the entity is. A domain has only Host
	// (its name); a host has Scheme (none for a name host) and Host
	// (lower case, with a port only when it isn't the scheme's default); a
	// URL has all three, Path templated (/users/{id}/orders).
	Scheme string
	Host   string
	Path   string

	// TargetID is the target the entity was first seen under.
	TargetID string
	// Origin is what first put the entity in the store: OriginTraffic,
	// OriginLink, OriginReferer, OriginTarget or a plugin's origin.
	Origin string
	// Hits counts the requests made to the entity or anything under it
	// (for a URL, to it), including those whose sightings have been let
	// go. None means it was only ever discovered.
	Hits int
	// Links are what discovered the entity: the responses that linked to
	// it and the plugins that added it, the first maxLinks of them.
	Links []Link
	// KV is the entity's key/value store, in the order keys were first set.
	KV []Pair

	FirstSeen time.Time
	LastSeen  time.Time

	// Exactly one of these is set, for the entity's Type.
	DomainAttrs *DomainAttrs
	HostAttrs   *HostAttrs
	URLAttrs    *URLAttrs
}

// DomainAttrs are what is particular to a domain.
type DomainAttrs struct {
	// Suffix is the public suffix the domain is registered under, e.g.
	// "co.uk"; ICANN is set when the suffix is an ICANN one rather than a
	// privately run one (such as github.io).
	Suffix string `json:"suffix,omitempty"`
	ICANN  bool   `json:"icann,omitempty"`
}

// HostAttrs are what is particular to a host. A host is either a name
// host (no scheme: a DNS name or an IP address, which is what DNS facts
// are about) or one of the origins served on it (scheme://name[:port],
// which is what traffic reaches).
type HostAttrs struct {
	// Port is the port the origin is served on: the one in its URLs, or the
	// scheme's default. It is 0 for a name host.
	Port int `json:"port,omitempty"`
	// IP is set when the host's name is an IP address.
	IP bool `json:"ip,omitempty"`
	// Servers are the Server headers an origin's responses carried, the
	// first few.
	Servers []string `json:"servers,omitempty"`
	// Addresses are the IP addresses a name host is known to resolve to
	// (see DiscoverHost), the first maxAddresses. Each is a host of its own.
	Addresses []string `json:"addresses,omitempty"`
}

// IsName reports whether e is a name host: a DNS name or an IP address on
// its own, with the origins served on it beneath it.
func (e *Entity) IsName() bool { return e.Type == TypeHost && e.Scheme == "" }

// URLAttrs are what is particular to a URL: an OpenAPI-style path item.
type URLAttrs struct {
	// Operations are the methods used on it, in conventional order (GET,
	// HEAD, POST, PUT, PATCH, DELETE, OPTIONS, then the rest).
	Operations []*Operation `json:"operations,omitempty"`
	// PathParams are the templated segments, in order, with the values seen.
	PathParams []*Param `json:"path_params,omitempty"`
	// Examples are concrete paths requested (or, for a URL only
	// discovered, linked to), e.g. /users/12.
	Examples []string `json:"examples,omitempty"`
	// Kinds are the types of content served here, in the order first seen:
	// from the responses' Content-Type, or the path's extension when there
	// is no usable one (and always, for a path only linked to).
	Kinds []Kind `json:"kinds,omitempty"`
	// Sightings are the most recent requests made to it, oldest first.
	// They are kept in a table of their own, not in the attributes.
	Sightings []Sighting `json:"-"`
}

// Pair is one entry of an entity's key/value store.
type Pair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Label is how the entity is named: the domain or address, the origin
// (https://host), or the templated URL (https://host/users/{id}).
func (e *Entity) Label() string {
	switch {
	case e.Type == TypeURL:
		return e.Scheme + "://" + e.Host + e.Path
	case e.Scheme != "":
		return e.Scheme + "://" + e.Host
	}
	return e.Host
}

// Visited reports whether any request was made to the entity (to anything
// under it, for a domain or host).
func (e *Entity) Visited() bool { return e.Hits > 0 }

// Get returns the value of a key in the entity's key/value store.
func (e *Entity) Get(key string) (string, bool) {
	for _, p := range e.KV {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

// LinkedFrom returns the URLs whose responses link here, each once.
func (e *Entity) LinkedFrom() []string {
	var out []string
	for _, l := range e.Links {
		if l.FromURL != "" {
			addUnique(&out, l.FromURL)
		}
	}
	return out
}

// FlowIDs returns the flows of a URL's sightings, in arrival order. A flow
// cleared from the traffic list no longer resolves. Other types have none.
func (e *Entity) FlowIDs() []uint64 {
	if e.URLAttrs == nil {
		return nil
	}
	ids := make([]uint64, 0, len(e.URLAttrs.Sightings))
	for _, s := range e.URLAttrs.Sightings {
		ids = append(ids, s.FlowID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Operations returns a URL's operations (nil for other types).
func (e *Entity) Operations() []*Operation {
	if e.URLAttrs == nil {
		return nil
	}
	return e.URLAttrs.Operations
}

// Kinds returns the types of content a URL serves (nil for other types).
func (e *Entity) Kinds() []Kind {
	if e.URLAttrs == nil {
		return nil
	}
	return e.URLAttrs.Kinds
}

// Operation is one method on a URL.
type Operation struct {
	Method string `json:"method"`
	// Requests counts the requests made with this method.
	Requests int      `json:"requests"`
	Query    []*Param `json:"query,omitempty"`
	// Body is the request body's fields: form fields, multipart parts, or
	// JSON members flattened to dotted names ("user.name", "tags[]").
	Body []*Param `json:"body,omitempty"`
	// BodyTypes are the request content types seen, e.g. application/json.
	BodyTypes []string    `json:"body_types,omitempty"`
	Responses []*Response `json:"responses,omitempty"`
}

// Response is one status code seen in reply to an operation.
type Response struct {
	Status       int      `json:"status"`
	Text         string   `json:"text"` // e.g. "200 OK"
	ContentTypes []string `json:"content_types,omitempty"`
	Count        int      `json:"count"`
}

// Param is a parameter seen on an operation, or a templated path segment.
type Param struct {
	Name string `json:"name"`
	// Type is inferred from the values seen: string, integer, number,
	// boolean, array, object, null or file.
	Type     string   `json:"type"`
	Examples []string `json:"examples,omitempty"`
	// Seen counts the requests carrying it.
	Seen int `json:"seen"`
	// Required is set when every request of the operation carried it.
	Required bool `json:"required"`
}

// Link is one thing that discovered an entity.
type Link struct {
	// Source is OriginLink for a link found in a response, OriginReferer
	// for a request another host's page led to, or a plugin's origin.
	Source string
	// FromURL is the URL of the response the link was found in, without
	// its query; FromFlowID is that response's flow. Either may be empty.
	FromURL    string
	FromFlowID uint64
	// Detail says where it was found, e.g. "json:$.links[3].href".
	Detail string
	Added  time.Time
}

// Sighting is one request made to a URL: a snapshot, so it still reads
// after the flow is cleared from the traffic list.
type Sighting struct {
	FlowID uint64
	Method string
	Status int
	URL    string
	Seen   time.Time
}

// Clone returns a deep copy, safe to hand out of the store.
func (e *Entity) Clone() Entity {
	c := *e
	c.Links = append([]Link(nil), e.Links...)
	c.KV = append([]Pair(nil), e.KV...)
	if e.DomainAttrs != nil {
		d := *e.DomainAttrs
		c.DomainAttrs = &d
	}
	if e.HostAttrs != nil {
		h := *e.HostAttrs
		h.Servers = append([]string(nil), e.HostAttrs.Servers...)
		h.Addresses = append([]string(nil), e.HostAttrs.Addresses...)
		c.HostAttrs = &h
	}
	if u := e.URLAttrs; u != nil {
		a := URLAttrs{
			Operations: make([]*Operation, len(u.Operations)),
			PathParams: cloneParams(u.PathParams),
			Examples:   append([]string(nil), u.Examples...),
			Kinds:      append([]Kind(nil), u.Kinds...),
			Sightings:  append([]Sighting(nil), u.Sightings...),
		}
		for i, op := range u.Operations {
			o := *op
			o.Query = cloneParams(op.Query)
			o.Body = cloneParams(op.Body)
			o.BodyTypes = append([]string(nil), op.BodyTypes...)
			o.Responses = make([]*Response, len(op.Responses))
			for j, r := range op.Responses {
				rc := *r
				rc.ContentTypes = append([]string(nil), r.ContentTypes...)
				o.Responses[j] = &rc
			}
			a.Operations[i] = &o
		}
		c.URLAttrs = &a
	}
	return c
}

func cloneParams(ps []*Param) []*Param {
	if ps == nil {
		return nil
	}
	out := make([]*Param, len(ps))
	for i, p := range ps {
		c := *p
		c.Examples = append([]string(nil), p.Examples...)
		out[i] = &c
	}
	return out
}

// ---- Keys and templating ----------------------------------------------------

// Address is where a URL lives, worked out from a concrete URL.
type Address struct {
	Scheme string
	Host   string
	// Path is the templated path; Concrete the path as requested.
	Path     string
	Concrete string
	// Values are the templated segments' values, in order.
	Values []string
	// Placeholders is set when the path came with {name} segments of its
	// own, as an OpenAPI document gives them: it is a template, not an
	// example, and its values are unknown.
	Placeholders bool
}

// Key is the URL key for the address.
func (a Address) Key() string { return a.Scheme + "://" + a.Host + a.Path }

// AddressOf templates a concrete path on a host. The host is lower-cased
// and loses a port that is the scheme's default.
func AddressOf(scheme, host, path string) Address {
	scheme = strings.ToLower(scheme)
	host = normalHost(scheme, host)
	segs := splitPath(path)
	a := Address{Scheme: scheme, Host: host, Concrete: "/" + strings.Join(segs, "/")}
	ids := 0
	var b strings.Builder
	for _, seg := range segs {
		name := seg
		switch {
		case isPlaceholder(seg):
			ids++
			name = paramName(ids)
			a.Placeholders = true
		case isIdentifier(seg):
			ids++
			name = paramName(ids)
			a.Values = append(a.Values, seg)
		}
		b.WriteByte('/')
		b.WriteString(name)
	}
	a.Path = b.String()
	if a.Path == "" {
		a.Path = "/"
	}
	return a
}

// isPlaceholder reports whether a segment is a template parameter as API
// descriptions write them: {id}, {orderId}, or %7Bid%7D once escaped.
func isPlaceholder(seg string) bool {
	if strings.HasPrefix(seg, "%7B") || strings.HasPrefix(seg, "%7b") {
		if u, err := url.PathUnescape(seg); err == nil {
			seg = u
		}
	}
	return len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' && !strings.ContainsAny(seg[1:len(seg)-1], "{}/")
}

func normalHost(scheme, host string) string {
	host = strings.ToLower(host)
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

// paramName names the nth templated segment of a path: {id}, {id2}, ...
func paramName(n int) string {
	if n == 1 {
		return "{id}"
	}
	return "{id" + strconv.Itoa(n) + "}"
}

// ---- Merging ------------------------------------------------------------------

// Visit is what one request contributes to the URL it was made to.
type Visit struct {
	FlowID   uint64
	TargetID string
	Address  Address
	Method   string
	URL      string // without the query
	Seen     time.Time

	Query    []Field
	Body     []Field
	BodyType string

	Status     int
	StatusText string
	RespType   string
	Kind       Kind
	// Server is the response's Server header, for the host.
	Server string
}

// Field is one parameter value from one request.
type Field struct {
	Name, Type, Value string
}

// Discovery is a URL something pointed at: a link in a response, or a URL
// a plugin found.
type Discovery struct {
	TargetID string
	Address  Address
	Link     Link
}

// HostDiscovery is a host learnt of without a URL: a name or address a
// plugin or agent found (in DNS records, say), and the addresses the name
// resolves to.
type HostDiscovery struct {
	// Name is a DNS name or an IP address, without scheme or port.
	Name     string
	TargetID string
	Link     Link
	// Addresses are IP addresses the name resolves to. Each becomes a host
	// of its own, with the same link.
	Addresses []string
}

// addLink adds what discovered an entity, unless the same source and page
// already did (or it has all it keeps). It reports whether it was added.
func (e *Entity) addLink(l Link) bool {
	for _, have := range e.Links {
		if have.Source == l.Source && have.FromURL == l.FromURL {
			return false
		}
	}
	if len(e.Links) >= maxLinks {
		return false
	}
	e.Links = append(e.Links, l)
	return true
}

// visit merges a request into a URL. The store counts the hit, on the URL
// and everything above it.
func (e *Entity) visit(v Visit) {
	u := e.URLAttrs
	addExample(&u.Examples, v.Address.Concrete)
	u.mergePathParams(v.Address.Values)
	u.addKind(v.Kind)

	op := u.operation(v.Method)
	op.Requests++
	mergeParams(&op.Query, v.Query, op.Requests)
	mergeParams(&op.Body, v.Body, op.Requests)
	if v.BodyType != "" {
		addUnique(&op.BodyTypes, v.BodyType)
	}
	if v.Status != 0 {
		r := op.response(v.Status, v.StatusText)
		r.Count++
		if v.RespType != "" {
			addUnique(&r.ContentTypes, v.RespType)
		}
	}

	u.Sightings = append(u.Sightings, Sighting{
		FlowID: v.FlowID, Method: v.Method, Status: v.Status, URL: v.URL, Seen: v.Seen,
	})
	if n := len(u.Sightings) - maxSightings; n > 0 {
		u.Sightings = append(u.Sightings[:0:0], u.Sightings[n:]...)
	}
}

// discover merges a discovery into a URL, and reports whether it added a
// link (a repeat of one already held adds nothing).
func (e *Entity) discover(d Discovery) bool {
	u := e.URLAttrs
	u.mergePathParams(d.Address.Values)
	if !e.Visited() && !d.Address.Placeholders {
		addExample(&u.Examples, d.Address.Concrete)
		u.addKind(KindOf("", d.Address.Concrete))
	}
	return e.addLink(d.Link)
}

// hit counts a request made to the entity or below it.
func (e *Entity) hit(v Visit) {
	e.Hits++
	if v.Seen.After(e.LastSeen) {
		e.LastSeen = v.Seen
	}
	// The Server header is the origin's: one name can serve several.
	if e.HostAttrs != nil && e.Scheme != "" && v.Server != "" && len(e.HostAttrs.Servers) < maxServers {
		addUnique(&e.HostAttrs.Servers, v.Server)
	}
}

func (u *URLAttrs) operation(method string) *Operation {
	for _, op := range u.Operations {
		if op.Method == method {
			return op
		}
	}
	op := &Operation{Method: method}
	u.Operations = append(u.Operations, op)
	sort.SliceStable(u.Operations, func(i, j int) bool {
		return methodLess(u.Operations[i].Method, u.Operations[j].Method)
	})
	return op
}

func (op *Operation) response(status int, text string) *Response {
	for _, r := range op.Responses {
		if r.Status == status {
			return r
		}
	}
	r := &Response{Status: status, Text: text}
	op.Responses = append(op.Responses, r)
	sort.Slice(op.Responses, func(i, j int) bool { return op.Responses[i].Status < op.Responses[j].Status })
	return r
}

func (u *URLAttrs) mergePathParams(values []string) {
	for i, v := range values {
		if i >= len(u.PathParams) {
			u.PathParams = append(u.PathParams, &Param{Name: strings.Trim(paramName(i+1), "{}"), Required: true})
		}
		p := u.PathParams[i]
		p.Seen++
		p.Type = mergeType(p.Type, scalarType(v), p.Seen == 1)
		addExample(&p.Examples, v)
	}
}

// mergeParams merges one request's fields into an operation's parameters,
// keeping first-seen order. requests is the operation's request count,
// including this one.
func mergeParams(list *[]*Param, fields []Field, requests int) {
	if len(fields) > 0 {
		byName := make(map[string]*Param, len(*list))
		for _, p := range *list {
			byName[p.Name] = p
		}
		seen := make(map[string]bool, len(fields))
		for _, f := range fields {
			p := byName[f.Name]
			if p == nil {
				p = &Param{Name: f.Name}
				byName[f.Name] = p
				*list = append(*list, p)
			}
			if !seen[f.Name] { // a repeated query name counts once per request
				seen[f.Name] = true
				p.Seen++
			}
			p.Type = mergeType(p.Type, f.Type, p.Type == "")
			if f.Value != "" {
				addExample(&p.Examples, f.Value)
			}
		}
	}
	for _, p := range *list {
		p.Required = p.Seen == requests
	}
}

// mergeType combines the type already inferred for a parameter with that of
// a new value: integers widen to numbers, and a parameter seen with
// different kinds of value is a string. null says nothing about the type.
func mergeType(have, next string, first bool) string {
	switch {
	case first || have == "" || have == "null":
		return next
	case next == "null" || have == next:
		return have
	case have == "integer" && next == "number", have == "number" && next == "integer":
		return "number"
	}
	return "string"
}

func (u *URLAttrs) addKind(k Kind) {
	for _, have := range u.Kinds {
		if have == k {
			return
		}
	}
	u.Kinds = append(u.Kinds, k)
}

func addExample(list *[]string, v string) {
	if len(*list) >= maxExamples {
		return
	}
	addUnique(list, v)
}

func addUnique(list *[]string, v string) {
	for _, have := range *list {
		if have == v {
			return
		}
	}
	*list = append(*list, v)
}

var methodOrder = map[string]int{"GET": 0, "HEAD": 1, "POST": 2, "PUT": 3, "PATCH": 4, "DELETE": 5, "OPTIONS": 6}

func methodLess(a, b string) bool {
	ra, oka := methodOrder[a]
	rb, okb := methodOrder[b]
	switch {
	case oka && okb:
		return ra < rb
	case oka != okb:
		return oka
	}
	return a < b
}
