package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Match narrows the events a target's function receives. Every field that
// is set must match; within a field, any entry may. It is compiled once, when
// the plugin loads, into a Matcher that core evaluates in Go — the plugin's
// code never runs for an event its filter turns away.
type Match struct {
	// Scope is "any" (the default), "in_scope" (a flow some target covers,
	// or any flow while no targets are configured) or "out_of_scope".
	Scope string `json:"scope,omitempty"`
	// Targets limits the match to these target IDs.
	Targets []string `json:"targets,omitempty"`
	// Hosts are host patterns: "app.test", "*.app.test" (subdomains only),
	// or any glob with * in it. Ports are ignored.
	Hosts []string `json:"hosts,omitempty"`
	// Paths are globs over the path, without the query: "/api/*". On a file
	// hook they are globs over the recovered file's path, which is relative
	// to the engagement's recovered files and so has no leading slash:
	// "app.test/*", "*.ts".
	Paths []string `json:"paths,omitempty"`
	// PathRegex is a regular expression (RE2) over the path and query.
	PathRegex string `json:"path_regex,omitempty"`
	// Methods are HTTP methods.
	Methods []string `json:"methods,omitempty"`
	// Status are response codes: "404", "2xx", "500-599".
	Status []string `json:"status,omitempty"`
	// ContentTypes are media type globs: "application/json", "text/*",
	// "application/*+json". Request hooks test the request's Content-Type,
	// the rest the response's.
	ContentTypes []string `json:"content_types,omitempty"`
	// Sources are where a flow came from: proxy, replay, agent, crawl, plugin
	// (default: all but plugin), or who recorded a finding: user, agent,
	// plugin (default: all).
	Sources []string `json:"sources,omitempty"`
	// Types are finding type IDs, for an entity hook entity types (domain,
	// host, url), or for a file hook the kind a recovered file carries:
	// "source-map", "source-map-dependency", or whatever the plugin that
	// recovered it labelled it.
	Types []string `json:"types,omitempty"`
	// Directions are WebSocket message directions: "sent" (client→server),
	// "received" (server→client). It filters websocket message hooks only.
	Directions []string `json:"directions,omitempty"`
}

// Facts is what a Matcher tests: the parts of a flow, finding or entity
// it can filter on. Callers fill in what their subject has.
type Facts struct {
	Source    string // proxy, replay, agent, crawl, plugin; user for a finding
	TargetID  string
	NoTargets bool // no targets are configured, so everything is in scope
	Method    string
	Status    int
	Host      string // any case, may carry a port
	Path      string // the request-target: path and query
	// Head is the message whose Content-Type a content_types filter tests:
	// its raw bytes, of which only the headers are read. ContentType, when
	// set, is used instead.
	Head        []byte
	ContentType string
	// Direction is a WebSocket message's direction: "sent" or "received".
	Direction string
	Types     []string // a finding's type IDs, or an entity's type
}

// Matcher is a compiled Match.
type Matcher struct {
	scope      string
	targets    []string
	hosts      []glob
	paths      []glob
	pathRe     *regexp.Regexp
	methods    []string
	status     [][2]int
	types      []glob
	sources    []string
	anySource  bool
	findings   []string
	directions []string
}

// Flow sources, as plugin.json names them.
var flowSources = []string{"proxy", "replay", "agent", "crawl", "plugin"}

// actionInfo is what an action's filter is compiled against: a flow, its
// response's content type.
var actionInfo = HookInfo{Hook: "action", Class: Observe, Subject: SubjectFlow}

// CompileAction compiles an action's filter (see Action.Match). It is a
// traffic hook's, except that with no sources given it accepts every
// source: the user picked the flow, plugins' own included.
func (m *Match) CompileAction() (*Matcher, error) {
	c, err := m.Compile(actionInfo)
	if err != nil {
		return nil, err
	}
	if m == nil || len(m.Sources) == 0 {
		c.sources = flowSources
	}
	return c, nil
}

