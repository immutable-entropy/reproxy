package crawl

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
	"reproxy/internal/replay"
)

// site serves a small set of linked pages and counts the requests each
// path gets.
type site struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	pages map[string]string
	delay time.Duration

	inFlight, peak atomic.Int32
}

func newSite(t *testing.T, pages map[string]string) *site {
	t.Helper()
	s := &site{hits: map[string]int{}, pages: pages}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		for {
			p := s.peak.Load()
			if n <= p || s.peak.CompareAndSwap(p, n) {
				break
			}
		}
		s.mu.Lock()
		s.hits[r.URL.RequestURI()]++
		body, ok := s.pages[r.URL.RequestURI()]
		delay := s.delay
		s.mu.Unlock()
		time.Sleep(delay)
		if !ok {
			http.NotFound(w, r)
			return
		}
		ct := "text/html"
		if strings.HasSuffix(r.URL.Path, ".json") {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) host() string { u, _ := url.Parse(s.srv.URL); return u.Host }

func (s *site) hitsOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *site) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.hits {
		n += v
	}
	return n
}

// hosts is a scope covering the hosts in it, changeable as a test goes.
type hosts struct {
	mu sync.Mutex
	in map[string]bool
}

func scopeOf(list ...string) *hosts {
	h := &hosts{in: map[string]bool{}}
	for _, l := range list {
		h.in[l] = true
	}
	return h
}

func (h *hosts) add(host string) { h.mu.Lock(); h.in[host] = true; h.mu.Unlock() }

func (h *hosts) scope(host, _ string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "t1", h.in[host]
}

func newCrawler(store *capture.Store, sc *hosts, seeds ...string) *Crawler {
	return New(Options{
		Store: store,
		Dial:  (&net.Dialer{}).DialContext,
		Scope: sc.scope,
		Seeds: func() []string { return seeds },
	})
}

func fast() Settings {
	return Settings{RequestsPerSecond: MaxRequestsPerSecond, Threads: 1, OnlyNew: true}
}

func crawlOnce(t *testing.T, c *Crawler, s Settings) {
	t.Helper()
	if err := c.Start(s); err != nil {
		t.Fatal(err)
	}
	c.Wait()
}

func TestCrawlFollowsLinksOnceEachAndRecordsFlows(t *testing.T) {
	s := newSite(t, map[string]string{
		"/":          `<a href="/a">a</a> <a href="/b#top">b</a> <img src="/logo.png">`,
		"/a":         `<a href="/">home</a> <a href="/b">b</a> <a href="/c?x=1">c</a>`,
		"/b":         `<script>fetch("/api/items")</script>`,
		"/c?x=1":     `nothing`,
		"/api/items": `[]`,
	})
	store := capture.NewStore()
	c := newCrawler(store, scopeOf(s.host()), s.srv.URL+"/")
	crawlOnce(t, c, fast())

	for _, p := range []string{"/", "/a", "/b", "/c?x=1", "/api/items"} {
		if n := s.hitsOf(p); n != 1 {
			t.Errorf("%s fetched %d times, want 1", p, n)
		}
	}
	if n := s.hitsOf("/logo.png"); n != 0 {
		t.Errorf("an image was fetched %d times; it should be left to the Explorer", n)
	}
	for _, p := range SitemapPaths {
		if n := s.hitsOf(p); n != 1 {
			t.Errorf("well-known %s fetched %d times, want 1", p, n)
		}
	}
	want := 5 + len(SitemapPaths)
	st := c.Status()
	if st.Running || st.Ended != "finished" || st.Crawled != want || st.Queued != 0 || st.Sent != want {
		t.Errorf("status = %+v, want %d crawled", st, want)
	}
	flows := store.List()
	if len(flows) != want {
		t.Fatalf("%d flows recorded, want %d", len(flows), want)
	}
	for _, f := range flows {
		if f.Source != capture.SourceCrawl || f.State != capture.StateComplete || f.TargetID != "t1" {
			t.Errorf("flow %s %s: source %q state %v target %q", f.Method, f.Path, f.Source, f.State, f.TargetID)
		}
	}
}

