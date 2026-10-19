package entities

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Found is one URL a message refers to, and where in the message it was.
type Found struct {
	URL *url.URL
	// Detail says where it was found: "html:src", "css", "js:fetch",
	// "json:$.links[0].href", "robots:disallow", "sitemap",
	// "openapi:GET,POST /users/{id}", "header:location", "header:csp"...
	Detail string
}

var (
	// HTML attributes that name another resource.
	attrRe = regexp.MustCompile(`(?i)\b(href|src|action|formaction|data-url|data-src|poster|manifest|cite|background|srcset)\s*=\s*["']([^"'<>]+)["']`)
	// <meta http-equiv="refresh" content="0; url=...">.
	metaRefreshRe = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*["']?refresh["']?[^>]*content\s*=\s*["'][^"']*?url\s*=\s*([^"'\s>]+)`)
	// Absolute URLs anywhere in a text body.
	absRe = regexp.MustCompile(`https?://[A-Za-z0-9.\-:\[\]]+(?:/[^\s"'<>\\)` + "`" + `]*)?`)
	// Quoted root-relative paths in scripts and JSON, e.g. "/api/users".
	quotedPathRe = regexp.MustCompile(`["'` + "`" + `](/[A-Za-z0-9_\-.~%/]*[A-Za-z][A-Za-z0-9_\-.~%/]*)["'` + "`" + `]`)
	// Quoted protocol-relative URLs in scripts, e.g. "//cdn.example.com/x.js".
	quotedNetRe = regexp.MustCompile(`["'` + "`" + `](//[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+(?::\d+)?(?:/[^"'` + "`" + `\s]*)?)["'` + "`" + `]`)
	// CSS references: url(...) and @import "...".
	cssURLRe    = regexp.MustCompile(`(?i)url\(\s*["']?([^"')\s]+)["']?\s*\)`)
	cssImportRe = regexp.MustCompile(`(?i)@import\s+["']([^"']+)["']`)
	// The requests scripts make: fetch, axios, XMLHttpRequest.open, and
	// url: fields as $.ajax and friends take them.
	scriptCallRes = []struct {
		via string
		re  *regexp.Regexp
	}{
		{"fetch", regexp.MustCompile(`\bfetch\(\s*["'` + "`" + `]([^"'` + "`" + `\s]{1,1000})["'` + "`" + `]`)},
		{"axios", regexp.MustCompile(`\baxios(?:\.(?:get|post|put|patch|delete|head|options|request))?\(\s*["'` + "`" + `]([^"'` + "`" + `\s]{1,1000})["'` + "`" + `]`)},
		{"xhr", regexp.MustCompile(`\.open\(\s*["'][A-Za-z]+["']\s*,\s*["'` + "`" + `]([^"'` + "`" + `\s]{1,1000})["'` + "`" + `]`)},
		{"url", regexp.MustCompile(`\burl\s*:\s*["'` + "`" + `]([^"'` + "`" + `\s]{1,1000})["'` + "`" + `]`)},
	}
	robotsRe  = regexp.MustCompile(`(?im)^\s*(allow|disallow|sitemap)\s*:\s*(\S+)`)
	sitemapRe = regexp.MustCompile(`(?i)<loc>\s*([^<\s]+)\s*</loc>`)
	// headerLinkRe is one <url> of a Link header.
	headerLinkRe = regexp.MustCompile(`<([^>]+)>`)
	// jsonPathKeyRe is a member name JSONPath can write after a dot.
	jsonPathKeyRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	// specYAMLRe is the top-level key an OpenAPI or Swagger document in
	// YAML starts with; only text that has one is parsed as YAML.
	specYAMLRe = regexp.MustCompile(`(?m)^["']?(openapi|swagger)["']?\s*:`)
	// placeholderRe is a {name} segment of an API description's path.
	placeholderRe = regexp.MustCompile(`\{([^{}/]+)\}`)
	// looksLikePathRe is a root-relative path with a letter in it.
	looksLikePathRe = regexp.MustCompile(`^/[A-Za-z0-9_\-.~%/{}]*[A-Za-z][A-Za-z0-9_\-.~%/{}]*$`)
)

// linkable reports whether a response of this media type is read for the
// URLs it refers to: any text, script, JSON, YAML or XML.
func linkable(mt string) bool {
	switch {
	case strings.HasPrefix(mt, "text/"),
		strings.Contains(mt, "javascript"), strings.Contains(mt, "ecmascript"),
		mt == "application/json", strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "/json"),
		strings.HasSuffix(mt, "yaml"), mt == "application/vnd.oai.openapi",
		mt == "application/xml", mt == "application/xhtml+xml", mt == "application/rss+xml",
		mt == "application/atom+xml", mt == "application/manifest+json":
		return true
	}
	return false
}