// Compile checks the filter against the hook it is on and compiles it. A
// nil Match compiles to a Matcher with only the hook's defaults.
func (m *Match) Compile(info HookInfo) (*Matcher, error) {
	if m == nil {
		m = &Match{}
	}
	c := &Matcher{scope: m.Scope}
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	not := func(field string, set bool) {
		if set {
			bad("%s doesn't apply to this hook", field)
		}
	}

	switch m.Scope {
	case "", "any", "in_scope", "out_of_scope":
	default:
		bad("scope %q must be any, in_scope or out_of_scope", m.Scope)
	}
	c.targets = m.Targets
	for _, h := range m.Hosts {
		g, err := compileGlob(strings.ToLower(h), true)
		if err != nil {
			bad("hosts: %v", err)
		}
		c.hosts = append(c.hosts, g)
	}
	for _, p := range m.Paths {
		// A recovered file's path is relative to the recovered files
		// directory, so only a request path has to start at the root.
		if info.Subject != SubjectFile && !strings.HasPrefix(p, "/") && p != "*" {
			bad("paths: %q must start with /", p)
		}
		g, err := compileGlob(p, false)
		if err != nil {
			bad("paths: %v", err)
		}
		c.paths = append(c.paths, g)
	}
	if m.PathRegex != "" {
		re, err := regexp.Compile(m.PathRegex)
		if err != nil {
			bad("path_regex: %v", err)
		}
		c.pathRe = re
	}
	for _, method := range m.Methods {
		c.methods = append(c.methods, strings.ToUpper(method))
	}
	for _, s := range m.Status {
		r, err := parseStatus(s)
		if err != nil {
			bad("status: %v", err)
		}
		c.status = append(c.status, r)
	}
	for _, t := range m.ContentTypes {
		g, err := compileGlob(strings.ToLower(t), false)
		if err != nil {
			bad("content_types: %v", err)
		}
		c.types = append(c.types, g)
	}

	switch info.Subject {
	case SubjectFlow:
		not("types", len(m.Types) > 0)
		c.sources = m.Sources
		if len(c.sources) == 0 {
			c.sources = flowSources[:4] // other plugins' traffic only on request
		}
		for _, s := range c.sources {
			if !contains(flowSources, s) {
				bad("sources: %q must be one of %s", s, strings.Join(flowSources, ", "))
			}
		}
	case SubjectFinding:
		not("scope", m.Scope != "" && m.Scope != "any")
		not("targets", len(m.Targets) > 0)
		not("hosts", len(m.Hosts) > 0)
		not("paths", len(m.Paths) > 0)
		not("path_regex", m.PathRegex != "")
		not("methods", len(m.Methods) > 0)
		not("status", len(m.Status) > 0)
		not("content_types", len(m.ContentTypes) > 0)
		c.findings = m.Types
		c.sources = m.Sources
		c.anySource = len(m.Sources) == 0
		for _, s := range c.sources {
			if s != "user" && s != "agent" && s != "plugin" {
				bad("sources: %q must be user, agent or plugin", s)
			}
		}
	case SubjectEntity:
		not("methods", len(m.Methods) > 0)
		not("status", len(m.Status) > 0)
		not("content_types", len(m.ContentTypes) > 0)
		not("sources", len(m.Sources) > 0)
		not("directions", len(m.Directions) > 0)
		c.anySource = true
		// types picks entity types here: domain, host or url.
		c.findings = m.Types
		for _, t := range m.Types {
			if t != "domain" && t != "host" && t != "url" {
				bad("types: %q must be domain, host or url", t)
			}
		}
	case SubjectFile:
		not("scope", m.Scope != "" && m.Scope != "any")
		not("targets", len(m.Targets) > 0)
		not("methods", len(m.Methods) > 0)
		not("status", len(m.Status) > 0)
		not("path_regex", m.PathRegex != "")
		not("directions", len(m.Directions) > 0)
		// types picks the kind a recovered file carries, and content_types
		// its media type — worked out from the name and the bytes, so a
		// scanner can ask for text/* and never be handed a font.
		c.findings = m.Types
		c.sources = m.Sources
		c.anySource = len(m.Sources) == 0
		for _, s := range c.sources {
			if s != "user" && s != "agent" && s != "plugin" {
				bad("sources: %q must be user, agent or plugin", s)
			}
		}
	case SubjectWebSocket:
		not("methods", len(m.Methods) > 0)
		not("status", len(m.Status) > 0)
		not("content_types", len(m.ContentTypes) > 0)
		not("types", len(m.Types) > 0)
		c.sources = m.Sources
		if len(c.sources) == 0 {
			c.sources = flowSources[:3]
		}
		for _, s := range c.sources {
			if !contains(flowSources, s) {
				bad("sources: %q must be one of %s", s, strings.Join(flowSources, ", "))
			}
		}
		for _, d := range m.Directions {
			if d != "sent" && d != "received" {
				bad("directions: %q must be sent or received", d)
			}
		}
		c.directions = m.Directions
	default:
		c.anySource = true
	}
	if len(errs) > 0 {
		return nil, errors.New("match: " + strings.Join(errs, "; "))
	}
	return c, nil
}