func TestCrawlWaitsOnHostsOutOfScopeUntilATargetCoversThem(t *testing.T) {
	other := newSite(t, map[string]string{"/": `<a href="/deeper">x</a>`, "/deeper": `end`})
	s := newSite(t, map[string]string{"/": fmt.Sprintf(`<a href="%s/">related</a>`, other.srv.URL)})
	store := capture.NewStore()
	sc := scopeOf(s.host())
	c := newCrawler(store, sc, s.srv.URL+"/")
	crawlOnce(t, c, fast())

	if other.total() != 0 {
		t.Fatal("a host no target covers was crawled")
	}
	if st, ok := c.State(other.srv.URL + "/"); !ok || st != Waiting {
		t.Fatalf("the related link is %v (known %v), want waiting", st, ok)
	}

	// The related host is promoted to a target: crawling only new URLs
	// carries on there without fetching the first site again.
	sc.add(other.host())
	c.Rescope()
	crawlOnce(t, c, fast())
	if s.hitsOf("/") != 1 {
		t.Errorf("the first site's page was fetched %d times, want 1", s.hitsOf("/"))
	}
	if other.hitsOf("/") != 1 || other.hitsOf("/deeper") != 1 {
		t.Errorf("the promoted host's pages were fetched %v", other.hits)
	}
}

func TestStartWithNothingNewIsRefusedAndRecrawlFetchesAgain(t *testing.T) {
	s := newSite(t, map[string]string{"/": `<a href="/a">a</a>`, "/a": `end`})
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	crawlOnce(t, c, fast())
	if err := c.Start(fast()); err != ErrAllCrawled {
		t.Fatalf("starting with everything crawled: err = %v, want ErrAllCrawled", err)
	}
	again := fast()
	again.OnlyNew = false
	crawlOnce(t, c, again)
	if s.hitsOf("/") != 2 || s.hitsOf("/a") != 2 {
		t.Errorf("a full recrawl fetched / %d and /a %d times, want 2 each", s.hitsOf("/"), s.hitsOf("/a"))
	}
}

func TestOutOfScopeSeedsAreRefused(t *testing.T) {
	c := newCrawler(capture.NewStore(), scopeOf("covered.example"), "http://elsewhere.example/")
	if err := c.Start(fast()); err != ErrNothingToCrawl {
		t.Fatalf("err = %v, want ErrNothingToCrawl", err)
	}
}