// FindURLs returns every http(s) URL a response refers to, on any host,
// each once, with where it was found: its headers (Location, Link,
// Content-Security-Policy, CORS) and, for a body of media type mt, what
// that kind of document can point at. base is the URL the response came
// from; relative references resolve against it. At most limit are
// returned (0: maxLinksFound).
func FindURLs(mt string, body []byte, header http.Header, base *url.URL, limit int) []Found {
	if limit <= 0 {
		limit = maxLinksFound
	}
	c := collector{base: base, limit: limit, seen: map[string]bool{}}
	c.headers(header)
	if len(body) > 0 {
		c.body(mt, string(body))
	}
	return c.out
}

type collector struct {
	base  *url.URL
	limit int
	seen  map[string]bool
	out   []Found
}

func (c *collector) full() bool { return len(c.out) >= c.limit }

// add resolves raw against the base and keeps it if it is a new http(s) URL.
func (c *collector) add(raw, detail string) {
	if c.full() {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return // mailto:, javascript:, data:, blob: ...
	}
	abs := c.base.ResolveReference(u)
	if abs.Host == "" || strings.ContainsAny(abs.Host, "*{}$") {
		return
	}
	if !pathOK(abs.Path) {
		return
	}
	abs.Host = strings.ToLower(abs.Host)
	abs.Fragment, abs.RawFragment = "", ""
	key := abs.String()
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.out = append(c.out, Found{URL: abs, Detail: detail})
}

// addMarkup is add for a reference read out of HTML, where the value is
// escaped text: a page writes style="…url(&quot;#glow&quot;)" and
// href="/a?x=1&amp;y=2", so the character references are undone, and the
// quotes they turn out to be are stripped, before the value is read as a
// URL. Skipping that is what made a filter's url(&quot;#glow&quot;) into a
// URL of its own, and lost the real one in url(&#x27;/bg.webp&#x27;).
func (c *collector) addMarkup(raw, detail string) {
	c.add(unquote(html.UnescapeString(raw)), detail)
}

// unquote strips one matching pair of quotes from around a value.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// pathOK reports whether a path is one a URI can hold at all. RFC 3986
// excludes these characters from a URI, so a path that has one once its
// percent-encoding is undone was never a reference the page made: it is a
// piece of something else a pattern matched inside, as a quoted path
// matches "/%3e%3c/svg%3e" in the middle of a data:image/svg+xml URI.
// Braces are allowed, because an API description's paths are templated.
func pathOK(p string) bool {
	// What RFC 3986 appendix C excludes from a URI outright, bar the braces
	// an API description's templated paths are written with.
	const forbidden = "<>\"\\^|`"
	for _, r := range p {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(forbidden, r) {
			return false
		}
	}
	return true
}

func (c *collector) headers(h http.Header) {
	if h == nil {
		return
	}
	for _, name := range []string{"Location", "Content-Location"} {
		if v := h.Get(name); v != "" {
			c.add(v, "header:"+strings.ToLower(name))
		}
	}
	if v := h.Get("Refresh"); v != "" {
		if i := strings.Index(strings.ToLower(v), "url="); i >= 0 {
			c.add(strings.Trim(v[i+4:], `"' `), "header:refresh")
		}
	}
	for _, v := range h.Values("Link") {
		for _, m := range headerLinkRe.FindAllStringSubmatch(v, -1) {
			c.add(m[1], "header:link")
		}
	}
	for _, name := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		for _, v := range h.Values(name) {
			for _, src := range cspSources(v) {
				c.add(src, "header:csp")
			}
		}
	}
	if v := h.Get("Access-Control-Allow-Origin"); v != "" && v != "*" && v != "null" {
		c.add(v, "header:cors")
	}
}

