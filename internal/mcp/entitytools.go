package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"reproxy/internal/entities"
)

// The entity tools expose the engagement's Entity Explorer (package
// entities): the domains, hosts and URLs requested or discovered on the
// targets. Like the findings tools they act on the local process's store,
// because the Explorer belongs to one engagement.

// entityOut is an entity as the model sees it.
type entityOut struct {
	Type     string            `json:"type"`
	Key      string            `json:"key"`
	Parent   string            `json:"parent,omitempty"`
	Scheme   string            `json:"scheme,omitempty"`
	Host     string            `json:"host"`
	Path     string            `json:"path,omitempty"`
	TargetID string            `json:"target_id,omitempty"`
	Origin   string            `json:"origin"`
	Visited  bool              `json:"visited"`
	Hits     int               `json:"hits"`
	KV       map[string]string `json:"kv,omitempty"`

	// A domain's.
	Suffix string `json:"suffix,omitempty"`
	// A host's.
	Port    int      `json:"port,omitempty"`
	IP      bool     `json:"ip,omitempty"`
	Servers []string `json:"servers,omitempty"`
	// Addresses are what a name host resolves to (each a host of its own).
	Addresses []string `json:"addresses,omitempty"`
	// A URL's.
	URL        string         `json:"url,omitempty"`
	Kinds      []string       `json:"kinds,omitempty"`
	Methods    []string       `json:"methods,omitempty"`
	Operations []operationOut `json:"operations,omitempty"`
	Examples   []string       `json:"examples,omitempty"`
	LinkedFrom []string       `json:"linked_from,omitempty"`
	Flows      []uint64       `json:"flows,omitempty"`
	// What sits under a domain or host, by key.
	Children []string `json:"children,omitempty"`
}

type operationOut struct {
	Method    string   `json:"method"`
	Requests  int      `json:"requests"`
	Query     []string `json:"query,omitempty"`
	Body      []string `json:"body,omitempty"`
	Responses []int    `json:"responses,omitempty"`
}

func summarizeEntity(store EntityStore, e entities.Entity, full bool) entityOut {
	out := entityOut{
		Type: string(e.Type), Key: e.Key, Parent: e.Parent, Scheme: e.Scheme, Host: e.Host, Path: e.Path,
		TargetID: e.TargetID, Origin: e.Origin, Visited: e.Visited(), Hits: e.Hits,
	}
	if len(e.KV) > 0 {
		out.KV = make(map[string]string, len(e.KV))
		for _, p := range e.KV {
			out.KV[p.Key] = p.Value
		}
	}
	switch {
	case e.DomainAttrs != nil:
		out.Suffix = e.DomainAttrs.Suffix
	case e.HostAttrs != nil:
		out.Port, out.IP, out.Servers = e.HostAttrs.Port, e.HostAttrs.IP, e.HostAttrs.Servers
		out.Addresses = e.HostAttrs.Addresses
	case e.URLAttrs != nil:
		out.URL = e.Label()
		for _, k := range e.URLAttrs.Kinds {
			out.Kinds = append(out.Kinds, string(k))
		}
		for _, op := range e.URLAttrs.Operations {
			out.Methods = append(out.Methods, op.Method)
		}
	}
	if !full {
		return out
	}
	for _, op := range e.Operations() {
		o := operationOut{Method: op.Method, Requests: op.Requests}
		for _, p := range op.Query {
			o.Query = append(o.Query, p.Name)
		}
		for _, p := range op.Body {
			o.Body = append(o.Body, p.Name)
		}
		for _, r := range op.Responses {
			o.Responses = append(o.Responses, r.Status)
		}
		out.Operations = append(out.Operations, o)
	}
	if e.URLAttrs != nil {
		out.Examples = e.URLAttrs.Examples
		out.Flows = e.FlowIDs()
	} else {
		for _, c := range store.Children(e.Key) {
			out.Children = append(out.Children, c.Key)
		}
	}
	out.LinkedFrom = e.LinkedFrom()
	return out
}

var listEntities = &Tool{
	Name: "list_entities",
	Description: "List the entities in the engagement's Entity Explorer: the domains (example.com), hosts " +
		"(origins such as https://api.example.com, or an IP address that origins sit under) and URLs " +
		"requested or discovered on the targets. URLs are keyed by scheme, host and templated path " +
		"(/users/12 and /users/13 are one URL, /users/{id}). Each entity sits under its parent: a URL " +
		"under its host, a host under its domain. Use it to see the application's surface; get_entity " +
		"has the detail of one.",
	Schema: object(map[string]any{
		"type":    strField("Keep only entities of this type: domain, host or url."),
		"target":  strField("Keep only entities first seen under this target id."),
		"host":    strField("Keep only entities on this host (a domain's name, a host's name[:port])."),
		"parent":  strField("Keep only the entities directly under this key; \"\" is not a filter."),
		"visited": boolField("true keeps only entities traffic reached; false only those merely discovered."),
		"limit":   intField("The most entities to return; the default is 200."),
	}),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Type    string `json:"type"`
			Target  string `json:"target"`
			Host    string `json:"host"`
			Parent  string `json:"parent"`
			Visited *bool  `json:"visited"`
			Limit   int    `json:"limit"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Type != "" && !entities.Type(a.Type).Valid() {
			return nil, fmt.Errorf("type %q must be domain, host or url", a.Type)
		}
		store, err := r.entityStore()
		if err != nil {
			return nil, err
		}
		list := store.ByTarget(a.Target)
		limit := a.Limit
		if limit <= 0 {
			limit = 200
		}
		out := make([]entityOut, 0, min(len(list), limit))
		matched := 0
		for _, e := range list {
			switch {
			case a.Type != "" && string(e.Type) != a.Type:
			case a.Host != "" && !strings.EqualFold(e.Host, a.Host):
			case a.Parent != "" && e.Parent != a.Parent:
			case a.Visited != nil && e.Visited() != *a.Visited:
			default:
				matched++
				if len(out) < limit {
					out = append(out, summarizeEntity(store, e, false))
				}
			}
		}
		return map[string]any{"entities": out, "matched": matched, "more": matched > len(out)}, nil
	},
}

var getEntity = &Tool{
	Name: "get_entity",
	Description: "Get one entity in full. For a URL: its methods, their parameters and responses, the " +
		"concrete paths seen, where it was linked from, and the ids of the flows made to it (for get_flow, " +
		"or list_flows with entity). For a domain or host: what sits under it. Every entity also has its " +
		"key/value store. Identify it by its key (from list_entities) or, for a URL, any URL on it.",
	Schema: object(map[string]any{
		"key": strField("The entity key, e.g. \"example.com\", \"https://app.test\" or \"https://app.test/users/{id}\"."),
		"url": strField("Any URL on a URL entity, e.g. \"https://app.test/users/12\"; it is templated to find it."),
	}),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Key string `json:"key"`
			URL string `json:"url"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		store, err := r.entityStore()
		if err != nil {
			return nil, err
		}
		e, ok := resolveEntity(store, a.Key, a.URL)
		if !ok {
			return nil, fmt.Errorf("no entity %q in the Explorer", firstNonEmpty(a.Key, a.URL))
		}
		return map[string]any{"entity": summarizeEntity(store, e, true)}, nil
	},
}