func TestRateIsStrict(t *testing.T) {
	pages := map[string]string{"/": ""}
	for i := range 6 {
		pages["/"] += fmt.Sprintf(`<a href="/p%d">x</a>`, i)
		pages[fmt.Sprintf("/p%d", i)] = "end"
	}
	s := newSite(t, pages)
	var mu sync.Mutex
	var starts []time.Time
	c := New(Options{
		Store: capture.NewStore(),
		Dial:  (&net.Dialer{}).DialContext,
		Scope: scopeOf(s.host()).scope,
		Seeds: func() []string { return []string{s.srv.URL + "/"} },
		Send: func(ctx context.Context, r replay.Request) (replay.Response, error) {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			return replay.Send(ctx, r)
		},
	})
	crawlOnce(t, c, Settings{RequestsPerSecond: 10, Threads: min(4, MaxThreads()), OnlyNew: true})

	if want := 7 + len(SitemapPaths); len(starts) != want {
		t.Fatalf("%d requests, want %d", len(starts), want)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	for i := 1; i < len(starts); i++ {
		// Timers fire late, never early; allow a little for the clock.
		if gap := starts[i].Sub(starts[i-1]); gap < 95*time.Millisecond {
			t.Errorf("requests %d and %d started %v apart; at 10/s they must be 100ms apart", i-1, i, gap)
		}
	}
}

func TestThreadsBoundRequestsInFlight(t *testing.T) {
	if MaxThreads() < 2 {
		t.Skip("one CPU: one thread")
	}
	pages := map[string]string{"/": ""}
	for i := range 8 {
		pages["/"] += fmt.Sprintf(`<a href="/p%d">x</a>`, i)
		pages[fmt.Sprintf("/p%d", i)] = "end"
	}
	s := newSite(t, pages)
	s.delay = 150 * time.Millisecond
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	crawlOnce(t, c, Settings{RequestsPerSecond: MaxRequestsPerSecond, Threads: 2, OnlyNew: true})
	if p := s.peak.Load(); p > 2 {
		t.Errorf("%d requests were in flight at once with 2 threads", p)
	}
	if p := s.peak.Load(); p < 2 {
		t.Errorf("only %d request in flight at once; both threads should have been used", p)
	}
}

func TestStopLeavesTheRestQueuedAndStartCarriesOn(t *testing.T) {
	pages := map[string]string{"/": ""}
	for i := range 5 {
		pages["/"] += fmt.Sprintf(`<a href="/p%d">x</a>`, i)
		pages[fmt.Sprintf("/p%d", i)] = "end"
	}
	s := newSite(t, pages)
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	if err := c.Start(Settings{RequestsPerSecond: 5, Threads: 1, OnlyNew: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Status().Crawled < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c.Stop()
	c.Wait()
	st := c.Status()
	if st.Running || st.Ended != "stopped" || st.Queued == 0 {
		t.Fatalf("after Stop: %+v", st)
	}
	crawlOnce(t, c, fast())
	for p := range pages {
		if n := s.hitsOf(p); n != 1 {
			t.Errorf("%s fetched %d times across the stop, want 1", p, n)
		}
	}
}

func TestCrawlStateSurvivesReopeningTheEngagement(t *testing.T) {
	s := newSite(t, map[string]string{"/": `<a href="/a">a</a> <a href="http://related.example/x">r</a>`, "/a": "end"})
	path := filepath.Join(t.TempDir(), archive.FileName)
	ar, err := archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	if err := c.Use(ar.DB()); err != nil {
		t.Fatal(err)
	}
	settings := Settings{RequestsPerSecond: 20, Threads: 1, OnlyNew: true, UserAgent: "Custom/1.0"}
	crawlOnce(t, c, settings)
	if err := c.Use(nil); err != nil {
		t.Fatal(err)
	}
	ar.Close()

	ar, err = archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	if err := c.Use(ar.DB()); err != nil {
		t.Fatal(err)
	}
	if got := c.Settings(); !reflect.DeepEqual(got, settings) {
		t.Errorf("settings = %+v, want %+v", got, settings)
	}
	st := c.Status()
	if st.Crawled != 2+len(SitemapPaths) || st.Waiting != 1 || st.WriteError != "" {
		t.Errorf("reopened: %+v", st)
	}
	if err := c.Start(settings); err != ErrAllCrawled {
		t.Errorf("starting again: err = %v, want ErrAllCrawled", err)
	}
	if r := c.Recent(); len(r) != 2+len(SitemapPaths) || r[0].Links != -1 {
		t.Errorf("recent after reopening = %+v, want everything crawled", r)
	}
	if want := 2 + len(SitemapPaths); s.total() != want {
		t.Errorf("%d requests, want %d", s.total(), want)
	}
}

func TestSettingsAreValidated(t *testing.T) {
	for _, s := range []Settings{
		{RequestsPerSecond: 0, Threads: 1},
		{RequestsPerSecond: MaxRequestsPerSecond + 1, Threads: 1},
		{RequestsPerSecond: 1, Threads: 0},
		{RequestsPerSecond: 1, Threads: MaxThreads() + 1},
		{RequestsPerSecond: 1, Threads: 1, UserAgent: "x\r\nX-Injected: 1"},
		{RequestsPerSecond: 1, Threads: 1, UserAgent: "x\x00"},
		{RequestsPerSecond: 1, Threads: 1, UserAgent: strings.Repeat("x", MaxUserAgent+1)},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v was accepted", s)
		}
	}
	if err := DefaultSettings().Validate(); err != nil {
		t.Errorf("the defaults are invalid: %v", err)
	}
	for _, ua := range UserAgents {
		s := DefaultSettings()
		s.UserAgent = ua.Value
		if err := s.Validate(); err != nil {
			t.Errorf("offered user agent %s is invalid: %v", ua.ID, err)
		}
	}
}

// Every request carries the chosen User-Agent, and reProxy's own when none
// is chosen.
func TestCrawlSendsTheUserAgent(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"", DefaultUserAgent},
		{"Mozilla/5.0 (Test) Custom/2.0", "Mozilla/5.0 (Test) Custom/2.0"},
	} {
		var mu sync.Mutex
		seen := map[string]int{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.UserAgent()]++
			mu.Unlock()
			fmt.Fprint(w, `<a href="/a">a</a>`)
		}))
		u, _ := url.Parse(srv.URL)
		c := newCrawler(capture.NewStore(), scopeOf(u.Host), srv.URL+"/")
		settings := fast()
		settings.UserAgent = tc.set
		crawlOnce(t, c, settings)
		srv.Close()
		if len(seen) != 1 || seen[tc.want] == 0 {
			t.Errorf("with UserAgent %q the server saw %v, want only %q", tc.set, seen, tc.want)
		}
	}
}

