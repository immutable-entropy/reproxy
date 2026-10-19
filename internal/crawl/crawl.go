// Package crawl is Autocrawl: it follows the links of the targets' pages,
// recursively, so the Entity Explorer maps every URL, host and domain they
// lead to.
//
// It is core (discovery feeds intercept/edit/replay; see CLAUDE.md) and has
// no Qt dependency. Every request it makes goes out through the dial
// function it is given, which app makes the upstream chain in force (with
// the lookup made through package dns), and is recorded in the capture
// store as a flow whose Source is capture.SourceCrawl, so the Traffic list,
// the archive, the Explorer's indexer and plugins see it like any other.
//
// What it has crawled is kept in the engagement's capture.db (tables
// crawl_urls and crawl_meta, schema owned by package archive), so a crawl
// can be stopped and started again at any time without visiting a URL
// twice. A link to a host no target covers is kept as Waiting: when a
// target comes to cover it (a related host promoted, say), Rescope queues
// it, and the crawl carries on into that host.
//
// Two limits are strict. Requests start at most RequestsPerSecond apart,
// with no burst, across every worker and across stops and starts; and at
// most Threads requests are in flight at once.
package crawl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/replay"
)

// State is where a URL stands.
type State int

const (
	// Queued is waiting its turn.
	Queued State = iota
	// Crawled was fetched (whatever the status code).
	Crawled
	// Failed couldn't be fetched: no connection, a timeout, a bad TLS
	// handshake.
	Failed
	// Waiting is a link to somewhere no target covers, or under a target
	// the crawl leaves out. It is queued when that changes.
	Waiting

	// dropped marks an entry forgotten while it sat in the queue.
	dropped State = -1
)

func (s State) String() string {
	switch s {
	case Queued:
		return "queued"
	case Crawled:
		return "crawled"
	case Failed:
		return "failed"
	case Waiting:
		return "waiting"
	}
	return "unknown"
}

// Settings are how a crawl runs. They are kept with the engagement.
type Settings struct {
	// RequestsPerSecond is the most requests started in any second, across
	// every thread.
	RequestsPerSecond float64
	// Threads is the most requests in flight at once.
	Threads int
	// OnlyNew skips the URLs an earlier run crawled. Off, starting crawls
	// everything again (each URL still once in the run).
	OnlyNew bool
	// APIDocs guesses where an origin's API description is kept
	// (APIDocPaths) until one is found. Off, the crawl follows only what it
	// has been led to, with each origin's robots.txt and sitemap.
	APIDocs bool
	// ExcludedTargets are the IDs of targets this crawl leaves alone. It
	// lists those left out rather than those chosen, so a target added
	// later is crawled unless the user says otherwise.
	ExcludedTargets []string
}

// excludes reports whether the settings leave a target out.
func (s Settings) excludes(targetID string) bool {
	for _, id := range s.ExcludedTargets {
		if id == targetID {
			return true
		}
	}
	return false
}

// Limits on Settings.
const (
	MinRequestsPerSecond = 0.1
	MaxRequestsPerSecond = 50
)

// MaxThreads is the most threads a crawl may use: one per CPU this machine
// has.
func MaxThreads() int { return max(1, runtime.NumCPU()) }

// DefaultSettings are a gentle crawl: one request a second, one at a time,
// new URLs only, every target, and no guessing at API descriptions.
func DefaultSettings() Settings {
	return Settings{RequestsPerSecond: 1, Threads: 1, OnlyNew: true}
}

// Validate checks the settings are within the limits.
func (s Settings) Validate() error {
	if s.RequestsPerSecond < MinRequestsPerSecond || s.RequestsPerSecond > MaxRequestsPerSecond {
		return fmt.Errorf("requests per second must be between %g and %g", float64(MinRequestsPerSecond), float64(MaxRequestsPerSecond))
	}
	if s.Threads < 1 || s.Threads > MaxThreads() {
		return fmt.Errorf("threads must be between 1 and %d", MaxThreads())
	}
	return nil
}

