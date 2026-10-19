package entities

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// found runs FindURLs and renders each result as "url detail".
func found(t *testing.T, mt, body, base string, h http.Header) []string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range FindURLs(mt, []byte(body), h, u, 0) {
		out = append(out, f.URL.String()+" "+f.Detail)
	}
	return out
}

func contains(t *testing.T, got []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing %q in %q", w, got)
		}
	}
}

// lacks fails if any of these URLs was found.
func lacks(t *testing.T, got []string, unwanted ...string) {
	t.Helper()
	for _, g := range got {
		u, _, _ := strings.Cut(g, " ")
		for _, w := range unwanted {
			if u == w {
				t.Errorf("kept %q in %q", g, got)
			}
		}
	}
}

func TestFindURLsInHTML(t *testing.T) {
	got := found(t, "text/html", `<a href="/about">a</a><script src="https://cdn.test/app.js"></script>
		<img srcset="/s.png 1x, //img.cdn.test/l.png 2x"><a href="mailto:x@y.test">m</a>
		<style>body{background:url('/bg.png')}</style><script>fetch("/api/me")</script>`, "https://app.test/", nil)
	contains(t, got,
		"https://app.test/about html:href",
		"https://cdn.test/app.js html:src",
		"https://app.test/s.png html:srcset",
		"https://img.cdn.test/l.png html:srcset",
		"https://app.test/bg.png css",
		"https://app.test/api/me js:fetch")
	for _, g := range got {
		if strings.HasPrefix(g, "mailto") {
			t.Errorf("kept %q", g)
		}
	}
}

func TestFindURLsInScripts(t *testing.T) {
	js := `fetch("/api/me"); axios.post('/api/orders', o); x.open("GET", "https://api.app.test/v1/q");
		$.ajax({url: "/legacy/thing"}); const cdn = "//static.cdn.test/lib.js";`
	contains(t, found(t, "application/javascript", js, "https://app.test/app.js", nil),
		"https://app.test/api/me js:fetch",
		"https://app.test/api/orders js:axios",
		"https://api.app.test/v1/q js:xhr",
		"https://app.test/legacy/thing js:url",
		"https://static.cdn.test/lib.js js")
}

func TestFindURLsInJSON(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"links": []any{map[string]any{"href": "/api/orders"}},
		"next":  "https://api.app.test/v2/page/2", "name": "x", "date": "/2024/01",
	})
	got := found(t, "application/json", string(body), "https://app.test/api", nil)
	contains(t, got, "https://app.test/api/orders json:$.links[0].href", "https://api.app.test/v2/page/2 json:$.next")
	if len(got) != 2 {
		t.Errorf("got %q, want two", got)
	}
}