var addURL = &Tool{
	Name: "add_url",
	Description: "Put a URL in the Entity Explorer as discovered, for one you found that the traffic " +
		"hasn't reached yet. It is templated like the rest (/users/12 becomes /users/{id}), merges with " +
		"a URL already there, and brings its host and domain. Recorded as discovered by the agent.",
	Writes: true,
	Schema: object(map[string]any{
		"url":    strField("The absolute http(s) URL to add. Required."),
		"flow":   strField("The flow it was found in, as a flow handle from list_flows."),
		"detail": strField("Where it was found, e.g. \"in the openapi document\"."),
	}, "url"),
	run: func(_ context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			URL    string `json:"url"`
			Flow   string `json:"flow"`
			Detail string `json:"detail"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		store, err := r.entityStore()
		if err != nil {
			return nil, err
		}
		if !store.Recording() {
			return nil, entities.ErrNotRecording
		}
		u, err := url.Parse(a.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("%q is not an absolute http(s) URL", a.URL)
		}
		d := entities.Discovery{
			Address: entities.AddressOf(u.Scheme, u.Host, u.Path),
			Link:    entities.Link{Source: "agent", Detail: a.Detail},
		}
		if a.Flow != "" {
			if _, flowID, err := r.localFlow(a.Flow); err == nil {
				d.Link.FromFlowID = flowID
			}
		}
		e, created, err := store.Discover(d)
		if err != nil && e.Key == "" {
			return nil, err
		}
		return map[string]any{"entity": summarizeEntity(store, e, true), "created": created}, nil
	},
}

var setEntityKV = &Tool{
	Name: "set_entity_kv",
	Description: "Set or remove keys in an entity's key/value store: structured notes kept on a domain, " +
		"host or URL (a WAF seen in front of a host, the role a URL needs). Values are strings.",
	Writes: true,
	Schema: object(map[string]any{
		"key":   strField("The entity key (from list_entities). Required."),
		"set":   map[string]any{"type": "object", "description": "Keys to set, with their values.", "additionalProperties": map[string]any{"type": "string"}},
		"unset": map[string]any{"type": "array", "description": "Keys to remove.", "items": map[string]any{"type": "string"}},
	}, "key"),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Key   string            `json:"key"`
			Set   map[string]string `json:"set"`
			Unset []string          `json:"unset"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		store, err := r.entityStore()
		if err != nil {
			return nil, err
		}
		if !store.Recording() {
			return nil, entities.ErrNotRecording
		}
		e, ok := store.Get(a.Key)
		if !ok {
			return nil, fmt.Errorf("no entity %q in the Explorer", a.Key)
		}
		pairs := append([]entities.Pair(nil), e.KV...)
		drop := map[string]bool{}
		for _, k := range a.Unset {
			drop[k] = true
		}
		kept := pairs[:0]
		for _, p := range pairs {
			if v, ok := a.Set[p.Key]; ok {
				p.Value = v
				delete(a.Set, p.Key)
			}
			if !drop[p.Key] {
				kept = append(kept, p)
			}
		}
		for k, v := range sortedPairs(a.Set) {
			kept = append(kept, entities.Pair{Key: k, Value: v})
		}
		e, err = store.ReplaceKV(a.Key, kept)
		if err != nil {
			return nil, err
		}
		return map[string]any{"entity": summarizeEntity(store, e, false)}, nil
	},
}

// sortedPairs ranges over m in key order, so new keys are added in a
// stable order.
func sortedPairs(m map[string]string) func(yield func(string, string) bool) {
	return func(yield func(string, string) bool) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}

// resolveEntity finds an entity by key (a bare host name or address finds
// its name host, //name, when no domain has that key), or the URL entity a
// concrete URL falls under.
func resolveEntity(store EntityStore, key, rawURL string) (entities.Entity, bool) {
	if key != "" {
		if e, ok := store.Get(key); ok {
			return e, true
		}
		if !strings.Contains(key, "/") {
			if e, ok := store.Get(entities.HostKey("", key)); ok {
				return e, true
			}
		}
	}
	if rawURL != "" {
		if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
			return store.Lookup(u.Scheme, u.Host, u.Path)
		}
	}
	return entities.Entity{}, false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