// Tuning.
const (
	// requestTimeout bounds one fetch.
	requestTimeout = 30 * time.Second
	// maxResponse caps a response held in memory; a larger one fails.
	maxResponse = 8 << 20
	// maxURLs bounds how many URLs one engagement's crawl keeps, so a site
	// that generates links without end can't grow it without limit.
	maxURLs = 200000
	// maxRedirects bounds a chain of redirects the crawl follows from one
	// URL. A server that redirects on and on — an auth loop that mints a
	// fresh token each hop, say — would otherwise have the crawl chase a new
	// URL every time; after this many hops in a row the chain is dropped.
	maxRedirects = 10
	// maxRecent is how many results Recent keeps.
	maxRecent = 200
	userAgent = "Mozilla/5.0 (compatible; reProxy-Autocrawl/1.0)"
)

// ScopeFunc reports whether a target covers a URL's host and allows its
// path (the request target, with its query), and which one.
type ScopeFunc func(host, path string) (targetID string, ok bool)

// DialFunc opens the connection a request goes out on.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Sender performs one exchange; replay.Send is the default. Tests
// substitute it.
type Sender func(context.Context, replay.Request) (replay.Response, error)

// Options are what a Crawler works with.
type Options struct {
	// Store is where each request is recorded. Required.
	Store *capture.Store
	// Dial is how requests leave: app makes it the upstream chain. It is
	// required, so the crawler has no way to connect on its own.
	Dial DialFunc
	// Scope says what the targets cover. Without one nothing is in scope.
	Scope ScopeFunc
	// HostScope reports whether a target covers a host, ignoring its path
	// rules. It is consulted only to follow a redirect: when a crawled URL
	// redirects (a 3xx with a Location) to a host a target covers, the
	// crawl follows it even if the path rules wouldn't harvest that path,
	// because a redirect is the server saying the resource is there. May be
	// nil (redirects are then followed only within ordinary path scope).
	HostScope func(host string) (targetID string, ok bool)
	// Seeds lists where a crawl starts: the targets' origins and the URLs
	// already known under them. Those out of scope are ignored.
	Seeds func() []string
	// Send replaces replay.Send, for tests.
	Send Sender
}

// Result is what came of one URL, for the window's activity list.
type Result struct {
	URL     string
	State   State
	Status  int
	Error   string
	FlowID  uint64
	FoundOn string
	At      time.Time
	// Links is how many new URLs it led to (queued or waiting), or -1 for
	// a result from an earlier session, which doesn't record it.
	Links int
}

// Status is the crawl at a moment.
type Status struct {
	Running bool
	// Stopping is set between Stop and the last request in flight ending.
	Stopping bool
	// Recording is false with no database behind the crawler: what it does
	// is then forgotten when the engagement changes.
	Recording bool
	Queued    int
	Crawled   int
	Failed    int
	Waiting   int
	// InFlight counts requests started or waiting for their slot.
	InFlight int
	// Sent counts requests this run (or the last one) made.
	Sent     int
	Started  time.Time
	Finished time.Time
	// Ended says why the last run stopped: "finished", "stopped", or an
	// error. Empty while running or before the first run.
	Ended    string
	Settings Settings
	// WriteError is the last failure to record the crawl in the
	// engagement, if any: the crawl goes on, but what it did may not be
	// remembered.
	WriteError string
}

// ErrRunning is returned by Start while a crawl runs.
var ErrRunning = errors.New("a crawl is already running")

// ErrNothingToCrawl is returned by Start when no target covers anything to
// start from.
var ErrNothingToCrawl = errors.New("nothing to crawl: add a target (or browse one) first")

// ErrAllCrawled is returned by Start when crawling only new URLs and every
// URL in scope has been crawled.
var ErrAllCrawled = errors.New("everything in scope has been crawled: turn off “Only crawl new URLs” to crawl it again, " +
	"or promote a related host to a target to carry on there")