func TestFindURLsInOpenAPI(t *testing.T) {
	doc := `{"openapi":"3.0.0","servers":[{"url":"https://api.app.test/v1"}],
		"paths":{"/users/{userId}":{"get":{},"delete":{}},"/health":{"get":{}}}}`
	u, _ := url.Parse("https://app.test/openapi.json")
	var got []string
	for _, f := range FindURLs("application/json", []byte(doc), nil, u, 0) {
		if strings.HasPrefix(f.Detail, "openapi:") {
			got = append(got, AddressOf(f.URL.Scheme, f.URL.Host, f.URL.Path).Key()+" "+f.Detail)
		}
	}
	want := []string{
		"https://api.app.test/v1/health openapi:GET /health",
		"https://api.app.test/v1/users/{id} openapi:GET,DELETE /users/{userId}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	swagger := `{"swagger":"2.0","host":"api.app.test","basePath":"/v2","schemes":["https"],"paths":{"/pets":{"post":{}}}}`
	contains(t, found(t, "application/json", swagger, "https://app.test/", nil), "https://api.app.test/v2/pets openapi:POST /pets")
}

func TestFindURLsInRobotsAndSitemaps(t *testing.T) {
	robots := "User-agent: *\nDisallow: /admin/\nAllow: /public/*.html\nSitemap: https://app.test/sitemap.xml\nDisallow: /"
	got := found(t, "text/plain", robots, "https://app.test/robots.txt", nil)
	contains(t, got, "https://app.test/admin/ robots:disallow", "https://app.test/public/ robots:allow",
		"https://app.test/sitemap.xml robots:sitemap")
	if g := found(t, "text/plain", "Disallow: /secret", "https://app.test/notes.txt", nil); len(g) != 0 {
		t.Errorf("read a text file as robots.txt: %q", g)
	}
	contains(t, found(t, "application/xml",
		"<urlset><url><loc>https://app.test/a?x=1&amp;y=2</loc></url><url><loc> https://app.test/b </loc></url></urlset>",
		"https://app.test/sitemap.xml", nil),
		"https://app.test/a?x=1&y=2 sitemap", "https://app.test/b sitemap")
}

func TestFindURLsInHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Location", "https://sso.test/login")
	h.Add("Link", `<https://fonts.cdn.test/f.woff2>; rel=preload, </next>; rel=next`)
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'nonce-x' https://js.cdn.test *.wild.test data:; img-src img.cdn.test; report-uri https://report.test/csp")
	h.Set("Access-Control-Allow-Origin", "https://partner.test")
	got := found(t, "", "", "https://app.test/", h)
	contains(t, got,
		"https://sso.test/login header:location",
		"https://fonts.cdn.test/f.woff2 header:link",
		"https://app.test/next header:link",
		"https://js.cdn.test header:csp",
		"https://img.cdn.test header:csp",
		"https://report.test/csp header:csp",
		"https://partner.test header:cors")
	for _, g := range got {
		if strings.Contains(g, "wild") || strings.Contains(g, "self") {
			t.Errorf("kept %q", g)
		}
	}
}

func TestFindURLsLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString(`<a href="/p` + string(rune('a'+i)) + `">x</a>`)
	}
	u, _ := url.Parse("https://app.test/")
	if n := len(FindURLs("text/html", []byte(b.String()), nil, u, 5)); n != 5 {
		t.Errorf("got %d, want 5", n)
	}
}

func TestParseFindsOtherHostsAndReferrals(t *testing.T) {
	f := flow(1, "GET", "app.test", "/", 302, "Content-Type: text/html\r\nLocation: https://sso.test/login\r\n",
		`<script src="https://cdn.test/app.js"></script><a href="/about">a</a>`)
	_, links, _ := Parse(f)
	var keys []string
	for _, l := range links {
		keys = append(keys, l.Address.Key()+" "+l.Link.Detail)
	}
	contains(t, keys, "https://sso.test/login header:location", "https://cdn.test/app.js html:src", "https://app.test/about html:href")

	g := flow(2, "GET", "cdn.test", "/app.js", 200, "Content-Type: application/javascript\r\n", "")
	g.Request.Raw = []byte("GET /app.js HTTP/1.1\r\nHost: cdn.test\r\nReferer: https://app.test/page?q=1\r\n\r\n")
	_, links, _ = Parse(g)
	if len(links) != 1 || links[0].Link.Source != OriginReferer || links[0].Link.FromURL != "https://app.test/page" ||
		links[0].Address.Key() != "https://cdn.test/app.js" {
		t.Errorf("referral: %+v", links)
	}
	g.Request.Raw = []byte("GET /app.js HTTP/1.1\r\nHost: cdn.test\r\nReferer: https://cdn.test/x\r\n\r\n")
	if _, links, _ = Parse(g); len(links) != 0 {
		t.Errorf("same-host referer recorded: %+v", links)
	}
}

// scopeOf covers the given hosts, each its own target ("t:" + host).
func scopeOf(hosts ...string) ScopeFunc {
	return func(h string) (string, bool) {
		for _, c := range hosts {
			if c == h {
				return "t:" + c, true
			}
		}
		return "", false
	}
}