// cspSources picks the hosts a Content-Security-Policy names: host sources
// with or without a scheme, written as URLs. Keywords, scheme-only sources,
// nonces, hashes and wildcard hosts say nothing about a particular host.
func cspSources(policy string) []string {
	var out []string
	for _, directive := range strings.Split(policy, ";") {
		fields := strings.Fields(directive)
		if len(fields) < 2 {
			continue
		}
		if name := strings.ToLower(fields[0]); name == "report-uri" || name == "report-to" {
			if name == "report-uri" {
				out = append(out, fields[1:]...)
			}
			continue
		}
		for _, src := range fields[1:] {
			if strings.HasPrefix(src, "'") || strings.Contains(src, "*") || strings.HasSuffix(src, ":") {
				continue
			}
			if !strings.Contains(src, "://") {
				if !strings.Contains(src, ".") {
					continue
				}
				src = "https://" + src
			}
			out = append(out, src)
		}
	}
	return out
}

// specType is the media type to read a body as: its own, unless that says
// nothing (missing, or a generic octet-stream, as API descriptions are
// often served) and the path names JSON or YAML.
func specType(mt, urlPath string) string {
	switch mt {
	case "", "application/octet-stream", "binary/octet-stream", "application/unknown":
		switch strings.ToLower(path.Ext(urlPath)) {
		case ".json":
			return "application/json"
		case ".yaml", ".yml":
			return "application/yaml"
		}
	}
	return mt
}

func (c *collector) body(mt, text string) {
	path := c.base.Path
	kind := bodyKind(mt)
	// Everything in an HTML document is escaped text, so a reference read
	// out of one is unescaped first; the same patterns run over a .css or
	// .js file, where a character reference means nothing and "&" is itself.
	ref := c.add
	if kind == "html" {
		ref = c.addMarkup
	}
	if (kind == "yaml" || kind == "text") && specYAMLRe.MatchString(text) {
		var doc any
		if yaml.Unmarshal([]byte(text), &doc) == nil {
			doc = jsonish(doc)
			if d, ok := doc.(map[string]any); ok && (d["openapi"] != nil || d["swagger"] != nil) {
				c.openAPI(doc)
				c.walkJSON(doc, "$", 0)
				return
			}
		}
	}
	if path == "/robots.txt" {
		for _, m := range robotsRe.FindAllStringSubmatch(text, c.limit) {
			p := m[2]
			if star := strings.IndexByte(p, '*'); star >= 0 {
				p = p[:star]
			}
			p = strings.TrimSuffix(p, "$")
			if p != "" && p != "/" {
				c.add(p, "robots:"+strings.ToLower(m[1]))
			}
		}
	}
	if kind == "xml" || strings.HasSuffix(path, ".xml") {
		for _, m := range sitemapRe.FindAllStringSubmatch(text, c.limit) {
			c.add(html.UnescapeString(m[1]), "sitemap")
		}
	}
	if kind == "json" {
		var doc any
		if json.Unmarshal([]byte(text), &doc) == nil {
			c.openAPI(doc)
			c.walkJSON(doc, "$", 0)
			return // every string in it has been looked at
		}
	}
	if kind == "html" {
		for _, m := range metaRefreshRe.FindAllStringSubmatch(text, c.limit) {
			c.addMarkup(m[1], "html:refresh")
		}
		for _, m := range attrRe.FindAllStringSubmatch(text, -1) {
			if c.full() {
				return
			}
			attr := strings.ToLower(m[1])
			if attr == "srcset" {
				for _, cand := range strings.Split(m[2], ",") {
					if f := strings.Fields(cand); len(f) > 0 {
						c.addMarkup(f[0], "html:srcset")
					}
				}
				continue
			}
			c.addMarkup(m[2], "html:"+attr)
		}
	}
	if kind == "html" || kind == "css" {
		for _, m := range cssURLRe.FindAllStringSubmatch(text, c.limit) {
			ref(m[1], "css")
		}
		for _, m := range cssImportRe.FindAllStringSubmatch(text, c.limit) {
			ref(m[1], "css")
		}
	}
	if kind == "html" || kind == "js" {
		for _, sc := range scriptCallRes {
			for _, m := range sc.re.FindAllStringSubmatch(text, c.limit) {
				u := m[1]
				if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "./") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
					c.add(u, "js:"+sc.via)
				}
			}
		}
	}
	if kind == "js" || kind == "json" {
		for _, m := range quotedNetRe.FindAllStringSubmatch(text, c.limit) {
			c.add(m[1], kind)
		}
		for _, m := range quotedPathRe.FindAllStringSubmatch(text, c.limit) {
			if !strings.HasPrefix(m[1], "//") {
				c.add(m[1], kind)
			}
		}
	}
	// Last, absolute URLs anywhere in the text.
	for _, u := range absRe.FindAllString(text, c.limit) {
		ref(u, kind)
	}
}