type entry struct {
	url     string
	state   State
	depth   int
	foundOn string
	status  int
	flowID  uint64
	err     string
	added   time.Time
	crawled time.Time
	// viaRedirect marks a URL a redirect led to: it may be in scope by its
	// host alone (HostScope), so it is kept crawlable even where the path
	// rules wouldn't harvest it.
	viaRedirect bool
	// redirects is how many redirects in a row led to this URL, so a runaway
	// redirect chain is dropped once it passes maxRedirects. A URL reached
	// by an ordinary link is 0.
	redirects int
}

// Crawler is the engagement's Autocrawl. It is safe for concurrent use.
type Crawler struct {
	opts Options
	send Sender
	lim  limiter

	mu       sync.Mutex
	db       *sql.DB
	urls     map[string]*entry
	queue    []*entry // Queued entries in crawl order; others are skipped when popped
	settings Settings
	run      *run
	recent   []Result
	ended    string
	sent     int
	started  time.Time
	finished time.Time
	stopping bool
	writeErr error
	// probed is the origins whose SitemapPaths this session has added, and
	// probedDocs those whose APIDocPaths it has.
	probed, probedDocs map[string]bool
	// docsFound is the origins an API description was found on: their
	// APIDocPaths aren't guessed at any more. It is kept with the crawl.
	docsFound map[string]bool
	// warningDismissed is set once the user asks not to be warned before
	// starting a crawl in this engagement.
	warningDismissed bool
}

// run is one start-to-stop of the crawl.
type run struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	inFlight int
	workers  int
}

// New returns a crawler with nothing crawled and the default settings.
func New(opts Options) *Crawler {
	c := &Crawler{opts: opts, send: opts.Send, urls: map[string]*entry{}, settings: DefaultSettings(),
		probed: map[string]bool{}, probedDocs: map[string]bool{}, docsFound: map[string]bool{}}
	if c.send == nil {
		c.send = replay.Send
	}
	return c
}

// Use points the crawler at db — the open engagement's capture.db — and
// loads what it holds. A crawl running is stopped first, and Use waits for
// its requests to end, so nothing of one engagement is written into
// another's. A nil db keeps the crawl in memory.
func (c *Crawler) Use(db *sql.DB) error {
	c.Stop()
	c.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.db = db
	c.urls, c.queue, c.recent = map[string]*entry{}, nil, nil
	c.probed, c.probedDocs, c.docsFound = map[string]bool{}, map[string]bool{}, map[string]bool{}
	c.settings, c.warningDismissed = DefaultSettings(), false
	c.ended, c.sent, c.started, c.finished = "", 0, time.Time{}, time.Time{}
	if db == nil {
		return nil
	}
	return c.loadLocked()
}

// Settings returns the settings last saved.
func (c *Crawler) Settings() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// SaveSettings validates and keeps s for the next Start. It is refused
// while a crawl runs: the limits apply to a whole run.
func (c *Crawler) SaveSettings(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil {
		return ErrRunning
	}
	c.settings = s
	return c.saveSettingsLocked()
}

// WarningDismissed reports whether the user has asked not to be warned
// before starting a crawl in this engagement.
func (c *Crawler) WarningDismissed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warningDismissed
}

// DismissWarning stops the warning before a crawl starts, for this
// engagement: it is kept with the crawl's settings.
func (c *Crawler) DismissWarning() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warningDismissed = true
	return c.saveSettingsLocked()
}