// Match reports whether the facts pass the filter. It tests the cheapest
// fields first and reads the Content-Type only when everything else passed.
func (c *Matcher) Match(f *Facts) bool {
	if !c.anySource && !contains(c.sources, f.Source) {
		return false
	}
	switch c.scope {
	case "in_scope":
		if f.TargetID == "" && !f.NoTargets {
			return false
		}
	case "out_of_scope":
		if f.TargetID != "" || f.NoTargets {
			return false
		}
	}
	if len(c.targets) > 0 && !contains(c.targets, f.TargetID) {
		return false
	}
	if len(c.methods) > 0 && !contains(c.methods, f.Method) {
		return false
	}
	if len(c.status) > 0 {
		ok := false
		for _, r := range c.status {
			if f.Status >= r[0] && f.Status <= r[1] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(c.findings) > 0 && !anyIn(c.findings, f.Types) {
		return false
	}
	if len(c.hosts) > 0 && !anyHost(c.hosts, hostOnly(f.Host)) {
		return false
	}
	if len(c.paths) > 0 {
		p := f.Path
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		if !anyGlob(c.paths, p) {
			return false
		}
	}
	if c.pathRe != nil && !c.pathRe.MatchString(f.Path) {
		return false
	}
	if len(c.types) > 0 && !anyGlob(c.types, f.contentType()) {
		return false
	}
	if len(c.directions) > 0 && f.Direction != "" && !contains(c.directions, f.Direction) {
		return false
	}
	return true
}

func (f *Facts) contentType() string {
	if f.ContentType != "" {
		ct := f.ContentType
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		return strings.ToLower(strings.TrimSpace(ct))
	}
	return ContentType(f.Head)
}

// WantsFileBatch reports whether the filter accepts a recovery as a whole
// (file.on_batch_complete). A batch has no path and no media type of its
// own, so a filter on either is about the files in it and cannot turn the
// batch away — the plugin reads the batch's files with files.list and
// filters them itself. Everything a batch does have — who recovered it,
// the host it came from, the kind its files carry — is matched as usual.
func (c *Matcher) WantsFileBatch(f *Facts) bool {
	if !c.anySource && !contains(c.sources, f.Source) {
		return false
	}
	if len(c.findings) > 0 && !anyIn(c.findings, f.Types) {
		return false
	}
	if len(c.hosts) > 0 && !anyHost(c.hosts, hostOnly(f.Host)) {
		return false
	}
	return true
}

// WantsSource reports whether the filter accepts a flow source, for the
// dispatcher's coarse routing.
func (c *Matcher) WantsSource(source string) bool {
	return c.anySource || contains(c.sources, source)
}

// ContentType returns the media type of a raw HTTP message (lower case,
// without parameters), reading only its headers. "" when there is none.
func ContentType(raw []byte) string {
	head := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		head = raw[:i]
	}
	const name = "content-type:"
	for len(head) > 0 {
		i := bytes.IndexByte(head, '\n')
		line := head
		if i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		if len(line) > len(name) && bytes.EqualFold(line[:len(name)], []byte(name)) {
			v := line[len(name):]
			if j := bytes.IndexByte(v, ';'); j >= 0 {
				v = v[:j]
			}
			return strings.ToLower(string(bytes.TrimSpace(v)))
		}
	}
	return ""
}

// hostOnly drops a port. Case is left alone: host patterns compare without
// it, so the hot path needn't allocate a lower-cased copy.
func hostOnly(host string) string {
	if strings.HasPrefix(host, "[") { // [::1]:443
		if i := strings.IndexByte(host, ']'); i >= 0 {
			return host[1:i]
		}
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	return host
}

// anyHost matches a host against host patterns, ignoring case.
func anyHost(gs []glob, host string) bool {
	for _, g := range gs {
		switch {
		case g.any:
			return true
		case g.exact != "":
			if strings.EqualFold(host, g.exact) {
				return true
			}
		case g.suffix != "":
			if len(host) > len(g.suffix) && strings.EqualFold(host[len(host)-len(g.suffix):], g.suffix) {
				return true
			}
		case g.re != nil:
			if g.re.MatchString(strings.ToLower(host)) {
				return true
			}
		}
	}
	return false
}

// glob is a compiled pattern: an exact string, a domain suffix, or a
// regular expression for anything with * elsewhere.
type glob struct {
	exact  string
	suffix string // "*.app.test": ".app.test"
	any    bool
	re     *regexp.Regexp
}

func compileGlob(p string, host bool) (glob, error) {
	switch {
	case p == "":
		return glob{}, errors.New("empty pattern")
	case p == "*":
		return glob{any: true}, nil
	case !strings.Contains(p, "*"):
		return glob{exact: p}, nil
	case host && strings.HasPrefix(p, "*.") && !strings.Contains(p[2:], "*"):
		return glob{suffix: p[1:]}, nil
	}
	parts := strings.Split(p, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	return glob{re: re}, err
}

func (g glob) match(s string) bool {
	switch {
	case g.any:
		return true
	case g.exact != "":
		return s == g.exact
	case g.suffix != "":
		return strings.HasSuffix(s, g.suffix)
	case g.re != nil:
		return g.re.MatchString(s)
	}
	return false
}

func anyGlob(gs []glob, s string) bool {
	for _, g := range gs {
		if g.match(s) {
			return true
		}
	}
	return false
}

func parseStatus(s string) ([2]int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 3 && s[0] >= '1' && s[0] <= '5' && s[1:] == "xx" {
		lo := int(s[0]-'0') * 100
		return [2]int{lo, lo + 99}, nil
	}
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || a > b || a < 100 || b > 599 {
			return [2]int{}, fmt.Errorf("%q is not a range of status codes", s)
		}
		return [2]int{a, b}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 100 || n > 599 {
		return [2]int{}, fmt.Errorf("%q is not a status code, a class like 2xx, or a range", s)
	}
	return [2]int{n, n}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func anyIn(want, have []string) bool {
	for _, h := range have {
		if contains(want, h) {
			return true
		}
	}
	return false
}
