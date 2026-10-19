package entities

import (
	"net/netip"
	"sort"
	"strings"
	"time"
)

// ScopeFunc says which target covers a host (a bare host: no port), the
// way target.Set.MatchHost does, so this package needn't import target. It
// must be cheap and must not call back into the Store.
type ScopeFunc func(host string) (targetID string, ok bool)

// Related is a host no target covers that the targets' traffic refers to:
// a page on a target links to it, loads a script or font from it, names it
// in a Content-Security-Policy, redirects to it, or led the browser to
// request it. It is a candidate for scope, which the user can promote to a
// target; until then its entities are kept but not drawn in the Explorer.
type Related struct {
	// Host is a domain name or an IP address, lower case, without a port.
	Host string
	// IP is set when Host is an IP address.
	IP bool
	// URLs counts the URLs known on the host.
	URLs int
	// FoundOn are the hosts in scope whose traffic referred to it, each
	// once, and TargetIDs the targets covering them.
	FoundOn   []string
	TargetIDs []string
	FirstSeen time.Time
	LastSeen  time.Time
}

// BareHost is a host without its port (or an IPv6 address's brackets), as
// targets match it.
func BareHost(host string) string {
	host = strings.ToLower(host)
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end > 0 {
			return host[1:end]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && strings.Count(host, ":") == 1 {
		return host[:i]
	}
	return host
}

// linkHost is the bare host of a link's FromURL ("" when it has none).
func linkHost(fromURL string) string {
	_, rest, ok := strings.Cut(fromURL, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return BareHost(rest)
}

// SetScope tells the store which hosts the targets cover, and works out
// again which hosts are related. The app calls it at startup and after
// every change to the targets. A host that becomes related here for the
// first time (its target was removed, say) is announced like one found in
// traffic.
func (s *Store) SetScope(fn ScopeFunc) {
	s.mu.Lock()
	s.scope = fn
	s.rescan()
}

// SuppressFunc reports whether a host (bare, as BareHost gives it) is
// suppressed: never suggested as related, however the targets' traffic
// refers to it. It must be cheap and must not call back into the Store.
type SuppressFunc func(host string) bool

// SetSuppressed sets which hosts are kept out of Related (the user's
// suppression list) and works related hosts out again. A host whose
// suppression is lifted is related again at once, announced only if it
// never was before.
func (s *Store) SetSuppressed(fn SuppressFunc) {
	s.mu.Lock()
	s.suppressed = fn
	s.rescan()
}

// rescan works out the related hosts again, announcing what is new, and
// unlocks the store.
func (s *Store) rescan() {
	now := s.relatedLocked()
	var events []Event
	for h := range now {
		if !s.known[h] {
			s.known[h] = true
			events = append(events, Event{Kind: RelatedAdded, Host: h})
		}
	}
	s.related = now
	s.mu.Unlock()
	sort.Slice(events, func(i, j int) bool { return events[i].Host < events[j].Host })
	events = append(events, Event{Kind: RelatedChanged})
	s.emit(events...)
}

func (s *Store) suppressedLocked(host string) bool {
	return s.suppressed != nil && s.suppressed(host)
}

// relatesLocked reports whether links (new to a URL on host) make
// host related when it wasn't: host is out of scope and one of them came
// from a host in scope.
func (s *Store) relatesLocked(host string, links []Link) bool {
	if s.scope == nil || len(links) == 0 || s.related[host] || s.suppressedLocked(host) {
		return false
	}
	if _, ok := s.scope(host); ok {
		return false
	}
	for _, l := range links {
		if from := linkHost(l.FromURL); from != "" && from != host {
			if _, ok := s.scope(from); ok {
				return true
			}
		}
	}
	return false
}

// markRelatedLocked records host as related, returning the announcement
// when it is news.
func (s *Store) markRelatedLocked(host string) []Event {
	s.related[host] = true
	if s.known[host] {
		return nil
	}
	s.known[host] = true
	return []Event{{Kind: RelatedAdded, Host: host}}
}

// relatedLocked works out every related host from scratch, leaving out
// the suppressed ones.
func (s *Store) relatedLocked() map[string]bool {
	out := s.candidatesLocked()
	for h := range out {
		if s.suppressedLocked(h) {
			delete(out, h)
		}
	}
	return out
}

// candidatesLocked is every host that would be related, suppressed or not:
// out of scope, and referred to from a host in scope.
func (s *Store) candidatesLocked() map[string]bool {
	out := map[string]bool{}
	if s.scope == nil {
		return out
	}
	covered := map[string]bool{}
	in := func(h string) bool {
		c, ok := covered[h]
		if !ok {
			_, c = s.scope(h)
			covered[h] = c
		}
		return c
	}
	for _, e := range s.list {
		// A host's links are on its URLs, or on the name host itself when
		// it was found without one (DiscoverHost).
		if e.Type != TypeURL && !e.IsName() {
			continue
		}
		h := BareHost(e.Host)
		if out[h] || in(h) {
			continue
		}
		for _, l := range e.Links {
			if from := linkHost(l.FromURL); from != "" && from != h && in(from) {
				out[h] = true
				break
			}
		}
	}
	return out
}

// Related lists the related hosts, the most recently found first.
// Suppressed hosts are not among them.
func (s *Store) Related() []Related {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope == nil {
		return nil
	}
	return s.describeLocked(s.relatedLocked())
}

// Suppressed lists the hosts that would be related but are suppressed,
// the most recently found first: what a suppression is hiding.
func (s *Store) Suppressed() []Related {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scope == nil || s.suppressed == nil {
		return nil
	}
	hidden := s.candidatesLocked()
	for h := range hidden {
		if !s.suppressedLocked(h) {
			delete(hidden, h)
		}
	}
	return s.describeLocked(hidden)
}

// describeLocked summarises each of the hosts.
func (s *Store) describeLocked(related map[string]bool) []Related {
	byHost := map[string]*Related{}
	for _, e := range s.list {
		h := BareHost(e.Host)
		if (e.Type != TypeURL && !e.IsName()) || !related[h] {
			continue
		}
		r := byHost[h]
		if r == nil {
			addr, err := netip.ParseAddr(h)
			r = &Related{Host: h, IP: err == nil && addr.IsValid(), FirstSeen: e.FirstSeen, LastSeen: e.LastSeen}
			byHost[h] = r
		}
		if e.Type == TypeURL {
			r.URLs++
		}
		if e.FirstSeen.Before(r.FirstSeen) {
			r.FirstSeen = e.FirstSeen
		}
		if e.LastSeen.After(r.LastSeen) {
			r.LastSeen = e.LastSeen
		}
		for _, l := range e.Links {
			from := linkHost(l.FromURL)
			if from == "" || from == h {
				continue
			}
			if id, ok := s.scope(from); ok {
				addUnique(&r.FoundOn, from)
				addUnique(&r.TargetIDs, id)
			}
		}
	}
	out := make([]Related, 0, len(byHost))
	for _, r := range byHost {
		sort.Strings(r.FoundOn)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.After(out[j].FirstSeen)
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// OnHost returns the URLs on a host (any port or scheme), in the order
// they were first seen: what a related host's details list.
func (s *Store) OnHost(host string) []Entity {
	host = BareHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entity
	for _, e := range s.list {
		if e.Type == TypeURL && BareHost(e.Host) == host {
			out = append(out, e.Clone())
		}
	}
	return out
}

// Mapped returns the entities the Explorer draws, under the target with
// the given ID, or under every target when id is "", related hosts
// included. Drawn is the same with a say in that.
func (s *Store) Mapped(id string) []Entity { return s.Drawn(id, true) }

// Drawn is Mapped with a say in whether the related hosts come too. A
// related host is a candidate for scope rather than part of it (see
// Related), so the Explorer leaves it out until the user promotes it or
// asks to see it; what is known about it stays in the store either way,
// and the Targets section lists it whatever the Explorer shows.
//
// A URL is drawn when a target covers its host or its host has been
// visited (traffic collected with no target, or with targets not
// required); a URL only discovered on a host out of scope is left to
// Related. A suppressed host is never drawn, however its URLs were found
// and whenever they turn up: what is known about it stays in the store, so
// lifting the suppression puts it back at once. Hosts and domains are
// drawn when something under them is, a host a target covers always is,
// and so are the entities the targets stand for (SyncTargets).
//
// Each entity's TargetID is the one it is drawn under: for a URL
// requested, the target it was collected under, otherwise whichever target
// covers its host now, so promoting a related host puts its URLs under the
// new target. A host or domain takes the target covering its name, the
// target that names it, or else that of the first thing drawn under it.
func (s *Store) Drawn(id string, related bool) []Entity {
	s.mu.Lock()
	defer s.mu.Unlock()
	hidden := map[string]bool{}
	suppressed := func(h string) bool {
		v, ok := hidden[h]
		if !ok {
			v = s.suppressedLocked(h)
			hidden[h] = v
		}
		return v
	}
	type hostScope struct {
		target  string
		covered bool
		visited bool
	}
	hosts := map[string]*hostScope{}
	scopeOf := func(h string) *hostScope {
		hs := hosts[h]
		if hs == nil {
			hs = &hostScope{}
			if s.scope != nil {
				hs.target, hs.covered = s.scope(h)
			}
			hosts[h] = hs
		}
		return hs
	}
	for _, e := range s.list {
		if e.Type == TypeURL && e.Visited() {
			scopeOf(BareHost(e.Host)).visited = true
		}
	}

	// drawn holds what is drawn, with the target it is drawn under.
	drawn := map[*Entity]string{}
	draw := func(e *Entity, tid string) {
		for at := e; at != nil; at = s.byKey[at.Parent] {
			if _, ok := drawn[at]; ok {
				return
			}
			t := tid
			if at.Type != TypeURL {
				if hs := scopeOf(BareHost(at.Host)); hs.covered {
					t = hs.target
				} else if sd, ok := s.seeds[at.Key]; ok {
					t = sd.TargetID
				}
			}
			drawn[at] = t
		}
	}
	for _, e := range s.list {
		if e.Type != TypeURL {
			continue
		}
		// A suppressed host is left out here, so neither its URLs nor any
		// that turn up while it is suppressed are drawn. A related host
		// goes the same way while the caller doesn't want them: drawing
		// nothing on it is what keeps its domain out too, since only what
		// is drawn pulls its ancestors in.
		h := BareHost(e.Host)
		hs := scopeOf(h)
		if suppressed(h) || (!related && s.related[h]) || (s.scope != nil && !hs.covered && !hs.visited) {
			continue
		}
		tid := e.TargetID
		if hs.covered && (!e.Visited() || tid == "") {
			tid = hs.target
		}
		if id != "" && tid != id {
			continue
		}
		draw(e, tid)
	}
	// A host a target covers is drawn whether or not anything is under
	// it: a name a plugin found, say.
	for _, e := range s.list {
		if e.Type != TypeHost {
			continue
		}
		h := BareHost(e.Host)
		hs := scopeOf(h)
		if !hs.covered || suppressed(h) || (id != "" && hs.target != id) {
			continue
		}
		draw(e, hs.target)
	}
	for key, sd := range s.seeds {
		e := s.byKey[key]
		if e == nil || suppressed(BareHost(e.Host)) || (id != "" && sd.TargetID != id) {
			continue
		}
		draw(e, sd.TargetID)
	}

	out := make([]Entity, 0, len(drawn))
	for _, e := range s.list { // in the order first seen
		if tid, ok := drawn[e]; ok {
			c := e.Clone()
			c.TargetID = tid
			out = append(out, c)
		}
	}
	return out
}