// bodyKind names the kind of document mt is, for Found.Detail.
func bodyKind(mt string) string {
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return "html"
	case strings.Contains(mt, "javascript") || strings.Contains(mt, "ecmascript"):
		return "js"
	case mt == "text/css":
		return "css"
	case strings.HasSuffix(mt, "json"):
		return "json"
	case strings.HasSuffix(mt, "xml"):
		return "xml"
	case strings.HasSuffix(mt, "yaml"), mt == "application/vnd.oai.openapi":
		return "yaml"
	}
	return "text"
}

// jsonish turns a decoded YAML document into the shapes JSON decodes to:
// maps keyed by strings (YAML allows any key, as in a responses map's
// 200:), so the same walks read both.
func jsonish(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = jsonish(x)
		}
		return t
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(k)] = jsonish(x)
		}
		return out
	case []any:
		for i, x := range t {
			t[i] = jsonish(x)
		}
		return t
	}
	return v
}

// walkJSON looks at every string in a JSON document, keeping those that
// look like URLs with their JSONPath.
func (c *collector) walkJSON(v any, at string, depth int) {
	if depth > 32 || c.full() {
		return
	}
	switch t := v.(type) {
	case string:
		if looksLikeURL(t) {
			c.add(t, "json:"+at)
		}
	case []any:
		for i, x := range t {
			c.walkJSON(x, at+"["+strconv.Itoa(i)+"]", depth+1)
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			child := at + "[" + strconv.Quote(k) + "]"
			if jsonPathKeyRe.MatchString(k) {
				child = at + "." + k
			}
			c.walkJSON(t[k], child, depth+1)
		}
	}
}

// looksLikeURL: an absolute http(s) URL, or a root-relative path with a
// letter in it.
func looksLikeURL(v string) bool {
	if len(v) > 2048 || strings.ContainsAny(v, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return len(v) > len("https://")
	}
	return looksLikePathRe.MatchString(v) && !strings.HasPrefix(v, "//")
}