// Start saves s and starts crawling. It returns at once; Status follows
// the crawl, which ends by itself when nothing in scope is left to fetch.
func (c *Crawler) Start(s Settings) error {
	if c.opts.Dial == nil || c.opts.Store == nil {
		return errors.New("the crawler has no way to send requests")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	// The seeds are worked out before taking the lock: they read other
	// stores.
	var seeds []string
	if c.opts.Seeds != nil {
		seeds = c.opts.Seeds()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil {
		return ErrRunning
	}
	c.settings = s
	if err := c.saveSettingsLocked(); err != nil {
		return err
	}
	var changed []*entry
	if !s.APIDocs {
		// Guesses an earlier run queued aren't followed up without the
		// toggle; they are put back if it is turned on again.
		var guesses []*entry
		for _, e := range c.urls {
			if e.state == Queued && isDocProbe(e) {
				guesses = append(guesses, e)
			}
		}
		c.dropLocked(guesses)
		c.probedDocs = map[string]bool{}
	}
	if !s.OnlyNew {
		// Everything again: what was crawled (or failed) goes back in the
		// queue, in the order it was first found.
		for _, e := range c.ordered() {
			if e.state == Crawled || e.state == Failed {
				e.state, e.status, e.flowID, e.err, e.crawled = Queued, 0, 0, "", time.Time{}
				changed = append(changed, e)
			}
		}
	}
	changed = append(changed, c.rescopeLocked()...)
	changed = append(changed, c.addSeedsLocked(seeds)...)
	c.rebuildQueueLocked()
	if err := c.writeLocked(changed...); err != nil {
		return err
	}
	if c.queuedLocked() == 0 {
		for _, e := range c.urls {
			if e.state == Crawled || e.state == Failed {
				return ErrAllCrawled
			}
		}
		return ErrNothingToCrawl
	}

	c.lim.setRate(s.RequestsPerSecond)
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{ctx: ctx, cancel: cancel, workers: s.Threads}
	c.run = r
	c.ended, c.sent, c.started, c.finished, c.stopping = "", 0, time.Now(), time.Time{}, false
	r.wg.Add(s.Threads)
	for range s.Threads {
		go c.work(r)
	}
	return nil
}

// Stop ends the crawl: no new request starts, and those in flight are
// abandoned (their URLs stay queued). It returns at once; Wait waits for
// the last of them.
func (c *Crawler) Stop() {
	c.mu.Lock()
	r := c.run
	if r != nil {
		c.stopping = true
		if c.ended == "" {
			c.ended = "stopped"
		}
	}
	c.mu.Unlock()
	if r != nil {
		r.cancel()
	}
}

// Wait blocks until no crawl is running.
func (c *Crawler) Wait() {
	c.mu.Lock()
	r := c.run
	c.mu.Unlock()
	if r != nil {
		r.wg.Wait()
	}
}

// Rescope re-reads what the targets cover: a waiting URL a target now
// covers is queued (with the seeds of any new target), and a queued one
// none does any more waits. app calls it whenever the targets change, so a
// host promoted while the crawl runs is crawled in the same run.
func (c *Crawler) Rescope() {
	var seeds []string
	if c.opts.Seeds != nil {
		seeds = c.opts.Seeds()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.rescopeLocked()
	changed = append(changed, c.addSeedsLocked(seeds)...)
	if len(changed) > 0 {
		c.rebuildQueueLocked()
		_ = c.writeLocked(changed...)
	}
}

// Status reports where the crawl stands.
func (c *Crawler) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Running: c.run != nil, Stopping: c.stopping, Recording: c.db != nil,
		Sent: c.sent, Started: c.started, Finished: c.finished, Ended: c.ended,
		Settings: c.settings,
	}
	if c.writeErr != nil {
		st.WriteError = c.writeErr.Error()
	}
	if c.run != nil {
		st.InFlight = c.run.inFlight
	}
	for _, e := range c.urls {
		switch e.state {
		case Queued:
			st.Queued++
		case Crawled:
			st.Crawled++
		case Failed:
			st.Failed++
		case Waiting:
			st.Waiting++
		}
	}
	return st
}

// Recent returns the latest results, newest first.
func (c *Crawler) Recent() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Result, len(c.recent))
	for i, r := range c.recent {
		out[len(c.recent)-1-i] = r
	}
	return out
}

// State reports where one URL stands, as the crawler normalises it.
func (c *Crawler) State(rawURL string) (State, bool) {
	u, ok := normalize(rawURL)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.urls[u.String()]
	if !ok {
		return 0, false
	}
	return e.state, true
}

// ---- Running ------------------------------------------------------------------

func (c *Crawler) work(r *run) {
	defer c.workerDone(r)
	for {
		e, ok := c.next(r)
		if !ok {
			return
		}
		c.fetch(r, e)
	}
}