func TestRelatedHosts(t *testing.T) {
	s := NewStore()
	var mu sync.Mutex
	var announced []string
	s.Subscribe(func(e Event) {
		if e.Kind == RelatedAdded {
			mu.Lock()
			announced = append(announced, e.Host)
			mu.Unlock()
		}
	})
	s.SetScope(scopeOf("app.test"))

	page := flow(1, "GET", "app.test", "/", 200, "Content-Type: text/html\r\n",
		`<script src="https://cdn.test:8443/app.js"></script><img src="http://10.0.0.5/x.png"><a href="/about">a</a>`)
	page.TargetID = "t:app.test"
	if _, err := s.Apply(visit(t, page)); err != nil {
		t.Fatal(err)
	}
	// Something an out-of-scope page links to is not related.
	other := flow(2, "GET", "elsewhere.test", "/", 200, "Content-Type: text/html\r\n", `<a href="https://unrelated.test/">x</a>`)
	s.Apply(visit(t, other))

	rel := s.Related()
	var hosts []string
	for _, r := range rel {
		hosts = append(hosts, r.Host)
		if !reflect.DeepEqual(r.FoundOn, []string{"app.test"}) || !reflect.DeepEqual(r.TargetIDs, []string{"t:app.test"}) {
			t.Errorf("%s found on %v / %v", r.Host, r.FoundOn, r.TargetIDs)
		}
		if r.IP != (r.Host == "10.0.0.5") {
			t.Errorf("%s: IP = %v", r.Host, r.IP)
		}
	}
	if !reflect.DeepEqual(sortedCopy(hosts), []string{"10.0.0.5", "cdn.test"}) {
		t.Errorf("related = %v", hosts)
	}
	if !reflect.DeepEqual(sortedCopy(announced), []string{"10.0.0.5", "cdn.test"}) {
		t.Errorf("announced = %v", announced)
	}
	// Linking to it again is not news.
	page.ID = 3
	s.Apply(visit(t, page))
	if len(announced) != 2 {
		t.Errorf("announced again: %v", announced)
	}

	// The Map leaves related hosts out, and an URL linked from a
	// target belongs to the target covering its own host.
	for _, e := range s.Mapped("") {
		if BareHost(e.Host) == "cdn.test" || e.Host == "10.0.0.5" {
			t.Errorf("related URL %s on the Explorer", e.Key)
		}
	}
	if e, _ := s.Get("https://cdn.test:8443/app.js"); e.TargetID != "" {
		t.Errorf("cdn URL under target %q", e.TargetID)
	}
	if n := len(s.OnHost("cdn.test")); n != 1 {
		t.Errorf("OnHost(cdn.test) = %d URLs", n)
	}

	// Promoting it: no longer related, and its URLs are mapped under
	// the new target.
	s.SetScope(scopeOf("app.test", "cdn.test"))
	for _, r := range s.Related() {
		if r.Host == "cdn.test" {
			t.Error("promoted host still related")
		}
	}
	got := s.Mapped("t:cdn.test")
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Key)
		if e.TargetID != "t:cdn.test" {
			t.Errorf("%s drawn under %q", e.Key, e.TargetID)
		}
	}
	if !reflect.DeepEqual(keys, []string{"cdn.test", "//cdn.test", "https://cdn.test:8443", "https://cdn.test:8443/app.js"}) {
		t.Errorf("Mapped(cdn) = %v", keys)
	}
	// Demoting it again doesn't announce it twice.
	s.SetScope(scopeOf("app.test"))
	if len(announced) != 2 {
		t.Errorf("announced again after demotion: %v", announced)
	}
	// The visited elsewhere.test is still on the Explorer under All.
	if _, ok := find(s.Mapped(""), "https://elsewhere.test/"); !ok {
		t.Error("visited out-of-scope host left off the Explorer")
	}
}

func TestRelatedFromReferral(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("app.test"))
	g := flow(1, "GET", "cdn.test", "/lib.js", 200, "Content-Type: application/javascript\r\n", "")
	g.Request.Raw = []byte("GET /lib.js HTTP/1.1\r\nHost: cdn.test\r\nReferer: https://app.test/\r\n\r\n")
	g.Started = time.Unix(10, 0)
	s.Apply(visit(t, g))
	rel := s.Related()
	if len(rel) != 1 || rel[0].Host != "cdn.test" {
		t.Errorf("related = %+v", rel)
	}
	e, _ := s.Get("https://cdn.test/lib.js")
	if e.Hits != 1 || len(e.Links) != 1 || e.Links[0].Source != OriginReferer {
		t.Errorf("URL = %+v", e)
	}
}