// openAPI lists the operations of an OpenAPI 3 or Swagger 2 document. A
// templated path whose parameters all have an example (or a default, an
// enum, or an integer type to try 1 with) is also given as a concrete URL,
// with " example" after its detail, so it can be requested.
func (c *collector) openAPI(doc any) {
	d, ok := doc.(map[string]any)
	if !ok || (d["openapi"] == nil && d["swagger"] == nil) {
		return
	}
	paths, ok := d["paths"].(map[string]any)
	if !ok {
		return
	}
	root := ""
	if servers, ok := d["servers"].([]any); ok && len(servers) > 0 {
		if s, ok := servers[0].(map[string]any); ok {
			root, _ = s["url"].(string)
		}
	} else if host, ok := d["host"].(string); ok {
		scheme := c.base.Scheme
		if schemes, ok := d["schemes"].([]any); ok && len(schemes) > 0 {
			if s, ok := schemes[0].(string); ok {
				scheme = s
			}
		}
		basePath, _ := d["basePath"].(string)
		root = scheme + "://" + host + basePath
	} else if basePath, ok := d["basePath"].(string); ok {
		root = basePath
	}
	if root == "" {
		root = "/"
	}
	prefix := root
	if u, err := url.Parse(root); err == nil {
		prefix = c.base.ResolveReference(u).String()
	}
	prefix = strings.TrimSuffix(prefix, "/")
	for _, p := range sortedKeys(paths) {
		var methods []string
		if item, ok := paths[p].(map[string]any); ok {
			for _, m := range []string{"get", "head", "post", "put", "patch", "delete", "options", "trace"} {
				if item[m] != nil {
					methods = append(methods, strings.ToUpper(m))
				}
			}
		}
		detail := "openapi:?"
		if len(methods) > 0 {
			detail = "openapi:" + strings.Join(methods, ",")
		}
		key := p
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		c.add(prefix+p, detail+" "+p)
		if item, ok := paths[key].(map[string]any); ok && placeholderRe.MatchString(p) {
			if concrete, ok := fillPath(d, item, p); ok {
				c.add(prefix+concrete, detail+" "+p+" example")
			}
		}
	}
}

// fillPath puts example values in a templated path's {name} segments, from
// the parameters the path item and its operations declare (GET's first).
// ok is false when one has no value to try.
func fillPath(doc, item map[string]any, p string) (string, bool) {
	var params []any
	for _, m := range []string{"get", "head", "post", "put", "patch", "delete", "options", "trace"} {
		if op, ok := item[m].(map[string]any); ok {
			if list, ok := op["parameters"].([]any); ok {
				params = append(params, list...)
			}
		}
	}
	if list, ok := item["parameters"].([]any); ok {
		params = append(params, list...)
	}
	values := map[string]string{}
	for _, raw := range params {
		prm, ok := resolveRef(doc, raw).(map[string]any)
		if !ok || prm["in"] != "path" {
			continue
		}
		name, _ := prm["name"].(string)
		if _, done := values[name]; done || name == "" {
			continue
		}
		if v, ok := exampleOf(doc, prm); ok {
			values[name] = v
		}
	}
	ok := true
	out := placeholderRe.ReplaceAllStringFunc(p, func(seg string) string {
		v, have := values[seg[1:len(seg)-1]]
		if !have {
			ok = false
			return seg
		}
		return url.PathEscape(v)
	})
	return out, ok
}

// exampleOf is a value to try for a parameter: its example, a Swagger
// x-example, the first of its examples, its schema's example, default or
// first enum value, or 1 for an integer.
func exampleOf(doc, prm map[string]any) (string, bool) {
	schema, _ := resolveRef(doc, prm["schema"]).(map[string]any)
	if schema == nil {
		schema = prm // Swagger 2 puts type, default and enum on the parameter
	}
	if v, ok := scalar(prm["example"]); ok {
		return v, true
	}
	if v, ok := scalar(prm["x-example"]); ok {
		return v, true
	}
	if ex, ok := prm["examples"].(map[string]any); ok {
		for _, k := range sortedKeys(ex) {
			if e, ok := resolveRef(doc, ex[k]).(map[string]any); ok {
				if v, ok := scalar(e["value"]); ok {
					return v, true
				}
			}
		}
	}
	for _, k := range []string{"example", "default"} {
		if v, ok := scalar(schema[k]); ok {
			return v, true
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		if v, ok := scalar(enum[0]); ok {
			return v, true
		}
	}
	if t, _ := schema["type"].(string); t == "integer" || t == "number" {
		return "1", true
	}
	return "", false
}

// scalar writes a string, number or boolean as a path segment.
func scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// resolveRef follows a local $ref ("#/components/parameters/id"), as
// many as eight deep; anything else is returned as it is.
func resolveRef(doc map[string]any, v any) any {
	for range 8 {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return v
		}
		var at any = doc
		for _, part := range strings.Split(ref[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next, ok := at.(map[string]any)
			if !ok {
				return nil
			}
			at = next[part]
		}
		v = at
	}
	return v
}