// next takes the next URL to fetch, waiting while other workers may yet
// queue more. ok is false when the run is over.
func (c *Crawler) next(r *run) (*entry, bool) {
	for {
		c.mu.Lock()
		if r.ctx.Err() != nil || c.run != r {
			c.mu.Unlock()
			return nil, false
		}
		if e := c.popLocked(); e != nil {
			r.inFlight++
			c.mu.Unlock()
			return e, true
		}
		idle := r.inFlight == 0
		c.mu.Unlock()
		if idle {
			// Nothing queued and nothing in flight that could queue more.
			return nil, false
		}
		select {
		case <-r.ctx.Done():
			return nil, false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *Crawler) workerDone(r *run) {
	c.mu.Lock()
	r.workers--
	if r.workers == 0 && c.run == r {
		c.run = nil
		c.stopping = false
		c.finished = time.Now()
		if c.ended == "" {
			c.ended = "finished"
		}
	}
	c.mu.Unlock()
	r.cancel()
	r.wg.Done()
}

// popLocked takes the first queued URL still in scope off the queue. One a
// target no longer covers goes back to waiting.
func (c *Crawler) popLocked() *entry {
	for len(c.queue) > 0 {
		e := c.queue[0]
		c.queue = c.queue[1:]
		if e.state != Queued {
			continue
		}
		if !c.scoped(e) {
			e.state = Waiting
			_ = c.writeLocked(e)
			continue
		}
		return e
	}
	return nil
}

// fetch makes one request, records it, and queues what it links to.
func (c *Crawler) fetch(r *run, e *entry) {
	u, _ := url.Parse(e.url)
	targetID, _ := c.scope(u)

	if err := c.lim.wait(r.ctx); err != nil {
		c.requeue(r, e)
		return
	}
	raw := requestFor(u)
	addr := hostPort(u)
	secure := u.Scheme == "https"
	started := time.Now()
	flowID := c.opts.Store.Add(capture.Flow{
		State: capture.StatePending, Target: addr, Method: "GET", Host: u.Host, Path: u.RequestURI(),
		Secure: secure, TargetID: targetID, Source: capture.SourceCrawl,
		Request: capture.Message{Raw: raw, Size: int64(len(raw))}, Started: started,
	})
	c.mu.Lock()
	c.sent++
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.ctx, requestTimeout)
	ctx = capture.WithSource(ctx, capture.SourceCrawl, "") // for the lookup
	resp, err := c.send(ctx, replay.Request{Addr: addr, Secure: secure, Raw: raw, Dial: c.opts.Dial, Limit: maxResponse})
	cancel()
	finished := time.Now()

	if err != nil {
		if r.ctx.Err() != nil {
			// Stopped: the URL wasn't really tried, so it is tried next run.
			c.opts.Store.Fail(flowID, "Autocrawl stopped", finished)
			c.requeue(r, e)
			return
		}
		c.opts.Store.Fail(flowID, err.Error(), finished)
		c.finish(r, e, Failed, 0, flowID, err.Error(), nil)
		if unreachable(err) {
			c.skipOrigin(r, u, err)
		}
		return
	}
	c.opts.Store.Complete(flowID, capture.Message{Raw: resp.Raw, Size: int64(resp.Size)},
		resp.StatusCode, strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode))), started.Add(resp.Duration))
	c.finish(r, e, Crawled, resp.StatusCode, flowID, "", entities.ResponseURLs(resp.Raw, "GET", u))
}

// unreachable reports whether a send failed before reaching the server at
// all — no connection, or no TLS session — which every other URL on that
// origin would fail the same way. A timeout or a dropped response doesn't
// count: one slow page says nothing about the rest.
func unreachable(err error) bool {
	msg := err.Error()
	return strings.HasPrefix(msg, "connecting to ") || strings.HasPrefix(msg, "TLS handshake with ")
}

