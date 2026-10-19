package target

import (
	"fmt"
	"net/netip"
	"regexp"
)

// Set is a compiled list of targets. It is immutable once built, so the
// proxy can read one from every connection without locking (see Store.Set).
//
// Matching runs on the proxy's hot path: everything that can be decided once
// is decided at compile time, and Match allocates nothing.
type Set struct {
	entries []compiled
}

type compiled struct {
	Match
	host  hostMatcher
	rules []compiledRule
}

// hostMatcher is one target's address. Exactly one field is set: exact for a
// plain domain, re for a wildcard or regex, prefix for an IP or CIDR range.
type hostMatcher struct {
	exact  string
	re     *regexp.Regexp
	prefix netip.Prefix
}

type compiledRule struct {
	allow bool
	re    *regexp.Regexp
}

// Match identifies the target a request belongs to. It carries the label and
// colour so the UI can mark a flow without looking the target up again — and
// still mark flows from a target since deleted.
type Match struct {
	ID    string
	Name  string
	Color string
	// Intercept is the target's interception configuration, so the proxy
	// can decide what to hold without a second lookup.
	Intercept Intercept
}

// Compile builds a Set. Disabled targets are left out. A target whose value
// or patterns don't compile is an error: Store validates before saving, so
// this only fires on a hand-edited file.
func Compile(list []Target) (*Set, error) {
	s := &Set{}
	for _, t := range list {
		if !t.Enabled {
			continue
		}
		hm, err := compileValue(t.Kind, t.Value)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", t.Label(), err)
		}
		c := compiled{
			Match: Match{ID: t.ID, Name: t.Label(), Color: t.Color, Intercept: t.Intercept},
			host:  hm,
		}
		for i, r := range t.Rules {
			re, err := compilePattern(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("target %q, rule %d: %w", t.Label(), i+1, err)
			}
			c.rules = append(c.rules, compiledRule{allow: r.Action == Allow, re: re})
		}
		s.entries = append(s.entries, c)
	}
	return s, nil
}

// Empty reports whether the set would match nothing, so the caller can skip
// the work entirely.
func (s *Set) Empty() bool { return s == nil || len(s.entries) == 0 }

// Match finds the target a request belongs to.
//
// Targets are tried in order, and the first one whose address covers the host
// owns the request: its rules then decide, in order, with the first matching
// rule winning (like firewall rules). A request a target denies is out of
// scope — another target further down does not get to claim it, so a deny
// always means denied.
//
// host is the request's Host (a port is ignored); ip is the address the
// connection actually reached, which IP and CIDR targets match against as
// well as a host that is an IP literal; path is the request target.
func (s *Set) Match(host string, ip netip.Addr, path string) (Match, bool) {
	if s.Empty() {
		return Match{}, false
	}
	name := normalizeHost(host)
	hostIP, _ := netip.ParseAddr(name)
	ip = ip.Unmap()
	hostIP = hostIP.Unmap()

	for i := range s.entries {
		e := &s.entries[i]
		if !e.covers(name, hostIP, ip) {
			continue // not this target's host; try the next
		}
		for _, r := range e.rules {
			if r.re.MatchString(path) {
				if r.allow {
					return e.Match, true
				}
				return Match{}, false // denied by the target that owns it
			}
		}
		return Match{}, false // no rule matched: not in scope
	}
	return Match{}, false
}

// MatchHost finds the target a host belongs to, ignoring path rules: it is
// for a DNS lookup, which has a name (and, once answered, addresses) but no
// request target. The first target whose address covers the host, or any
// of ips, owns it, as in Match; a target whose rules allow none of its paths
// still owns its host's lookups.
func (s *Set) MatchHost(host string, ips ...netip.Addr) (Match, bool) {
	if s.Empty() {
		return Match{}, false
	}
	name := normalizeHost(host)
	hostIP, _ := netip.ParseAddr(name)
	hostIP = hostIP.Unmap()
	for i := range s.entries {
		e := &s.entries[i]
		if e.covers(name, hostIP, netip.Addr{}) {
			return e.Match, true
		}
		for _, ip := range ips {
			if e.covers(name, hostIP, ip.Unmap()) {
				return e.Match, true
			}
		}
	}
	return Match{}, false
}

// covers reports whether this target's address includes the host.
func (c *compiled) covers(host string, hostIP, connIP netip.Addr) bool {
	switch {
	case c.host.exact != "":
		return c.host.exact == host
	case c.host.re != nil:
		return c.host.re.MatchString(host)
	case c.host.prefix.IsValid():
		if hostIP.IsValid() && c.host.prefix.Contains(hostIP) {
			return true
		}
		return connIP.IsValid() && c.host.prefix.Contains(connIP)
	}
	return false
}