func TestNoDialerNoCrawl(t *testing.T) {
	c := New(Options{Store: capture.NewStore(), Scope: scopeOf("x").scope})
	if err := c.Start(fast()); err == nil {
		t.Fatal("a crawler with no dialer started; it must never connect on its own")
	}
}

// The API description at a well-known path is read: its GET operations are
// crawled, a templated path through its parameter's example, and an
// operation with no GET isn't requested.
func TestCrawlFollowsAnOpenAPIDescription(t *testing.T) {
	s := newSite(t, map[string]string{
		"/": "home",
		"/openapi.json": `{"openapi":"3.0.0","paths":{
			"/items":{"get":{}},
			"/items/{id}":{"get":{"parameters":[{"name":"id","in":"path","example":5}]}},
			"/orders":{"post":{}}}}`,
	})
	s.mu.Lock()
	s.pages["/items"], s.pages["/items/5"] = "[]", "{}"
	s.mu.Unlock()
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	guessing := fast()
	guessing.APIDocs = true
	crawlOnce(t, c, guessing)
	for p, want := range map[string]int{"/openapi.json": 1, "/items": 1, "/items/5": 1, "/items/%7Bid%7D": 0, "/orders": 0} {
		if n := s.hitsOf(p); n != want {
			t.Errorf("%s fetched %d times, want %d", p, n, want)
		}
	}
	// Once one is found, the origin's other doc paths aren't guessed at.
	for _, p := range APIDocPaths[1:] {
		if n := s.hitsOf(p); n != 0 {
			t.Errorf("%s was guessed at after the description was found", p)
		}
	}
}

// Without the toggle, nothing is guessed but robots.txt and the sitemap;
// a description a page links to is still followed.
func TestAPIDocsAreOnlyGuessedAtWhenAsked(t *testing.T) {
	s := newSite(t, map[string]string{
		"/":              `<a href="/spec/api.json">API</a>`,
		"/spec/api.json": `{"openapi":"3.0.0","paths":{"/things":{"get":{}}}}`,
		"/things":        "[]",
	})
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	crawlOnce(t, c, fast())
	for _, p := range APIDocPaths {
		if n := s.hitsOf(p); n != 0 {
			t.Errorf("%s was guessed at with API docs off", p)
		}
	}
	for _, p := range append([]string{"/", "/spec/api.json", "/things"}, SitemapPaths...) {
		if n := s.hitsOf(p); n != 1 {
			t.Errorf("%s fetched %d times, want 1", p, n)
		}
	}
	// A description was found here, so turning guessing on adds nothing.
	guessing := fast()
	guessing.APIDocs = true
	if err := c.Start(guessing); err != ErrAllCrawled {
		t.Errorf("err = %v, want ErrAllCrawled: the origin's docs are already known", err)
	}
}