// skipOrigin marks the URLs still queued on an origin the crawl couldn't
// reach as failed, without requesting them, so a dead origin costs one
// request rather than one per URL. A run that doesn't crawl only new URLs
// tries them again.
func (c *Crawler) skipOrigin(r *run, u *url.URL, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != r {
		return
	}
	prefix := u.Scheme + "://" + u.Host + "/"
	var skipped []*entry
	for _, e := range c.urls {
		if e.state == Queued && strings.HasPrefix(e.url, prefix) {
			e.state, e.err, e.crawled = Failed, "not tried: "+cause.Error(), time.Now()
			skipped = append(skipped, e)
		}
	}
	_ = c.writeLocked(skipped...)
}

// requeue puts a URL whose fetch was abandoned back at the front.
func (c *Crawler) requeue(r *run, e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.inFlight--
	if e.state == Queued {
		c.queue = append([]*entry{e}, c.queue...)
	}
}

// finish records a fetch's outcome and what it links to, in one write.
func (c *Crawler) finish(r *run, e *entry, state State, status int, flowID uint64, errText string, found []entities.Found) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r.inFlight--
	if c.run != r {
		return // the engagement changed under the run
	}
	e.state, e.status, e.flowID, e.err, e.crawled = state, status, flowID, errText, time.Now()
	changed := []*entry{e}
	describes := false
	redirect := isRedirect(status)
	for _, f := range found {
		if strings.HasPrefix(f.Detail, "openapi:") {
			describes = true
		}
		if specWithoutGet(f.Detail) {
			continue // the Explorer maps it; a GET would only be refused
		}
		// A redirect's Location is followed by host scope, not path scope.
		toRedirect := redirect && (f.Detail == "header:location" || f.Detail == "header:content-location")
		chain := 0
		if toRedirect {
			chain = e.redirects + 1
			if chain > maxRedirects {
				continue // a runaway redirect chain: stop chasing it
			}
		}
		for _, n := range c.addLocked(f.URL.String(), e.depth+1, e.url, toRedirect, chain) {
			changed = append(changed, n)
			if n.state == Queued {
				c.queue = append(c.queue, n)
			}
		}
	}
	if describes {
		c.docsFoundLocked(e.url)
	}
	_ = c.writeLocked(changed...)
	c.recent = append(c.recent, Result{
		URL: e.url, State: state, Status: status, Error: errText, FlowID: flowID,
		FoundOn: e.foundOn, At: e.crawled, Links: len(changed) - 1,
	})
	if len(c.recent) > maxRecent {
		c.recent = c.recent[len(c.recent)-maxRecent:]
	}
}

// ---- The frontier ---------------------------------------------------------------

// SitemapPaths are fetched on every origin a crawl reaches in scope: the
// files a site lists its own URLs in.
var SitemapPaths = []string{"/robots.txt", "/sitemap.xml"}

// APIDocPaths are where API descriptions (OpenAPI, Swagger) are usually
// served from, whose operations entities.FindURLs reads. They are guesses,
// so they are only tried with Settings.APIDocs, and on an origin only until
// a description is found there.
var APIDocPaths = []string{
	"/openapi.json", "/openapi.yaml", "/swagger.json", "/swagger.yaml",
	"/v3/api-docs", "/v2/api-docs", "/api-docs", "/swagger/v1/swagger.json",
	"/api/openapi.json", "/api/swagger.json",
}

// isDocProbe reports whether an entry is a guess at an API description
// rather than something the crawl was led to: one of APIDocPaths that no
// page linked to.
func isDocProbe(e *entry) bool {
	if e.foundOn != "" {
		return false
	}
	u, err := url.Parse(e.url)
	if err != nil || u.RawQuery != "" {
		return false
	}
	for _, p := range APIDocPaths {
		if u.Path == p {
			return true
		}
	}
	return false
}

// docsFoundLocked records that an API description was found on raw's
// origin, and stops guessing there: its doc probes still queued are
// dropped.
func (c *Crawler) docsFoundLocked(raw string) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	origin := u.Scheme + "://" + u.Host
	if c.docsFound[origin] {
		return
	}
	c.docsFound[origin] = true
	var guesses []*entry
	for _, e := range c.urls {
		if e.state == Queued && isDocProbe(e) && strings.HasPrefix(e.url, origin+"/") {
			guesses = append(guesses, e)
		}
	}
	c.dropLocked(guesses)
	_ = c.saveSettingsLocked()
}