func TestReloadedRelatedIsNotNews(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	s := NewStore()
	s.SetScope(scopeOf("app.test"))
	if err := s.Use(db); err != nil {
		t.Fatal(err)
	}
	page := flow(1, "GET", "app.test", "/", 200, "Content-Type: text/html\r\n", `<script src="https://cdn.test/a.js"></script>`)
	s.Apply(visit(t, page))

	s2 := NewStore()
	s2.SetScope(scopeOf("app.test"))
	news := 0
	s2.Subscribe(func(e Event) {
		if e.Kind == RelatedAdded {
			news++
		}
	})
	if err := s2.Use(db); err != nil {
		t.Fatal(err)
	}
	if news != 0 {
		t.Errorf("reopening announced %d related hosts", news)
	}
	if rel := s2.Related(); len(rel) != 1 || rel[0].Host != "cdn.test" {
		t.Errorf("related after reopening = %+v", rel)
	}
}

func TestBareHost(t *testing.T) {
	for in, want := range map[string]string{
		"App.Test:8443": "app.test", "app.test": "app.test", "[::1]:80": "::1", "::1": "::1", "10.0.0.1:81": "10.0.0.1",
	} {
		if got := BareHost(in); got != want {
			t.Errorf("BareHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func find(list []Entity, key string) (Entity, bool) {
	for _, e := range list {
		if e.Key == key {
			return e, true
		}
	}
	return Entity{}, false
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestSuppressedHostsAreNotRelated(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("app.test"))
	var news []string
	s.Subscribe(func(e Event) {
		if e.Kind == RelatedAdded {
			news = append(news, e.Host)
		}
	})
	suppressed := map[string]bool{"cdn.test": true}
	s.SetSuppressed(func(h string) bool { return suppressed[h] })

	page := flow(1, "GET", "app.test", "/", 200, "Content-Type: text/html\r\n",
		`<script src="https://cdn.test/a.js"></script><script src="https://other.test/b.js"></script>`)
	s.Apply(visit(t, page))
	if rel := s.Related(); len(rel) != 1 || rel[0].Host != "other.test" {
		t.Errorf("related = %+v", rel)
	}
	if hidden := s.Suppressed(); len(hidden) != 1 || hidden[0].Host != "cdn.test" || hidden[0].URLs != 1 {
		t.Errorf("suppressed = %+v", hidden)
	}
	if !reflect.DeepEqual(news, []string{"other.test"}) {
		t.Errorf("announced %v", news)
	}
	// Found again while suppressed: still nothing.
	page.ID = 2
	s.Apply(visit(t, page))
	if len(news) != 1 {
		t.Errorf("announced %v", news)
	}

	// Lifting the suppression brings it back, announced since it never was.
	suppressed = map[string]bool{}
	s.SetSuppressed(func(h string) bool { return suppressed[h] })
	if rel := s.Related(); len(rel) != 2 {
		t.Errorf("related after lifting = %+v", rel)
	}
	if !reflect.DeepEqual(news, []string{"other.test", "cdn.test"}) {
		t.Errorf("announced %v", news)
	}
}

func TestSuppressedHostsLeaveTheMap(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("app.test"))

	// A target's own traffic, plus traffic to a second host collected
	// under no target (so it is on the Explorer by having been visited).
	s.Apply(visit(t, flow(1, "GET", "app.test", "/", 200, "", "")))
	cdn := flow(2, "GET", "cdn.test", "/a.js", 200, "", "")
	s.Apply(visit(t, cdn))
	if _, ok := find(s.Mapped(""), "https://cdn.test/a.js"); !ok {
		t.Fatal("visited host missing from the Explorer before suppression")
	}

	// Suppressing it takes every URL on it off the Explorer at once,
	// while leaving the target's own URLs.
	suppressed := map[string]bool{"cdn.test": true}
	s.SetSuppressed(func(h string) bool { return suppressed[h] })
	if _, ok := find(s.Mapped(""), "https://cdn.test/a.js"); ok {
		t.Error("suppressed host still on the Explorer")
	}
	if _, ok := find(s.Mapped(""), "https://app.test/"); !ok {
		t.Error("suppression took an unrelated URL off the Explorer")
	}
	// Its URLs are kept, not deleted.
	if n := len(s.OnHost("cdn.test")); n != 1 {
		t.Errorf("OnHost(cdn.test) = %d, URLs should be kept", n)
	}

	// New traffic to a suppressed host does not surface on the Explorer either.
	s.Apply(visit(t, flow(3, "GET", "cdn.test", "/b.js", 200, "", "")))
	if _, ok := find(s.Mapped(""), "https://cdn.test/b.js"); ok {
		t.Error("URL discovered while suppressed reached the Explorer")
	}

	// Lifting the suppression brings the host (and both URLs) back.
	suppressed = map[string]bool{}
	s.SetSuppressed(func(h string) bool { return suppressed[h] })
	for _, key := range []string{"https://cdn.test/a.js", "https://cdn.test/b.js"} {
		if _, ok := find(s.Mapped(""), key); !ok {
			t.Errorf("%s missing from the Explorer after lifting suppression", key)
		}
	}
}

func TestFindURLsInAYAMLOpenAPIDocument(t *testing.T) {
	doc := `openapi: 3.0.3
servers:
  - url: /api
paths:
  /users/{userId}:
    parameters:
      - $ref: '#/components/parameters/UserId'
    get:
      responses:
        200:
          description: ok
  /orders/{orderId}/items/{n}:
    get:
      parameters:
        - {name: orderId, in: path, schema: {type: string, example: A-17}}
        - {name: n, in: path, schema: {type: integer}}
  /files/{name}:
    get:
      parameters:
        - {name: name, in: path, schema: {type: string}}
components:
  parameters:
    UserId: {name: userId, in: path, example: 42}
`
	got := found(t, "application/yaml", doc, "https://app.test/openapi.yaml", nil)
	contains(t, got,
		"https://app.test/api/users/%7BuserId%7D openapi:GET /users/{userId}",
		"https://app.test/api/users/42 openapi:GET /users/{userId} example",
		"https://app.test/api/orders/A-17/items/1 openapi:GET /orders/{orderId}/items/{n} example",
	)
	for _, g := range got {
		if strings.Contains(g, "/files/") && strings.HasSuffix(g, " example") {
			t.Errorf("a parameter with nothing to try was filled in: %q", g)
		}
	}
	// Served as plain text, it is still read as YAML.
	contains(t, found(t, "text/plain", doc, "https://app.test/spec", nil), "https://app.test/api/users/42 openapi:GET /users/{userId} example")
	// YAML that isn't an API description is left to the text scan.
	if g := found(t, "application/yaml", "name: app\nsteps: [build]\n", "https://app.test/ci.yaml", nil); len(g) != 0 {
		t.Errorf("found %q in YAML that isn't an API description", g)
	}
}

func TestSwagger2ParameterExamplesAndOctetStreamSpecs(t *testing.T) {
	doc := `{"swagger":"2.0","basePath":"/v1","paths":{"/pets/{petId}":{"get":{"parameters":[{"name":"petId","in":"path","type":"integer"}]}}}}`
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: " +
		strconv.Itoa(len(doc)) + "\r\n\r\n" + doc
	u, _ := url.Parse("https://app.test/swagger.json")
	var got []string
	for _, f := range ResponseURLs([]byte(raw), "GET", u) {
		got = append(got, f.URL.String()+" "+f.Detail)
	}
	contains(t, got, "https://app.test/v1/pets/1 openapi:GET /pets/{petId} example")
}

// A host the targets' traffic led to but no target covers is related: the
// Explorer leaves it out (Drawn with related false) along with its domain,
// which nothing else is on, while Mapped still draws both.
func TestDrawnWithoutRelatedHosts(t *testing.T) {
	s := NewStore()
	s.SetScope(scopeOf("app.test"))

	page := flow(1, "GET", "app.test", "/", 200, "Content-Type: text/html\r\n",
		`<script src="https://cdn.test/app.js"></script>`)
	page.TargetID = "t:app.test"
	s.Apply(visit(t, page))
	// The browser then loads it, so the host is visited as well as related.
	s.Apply(visit(t, flow(2, "GET", "cdn.test", "/app.js", 200, "", "")))

	if _, ok := find(s.Mapped(""), "https://cdn.test/app.js"); !ok {
		t.Fatal("Mapped should draw a visited related host")
	}
	for _, key := range []string{"https://cdn.test/app.js", "//cdn.test", "cdn.test"} {
		if _, ok := find(s.Drawn("", false), key); ok {
			t.Errorf("Drawn without related still draws %q", key)
		}
	}
	for _, key := range []string{"https://app.test/", "//app.test", "app.test"} {
		if _, ok := find(s.Drawn("", false), key); !ok {
			t.Errorf("Drawn without related dropped %q, which is in scope", key)
		}
	}
}

// A value in markup is escaped text, so a reference has to be unescaped
// before it is read as a URL. Taken literally, a filter's
// url(&quot;#glow&quot;) became a URL of its own, and url(&#x27;/bg.webp&#x27;)
// became another: url.Parse stops the path at the # of the &#x27; that
// closes it, leaving just the "&", which is how https://immunefi.com/&
// turned up in the Explorer.
func TestFindURLsUnescapesMarkup(t *testing.T) {
	got := found(t, "text/html", `<span style="filter:url(&quot;#glow&quot;)">1</span>
		<div style="background-image:url(&#x27;/bg.webp&#x27;)"></div>
		<a href="/search?q=a&amp;page=2">s</a>
		<img srcset="/a.png?w=1&amp;h=2 1x">`, "https://app.test/page", nil)
	contains(t, got,
		"https://app.test/bg.webp css",
		"https://app.test/search?q=a&page=2 html:href",
		"https://app.test/a.png?w=1&h=2 html:srcset")
	lacks(t, got,
		"https://app.test/&",
		"https://app.test/&quot;",
		"https://app.test/&quot;#glow&quot;",
		"https://app.test/search?q=a&amp;page=2")
}

// A .css or .js file is not markup: "&" in one is itself, and nothing in it
// may be unescaped. Only a document read as HTML is.
func TestFindURLsLeavesStylesheetsAlone(t *testing.T) {
	contains(t, found(t, "text/css", `a{background:url('/i.png?w=1&amp;h=2')}`,
		"https://app.test/a.css", nil),
		"https://app.test/i.png?w=1&amp;h=2 css")
}

// The payload of a data: URI is not a document to mine for paths. A quoted
// path matches "/%3e%3c/svg%3e" in the middle of an inlined SVG, which
// decodes to "/></svg>" — not a path any URI can hold. Autocrawl requested
// one of these before this was caught.
func TestFindURLsIgnoresWhatIsInsideADataURI(t *testing.T) {
	js := `const icon = "data:image/svg+xml,%3csvg%20xmlns='http://www.w3.org/2000/svg'%3e` +
		`%3ccircle%20r='32'/%3e%3c/svg%3e"; fetch("/api/me");`
	got := found(t, "text/javascript", js, "https://app.test/app.js", nil)
	contains(t, got, "https://app.test/api/me js:fetch")
	lacks(t, got,
		"https://app.test/%3e%3c/svg%3e",
		"https://app.test/></svg>")
	for _, g := range got {
		if strings.ContainsAny(g, "<>") {
			t.Errorf("kept a path no URI can hold: %q", g)
		}
	}
}

// "&" is not special in a path — only inside a query — so a path that holds
// one is a URL of its own and is kept as it was written. RFC 3986 lists it
// as a sub-delimiter, so /a&b and / are different resources.
func TestFindURLsKeepsAnAmpersandInAPath(t *testing.T) {
	contains(t, found(t, "text/html", `<a href="/a&b">x</a><a href="/c;d">y</a>`,
		"https://app.test/", nil),
		"https://app.test/a&b html:href",
		"https://app.test/c;d html:href")
}

// A sitemap is XML, so every character reference in a <loc> is undone, not
// only &amp;.
func TestSitemapLocIsUnescaped(t *testing.T) {
	contains(t, found(t, "application/xml",
		`<urlset><url><loc>https://app.test/p?a=1&amp;b=2&#38;c=3</loc></url></urlset>`,
		"https://app.test/sitemap.xml", nil),
		"https://app.test/p?a=1&b=2&c=3 sitemap")
}