// Guesses queued by a run with the toggle on are dropped by a run without.
func TestTurningAPIDocsOffDropsQueuedGuesses(t *testing.T) {
	s := newSite(t, map[string]string{"/": "home"})
	c := newCrawler(capture.NewStore(), scopeOf(s.host()), s.srv.URL+"/")
	guessing := Settings{RequestsPerSecond: 2, Threads: 1, OnlyNew: true, APIDocs: true}
	if err := c.Start(guessing); err != nil {
		t.Fatal(err)
	}
	c.Stop()
	c.Wait()
	if err := c.Start(fast()); err != nil && err != ErrAllCrawled {
		t.Fatal(err)
	}
	c.Wait()
	for _, p := range APIDocPaths[1:] {
		if n := s.hitsOf(p); n != 0 {
			t.Errorf("%s was fetched after API docs were turned off", p)
		}
		if _, known := c.State(s.srv.URL + p); known {
			t.Errorf("%s is still kept", p)
		}
	}
}

// A target left out of the crawl isn't crawled; putting it back in crawls
// it, without fetching again what was crawled.
func TestExcludedTargetsAreLeftAlone(t *testing.T) {
	a := newSite(t, map[string]string{"/": "a"})
	b := newSite(t, map[string]string{"/": "b"})
	ids := map[string]string{a.host(): "ta", b.host(): "tb"}
	c := New(Options{
		Store: capture.NewStore(),
		Dial:  (&net.Dialer{}).DialContext,
		Scope: func(host, _ string) (string, bool) { id, ok := ids[host]; return id, ok },
		Seeds: func() []string { return []string{a.srv.URL + "/", b.srv.URL + "/"} },
	})
	only := fast()
	only.ExcludedTargets = []string{"tb"}
	crawlOnce(t, c, only)
	if a.hitsOf("/") != 1 || b.total() != 0 {
		t.Fatalf("with tb left out: a / %d, b %d requests", a.hitsOf("/"), b.total())
	}
	crawlOnce(t, c, fast())
	if a.hitsOf("/") != 1 || b.hitsOf("/") != 1 {
		t.Errorf("with every target: a / %d, b / %d, want 1 each", a.hitsOf("/"), b.hitsOf("/"))
	}
}

func TestDismissingTheWarningIsKeptWithTheEngagement(t *testing.T) {
	path := filepath.Join(t.TempDir(), archive.FileName)
	ar, err := archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	c := newCrawler(capture.NewStore(), scopeOf())
	if err := c.Use(ar.DB()); err != nil {
		t.Fatal(err)
	}
	if c.WarningDismissed() {
		t.Fatal("dismissed before anyone asked")
	}
	if err := c.DismissWarning(); err != nil {
		t.Fatal(err)
	}
	if err := c.Use(nil); err != nil {
		t.Fatal(err)
	}
	if c.WarningDismissed() {
		t.Error("the dismissal leaked out of the engagement")
	}
	if err := c.Use(ar.DB()); err != nil {
		t.Fatal(err)
	}
	if !c.WarningDismissed() {
		t.Error("the dismissal wasn't kept")
	}
}

// An origin that refuses connections costs one request, not one per URL.
func TestAnUnreachableOriginIsTriedOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close() // nothing listens there now
	var sends atomic.Int32
	c := New(Options{
		Store: capture.NewStore(),
		Dial:  (&net.Dialer{}).DialContext,
		Scope: scopeOf(dead).scope,
		Seeds: func() []string { return []string{"http://" + dead + "/"} },
		Send: func(ctx context.Context, r replay.Request) (replay.Response, error) {
			sends.Add(1)
			return replay.Send(ctx, r)
		},
	})
	crawlOnce(t, c, fast())
	if n := sends.Load(); n != 1 {
		t.Errorf("%d requests to an origin that refuses connections, want 1", n)
	}
	if st := c.Status(); st.Failed != 1+len(SitemapPaths) || st.Queued != 0 {
		t.Errorf("status = %+v", st)
	}
}