// dropLocked forgets entries that were never fetched. One still in the
// queue is skipped when it comes up.
func (c *Crawler) dropLocked(list []*entry) {
	for _, e := range list {
		e.state = dropped
		delete(c.urls, e.url)
	}
	_ = c.deleteLocked(list...)
}

// addLocked adds a URL not seen before: queued when a target covers it,
// waiting otherwise. The first queued URL on an origin brings that
// origin's well-known paths with it (see probesLocked). It returns what it added: nothing for
// a URL already known, one not worth fetching, or once maxURLs are kept.
func (c *Crawler) addLocked(raw string, depth int, foundOn string, redirect bool, redirects int) []*entry {
	u, ok := normalize(raw)
	if !ok || !crawlable(u) {
		return nil
	}
	key := u.String()
	if e, known := c.urls[key]; known {
		// A redirect to a URL already known but waiting (its path is out of
		// scope) promotes it, if its host is a target: the server says the
		// resource is there.
		if redirect && e.state == Waiting && c.inHostScope(key) {
			e.state, e.viaRedirect, e.redirects = Queued, true, redirects
			return append([]*entry{e}, c.probesLocked(u)...)
		}
		return nil
	}
	if len(c.urls) >= maxURLs {
		return nil
	}
	e := &entry{url: key, state: Waiting, depth: depth, foundOn: foundOn, added: time.Now(), viaRedirect: redirect, redirects: redirects}
	if c.inScope(key) || (redirect && c.inHostScope(key)) {
		e.state = Queued
	}
	c.urls[key] = e
	out := []*entry{e}
	if e.state == Queued {
		out = append(out, c.probesLocked(u)...)
	}
	return out
}

// probesLocked adds an origin's SitemapPaths and, when the settings guess
// at API descriptions and none has been found there, its APIDocPaths;
// each once a session (what an earlier session added is known, and not
// added again).
func (c *Crawler) probesLocked(u *url.URL) []*entry {
	origin := u.Scheme + "://" + u.Host
	var paths []string
	if !c.probed[origin] {
		c.probed[origin] = true
		paths = append(paths, SitemapPaths...)
	}
	if c.settings.APIDocs && !c.probedDocs[origin] && !c.docsFound[origin] {
		c.probedDocs[origin] = true
		paths = append(paths, APIDocPaths...)
	}
	var out []*entry
	for _, p := range paths {
		out = append(out, c.addLocked(origin+p, 0, "", false, 0)...)
	}
	return out
}

// addSeedsLocked adds the seeds no earlier run found, and the well-known
// paths of their origins; those out of scope are left out rather than kept
// waiting.
func (c *Crawler) addSeedsLocked(seeds []string) []*entry {
	var out []*entry
	for _, s := range seeds {
		if !c.inScope(s) {
			continue
		}
		out = append(out, c.addLocked(s, 0, "", false, 0)...)
		if u, ok := normalize(s); ok {
			out = append(out, c.probesLocked(u)...)
		}
	}
	return out
}

// rescopeLocked moves URLs between Waiting and Queued as the targets now
// say, returning those that moved.
func (c *Crawler) rescopeLocked() []*entry {
	var out []*entry
	for _, e := range c.ordered() { // a copy: probes are added as it goes
		in := c.scoped(e)
		switch {
		case e.state == Waiting && in:
			e.state = Queued
			out = append(out, e)
			if u, err := url.Parse(e.url); err == nil {
				out = append(out, c.probesLocked(u)...)
			}
		case e.state == Queued && !in:
			e.state = Waiting
			out = append(out, e)
		}
	}
	return out
}

// rebuildQueueLocked lines the queued URLs up shallowest first, in the
// order they were found: a breadth-first crawl.
func (c *Crawler) rebuildQueueLocked() {
	c.queue = c.queue[:0]
	for _, e := range c.ordered() {
		if e.state == Queued {
			c.queue = append(c.queue, e)
		}
	}
}

