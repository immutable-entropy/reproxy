package app

import (
	"net/netip"
	"strings"

	"reproxy/internal/crawl"
	"reproxy/internal/entities"
)

// Autocrawl (package crawl) is wired here: it dials through the upstream
// chain in force, with the lookup made by a.dns (dialResolved), so a crawl
// reaches a target the same way the browser's traffic does and never goes
// out on its own. Its state lives in the open engagement's capture.db and
// follows the engagement like the findings do (see followEngagement).

func (a *App) newCrawler() {
	a.crawler = crawl.New(crawl.Options{
		Store:     a.store,
		Dial:      a.dialResolved,
		Scope:     a.crawlScope,
		HostScope: a.crawlHostScope,
		Seeds:     a.crawlSeeds,
	})
}

// Crawler implements ui.Controller: the engagement's Autocrawl.
func (a *App) Crawler() *crawl.Crawler { return a.crawler }

// crawlScope implements crawl.ScopeFunc: a URL is crawled when a target
// covers its host and allows its path, as the proxy would collect it.
// With no targets nothing is.
func (a *App) crawlScope(host, path string) (string, bool) {
	set := a.targets.Set()
	if set.Empty() {
		return "", false
	}
	m, ok := set.Match(host, netip.Addr{}, path)
	return m.ID, ok
}

// crawlHostScope implements crawl.Options.HostScope: whether a target covers
// a host at all, ignoring its path rules. It is consulted to follow a
// redirect into a target even where the path rules wouldn't harvest the
// destination path.
func (a *App) crawlHostScope(host string) (string, bool) {
	set := a.targets.Set()
	if set.Empty() {
		return "", false
	}
	m, ok := set.MatchHost(host)
	return m.ID, ok
}

// crawlSeeds is where a crawl starts: the root of every origin the
// Explorer knows, every concrete URL requested or linked to, and, for each
// target's name the Explorer has no origin for yet, its root over HTTPS
// and HTTP. The crawler keeps those under the targets the crawl covers, and
// adds each origin's robots.txt and sitemap (crawl.SitemapPaths) and, when
// asked to look for API docs, its crawl.APIDocPaths.
func (a *App) crawlSeeds() []string {
	var out []string
	origins := map[string]bool{}
	for _, e := range a.entities.List() {
		switch {
		case e.Type == entities.TypeHost && e.Scheme != "":
			origins[entities.BareHost(e.Host)] = true
			out = append(out, e.Key+"/")
		case e.Type == entities.TypeURL && e.URLAttrs != nil:
			for _, p := range e.URLAttrs.Examples {
				if !strings.ContainsAny(p, "{}") {
					out = append(out, e.Scheme+"://"+e.Host+p)
				}
			}
		}
	}
	for _, s := range entitySeeds(a.targets.List()) {
		if origins[s.Name] {
			continue
		}
		out = append(out, "https://"+s.Name+"/", "http://"+s.Name+"/")
	}
	return out
}