// redirectSite serves /old as a 301 to /new, and /new as a page, counting
// the hits on each.
func redirectSite(t *testing.T) (*httptest.Server, string, func(path string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
		case "/new":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, mustHost(srv.URL), func(p string) int { mu.Lock(); defer mu.Unlock(); return hits[p] }
}

func mustHost(raw string) string { u, _ := url.Parse(raw); return u.Host }

// pathScope allows only the given paths on host (plus the well-known probe
// paths), standing in for a target whose path rules don't harvest /new.
func pathScope(host string, allow ...string) ScopeFunc {
	ok := map[string]bool{"/robots.txt": true, "/sitemap.xml": true}
	for _, p := range allow {
		ok[p] = true
	}
	return func(h, p string) (string, bool) { return "t1", h == host && ok[p] }
}

func TestFollowsRedirectByHostScopeEvenWherePathRulesWouldNot(t *testing.T) {
	srv, host, hitsOf := redirectSite(t)
	c := New(Options{
		Store:     capture.NewStore(),
		Dial:      (&net.Dialer{}).DialContext,
		Scope:     pathScope(host, "/old"), // /new is out of path scope
		HostScope: func(h string) (string, bool) { return "t1", h == host },
		Seeds:     func() []string { return []string{srv.URL + "/old"} },
	})
	crawlOnce(t, c, fast())

	if hitsOf("/old") != 1 {
		t.Fatalf("/old fetched %d times, want 1", hitsOf("/old"))
	}
	if hitsOf("/new") != 1 {
		t.Fatalf("the redirect target /new fetched %d times, want 1 — the 301 wasn't followed", hitsOf("/new"))
	}
	if st, _ := c.State(srv.URL + "/new"); st != Crawled {
		t.Fatalf("/new state = %v, want crawled", st)
	}
}

func TestRedirectNotFollowedWhenTargetHostIsOutOfScope(t *testing.T) {
	srv, host, hitsOf := redirectSite(t)
	// No HostScope, and the path scope allows only /old: the 301 target is
	// on an in-scope host but not followed, because nothing says to.
	c := New(Options{
		Store: capture.NewStore(),
		Dial:  (&net.Dialer{}).DialContext,
		Scope: pathScope(host, "/old"),
		Seeds: func() []string { return []string{srv.URL + "/old"} },
	})
	crawlOnce(t, c, fast())

	if hitsOf("/old") != 1 {
		t.Fatalf("/old fetched %d times, want 1", hitsOf("/old"))
	}
	if hitsOf("/new") != 0 {
		t.Fatalf("/new was fetched %d times; without host scope the redirect shouldn't be followed", hitsOf("/new"))
	}
	if st, ok := c.State(srv.URL + "/new"); !ok || st != Waiting {
		t.Fatalf("/new state = %v (known %v), want waiting", st, ok)
	}
}

// TestRedirectChainIsCapped guards against an endless crawl: a server that
// redirects on and on, minting a fresh URL each hop (an auth loop with a
// changing token), must be dropped after maxRedirects rather than chased
// forever.
func TestRedirectChainIsCapped(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the chain redirects; other paths (the well-known probes)
		// 404, so this is one runaway chain, not several.
		if r.URL.Path != "/start" && r.URL.Path != "/step" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		// Each hop redirects to a brand-new URL, forever.
		http.Redirect(w, r, fmt.Sprintf("/step?t=%d", n), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	host := mustHost(srv.URL)

	c := New(Options{
		Store:     capture.NewStore(),
		Dial:      (&net.Dialer{}).DialContext,
		Scope:     func(h, _ string) (string, bool) { return "t1", h == host },
		HostScope: func(h string) (string, bool) { return "t1", h == host },
		Seeds:     func() []string { return []string{srv.URL + "/start"} },
	})
	crawlOnce(t, c, fast())

	mu.Lock()
	got := hits
	mu.Unlock()
	// The seed plus at most maxRedirects hops. Far from endless.
	if got > maxRedirects+1 {
		t.Fatalf("followed %d chain requests; a redirect chain should stop after %d hops", got, maxRedirects)
	}
	if got < 2 {
		t.Fatalf("followed %d requests; the redirect should have been followed at least once", got)
	}
	if st := c.Status(); st.Running {
		t.Fatal("the crawl is still running")
	}
}