func (c *Crawler) queuedLocked() int {
	n := 0
	for _, e := range c.queue {
		if e.state == Queued {
			n++
		}
	}
	return n
}

// ordered is every URL by depth, then the order it was found.
func (c *Crawler) ordered() []*entry {
	list := make([]*entry, 0, len(c.urls))
	for _, e := range c.urls {
		list = append(list, e)
	}
	sortEntries(list)
	return list
}

// inScope reports whether a URL is one this crawl fetches: a target covers
// it, and the settings don't leave that target out.
func (c *Crawler) inScope(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	id, ok := c.scope(u)
	return ok && !c.settings.excludes(id)
}

// inHostScope reports whether a target covers a URL's host, ignoring the
// path rules. It decides whether a redirect to the URL is followed.
func (c *Crawler) inHostScope(raw string) bool {
	if c.opts.HostScope == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	id, ok := c.opts.HostScope(u.Host)
	return ok && !c.settings.excludes(id)
}

// scoped reports whether an entry should be crawled now: in ordinary path
// scope, or — for a URL a redirect led to — in host scope.
func (c *Crawler) scoped(e *entry) bool {
	return c.inScope(e.url) || (e.viaRedirect && c.inHostScope(e.url))
}

// isRedirect reports whether a status is one whose Location names where the
// resource moved to.
func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func (c *Crawler) scope(u *url.URL) (string, bool) {
	if c.opts.Scope == nil {
		return "", false
	}
	return c.opts.Scope(u.Host, u.RequestURI())
}

// normalize makes the form a URL is known by: http(s) only, a lower-case
// host without the scheme's default port, no fragment, and "/" for an
// empty path.
func normalize(raw string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, false
	}
	u.Host = strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "http" && p == "80") || (u.Scheme == "https" && p == "443") {
		u.Host = strings.TrimSuffix(u.Host, ":"+p)
	}
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path, u.RawPath = "/", ""
	}
	return u, true
}

// crawlable reports whether a URL may hold more links: images, fonts,
// media and downloads are left to the Explorer, which already has them from
// the page that linked to them, as are an API description's templated
// paths (/users/{id}), which it gives filled in when it can.
func crawlable(u *url.URL) bool {
	if strings.ContainsAny(u.Path, "{}") {
		return false
	}
	switch entities.KindOf("", path.Base(u.Path)) {
	case entities.KindImage, entities.KindFont, entities.KindMedia, entities.KindFile:
		return false
	}
	return true
}

// specWithoutGet reports whether a link is an API description's path
// that declares operations but no GET (or HEAD): "openapi:POST,PUT /x".
func specWithoutGet(detail string) bool {
	rest, ok := strings.CutPrefix(detail, "openapi:")
	if !ok {
		return false
	}
	methods, _, _ := strings.Cut(rest, " ")
	if methods == "?" {
		return false
	}
	for _, m := range strings.Split(methods, ",") {
		if m == "GET" || m == "HEAD" {
			return false
		}
	}
	return true
}

// requestFor is the GET the crawler sends for u.
func requestFor(u *url.URL) []byte {
	var b strings.Builder
	b.WriteString("GET " + u.RequestURI() + " HTTP/1.1\r\n")
	b.WriteString("Host: " + u.Host + "\r\n")
	b.WriteString("User-Agent: " + userAgent + "\r\n")
	b.WriteString("Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\n")
	b.WriteString("Accept-Encoding: gzip, deflate, br\r\n")
	b.WriteString("Connection: close\r\n\r\n")
	return []byte(b.String())
}

// hostPort is where u is served: its host and port, the scheme's default
// when it names none.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// ---- Rate ------------------------------------------------------------------

// limiter spaces request starts at least interval apart, with no burst.
// Each caller reserves the next slot before waiting for it, so concurrent
// workers queue for slots rather than sharing one, and a slot reserved by
// a request that is then abandoned is not given back.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func (l *limiter) setRate(perSecond float64) {
	l.mu.Lock()
	l.interval = time.Duration(float64(time.Second) / perSecond)
	l.mu.Unlock()
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
