// Package target scopes the traffic reproxy works on.
//
// A target is one host in scope: a domain, an IP address or a CIDR range,
// plus the rules that say which paths within it are in scope. The proxy asks
// a compiled Set about every request, so traffic that belongs to no target
// can be passed straight through without being recorded — see Set.Match.
//
// It is core (it decides what is captured at all) and has no Qt dependency:
// the UI edits targets through a Store, and the proxy reads the compiled Set
// the Store publishes. The targets themselves belong to the open engagement
// (see package engagement), which is where the Store's file lives.
package target

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"reproxy/internal/randid"
)

// Kind is how a target's value is matched against a request.
type Kind string

const (
	// KindDomain matches one host exactly, ignoring case and any port.
	KindDomain Kind = "domain"
	// KindWildcard matches a host against a glob such as "*.example.com".
	KindWildcard Kind = "wildcard"
	// KindRegex matches a host against a regular expression. The pattern
	// must match the whole host: a partial match would let
	// "example.com.attacker.test" into a target for "example.com".
	KindRegex Kind = "regex"
	// KindIP matches one IP address, either the host itself or the address
	// the connection reached.
	KindIP Kind = "ip"
	// KindCIDR matches an IP address inside a range, e.g. "10.0.0.0/8".
	KindCIDR Kind = "cidr"
)

// Kinds lists the target kinds in the order the UI offers them.
var Kinds = []Kind{KindDomain, KindWildcard, KindRegex, KindIP, KindCIDR}

// Action is what a rule does with a request that matches it.
type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Rule decides whether a request to a target's host is in scope. Pattern is
// a regular expression matched (case-insensitively, anywhere in the string)
// against the request target, e.g. "/orders/12?full=1".
type Rule struct {
	Action  Action `json:"action"`
	Pattern string `json:"pattern"`
}

// DefaultRule ends every target's rule list and cannot be removed: whatever
// the target's host covers is in scope unless an earlier rule denies it.
// Rules above it narrow that down, in order, like firewall rules.
func DefaultRule() Rule { return Rule{Action: Allow, Pattern: ".*"} }

// IsDefault reports whether r is the undeletable final rule.
func (r Rule) IsDefault() bool { return r.Action == Allow && r.Pattern == DefaultRule().Pattern }

// Target is one host in scope: exactly one domain, IP address or CIDR range,
// and the rules that narrow it to the paths worth harvesting.
type Target struct {
	// ID is a stable random identifier. It tags every flow collected under
	// this target.
	ID string `json:"id"`
	// Kind is how Value is matched.
	Kind Kind `json:"kind"`
	// Value is the domain, IP address or CIDR range itself.
	Value string `json:"value"`
	// Name is optional: a label for this target. Label falls back to Value
	// when it is empty, so a target never shows up nameless.
	Name string `json:"name,omitempty"`
	// Notes is free-form description kept with the target.
	Notes string `json:"notes,omitempty"`
	// Color is "#rrggbb", assigned on creation and editable afterwards. It
	// marks this target's traffic in the request list.
	Color string `json:"color"`
	// Enabled is cleared to park a target without deleting it; a disabled
	// target matches nothing.
	Enabled bool `json:"enabled"`
	// Rules narrow the target's host down to the paths in scope, in
	// priority order, ending with DefaultRule.
	Rules []Rule `json:"rules,omitempty"`
	// Intercept says what the target holds for the user when interception
	// is switched on (Settings used to; it is per target now).
	Intercept Intercept `json:"intercept,omitempty"`
	Created   time.Time `json:"created"`
}

// Intercept is a target's interception configuration: which of its messages
// are held for the user when interception is on.
//
// DNS adds the target's name lookups to what is held: a lookup's query is
// held when Requests is set and its answer when Responses is, so the user
// can rename what is looked up, change the addresses it resolves to, or
// drop it so the lookup fails. It is off by default, because every new
// connection to a host can start a lookup.
type Intercept struct {
	Requests  bool `json:"requests,omitempty"`
	Responses bool `json:"responses,omitempty"`
	DNS       bool `json:"dns,omitempty"`
}

// New returns a target ready to be saved: a fresh ID, the given value and
// colour, enabled, with just the default rule. An empty kind is detected
// from the value.
func New(kind Kind, value, color string) Target {
	if kind == "" {
		kind = DetectKind(value)
	}
	return Target{
		ID:        NewID(),
		Kind:      kind,
		Value:     value,
		Color:     color,
		Enabled:   true,
		Rules:     []Rule{DefaultRule()},
		Intercept: Intercept{Requests: true},
		Created:   time.Now(),
	}
}

// Adopt readies a target that was worked out somewhere else — read from an
// imported scope file rather than typed — giving it the identity and the
// defaults New gives one the user added, while keeping what the import
// decided: the kind and address, the name and notes, the path rules, and
// whether it is enabled (which the caller always states, since a scope file
// can describe a target that is switched off).
func Adopt(t Target, color string) Target {
	ready := New(t.Kind, t.Value, color)
	ready.Name = t.Name
	ready.Notes = t.Notes
	ready.Enabled = t.Enabled
	if len(t.Rules) > 0 {
		ready.Rules = t.Rules
	}
	if t.Intercept != (Intercept{}) {
		ready.Intercept = t.Intercept
	}
	return ready.Normalized()
}

// NewID returns a random target identifier.
func NewID() string { return randid.Hex(8) }

// DetectKind guesses the kind of a typed value, so the user can paste
// "10.0.0.0/8" or "*.example.test" without also picking the kind. It never
// guesses Regex: that is always a deliberate choice.
func DetectKind(value string) Kind {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return KindDomain
	case strings.Contains(value, "/"):
		if _, err := netip.ParsePrefix(value); err == nil {
			return KindCIDR
		}
	case strings.ContainsAny(value, "*?"):
		return KindWildcard
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return KindIP
	}
	return KindDomain
}

// Label is what the UI calls this target: its name, or the value itself when
// it has no name.
func (t Target) Label() string {
	if name := strings.TrimSpace(t.Name); name != "" {
		return name
	}
	return strings.TrimSpace(t.Value)
}

// Display renders the target's address, qualified by its kind when the kind
// isn't obvious from the value itself.
func (t Target) Display() string {
	if t.Kind == KindDomain || t.Kind == KindIP || t.Kind == KindCIDR {
		return t.Value
	}
	return string(t.Kind) + ":" + t.Value
}

// Normalized returns t with its fields tidied: trimmed strings, a kind
// detected from the value when none was set, and a rule list that ends with
// the default rule. Saving goes through this, so the undeletable rule is
// restored however the list was edited.
func (t Target) Normalized() Target {
	t.Name = strings.TrimSpace(t.Name)
	t.Notes = strings.TrimSpace(t.Notes)
	t.Value = strings.TrimSpace(t.Value)
	t.Color = strings.ToLower(strings.TrimSpace(t.Color))
	if t.Kind == "" {
		t.Kind = DetectKind(t.Value)
	}

	// Everything but the final default rule, then the default rule back.
	rules := make([]Rule, 0, len(t.Rules)+1)
	for _, r := range t.Rules {
		r.Pattern = strings.TrimSpace(r.Pattern)
		if r.Pattern != "" && !r.IsDefault() {
			rules = append(rules, r)
		}
	}
	t.Rules = append(rules, DefaultRule())
	return t
}

// Validate reports whether t can be saved. It compiles the value and every
// pattern, so anything that would fail at match time is refused at edit time
// instead. The name is optional; the address is not.
func (t Target) Validate() error {
	switch {
	case t.ID == "":
		return fmt.Errorf("target has no ID")
	case strings.TrimSpace(t.Value) == "":
		return fmt.Errorf("target has no domain, IP address or CIDR range")
	case !validColor(t.Color):
		return fmt.Errorf("target %q: %q is not a colour like #3b6ef5", t.Label(), t.Color)
	}
	if _, err := compileValue(t.Kind, t.Value); err != nil {
		return fmt.Errorf("target %q: %w", t.Label(), err)
	}
	for i, r := range t.Rules {
		switch r.Action {
		case Allow, Deny:
		default:
			return fmt.Errorf("target %q, rule %d: unknown action %q", t.Label(), i+1, r.Action)
		}
		if _, err := compilePattern(r.Pattern); err != nil {
			return fmt.Errorf("target %q, rule %d: %w", t.Label(), i+1, err)
		}
	}
	return nil
}

func validColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	_, err := hex.DecodeString(s[1:])
	return err == nil
}

// Palette is the colours handed to new targets, in order. They are picked to
// stay distinguishable from each other on both the light and dark themes.
var Palette = []string{
	"#3b6ef5", // blue
	"#2e9e4f", // green
	"#d08700", // amber
	"#d6453d", // red
	"#8b5cf6", // violet
	"#0ea5a4", // teal
	"#ec4899", // pink
	"#84a700", // lime
}

// PickColor returns the first palette colour none of used holds, falling back
// to cycling through the palette once every colour is taken.
func PickColor(used []string) string {
	taken := make(map[string]bool, len(used))
	for _, c := range used {
		taken[strings.ToLower(c)] = true
	}
	for _, c := range Palette {
		if !taken[c] {
			return c
		}
	}
	return Palette[len(used)%len(Palette)]
}

// compilePattern builds a rule's path matcher: case-insensitive, and matching
// anywhere in the request target, so "/admin" catches "/admin/users".
func compilePattern(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("pattern is empty")
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	return re, nil
}

// compileValue builds a target's host matcher.
func compileValue(kind Kind, value string) (hostMatcher, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return hostMatcher{}, fmt.Errorf("value is empty")
	}
	switch kind {
	case KindDomain:
		// A domain typed with glob characters is matched as a wildcard, so a
		// "raw" *.example.com entry works even when the kind was left on
		// Domain rather than switched to Wildcard.
		if strings.ContainsAny(value, "*?") {
			re, err := compileGlob(value)
			if err != nil {
				return hostMatcher{}, err
			}
			return hostMatcher{re: re}, nil
		}
		return hostMatcher{exact: normalizeHost(value)}, nil
	case KindWildcard:
		re, err := compileGlob(value)
		if err != nil {
			return hostMatcher{}, err
		}
		return hostMatcher{re: re}, nil
	case KindRegex:
		// Anchored: a partial match would let a lookalike host through.
		re, err := regexp.Compile("(?i)^(?:" + value + ")$")
		if err != nil {
			return hostMatcher{}, fmt.Errorf("invalid regex: %w", err)
		}
		return hostMatcher{re: re}, nil
	case KindIP:
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return hostMatcher{}, fmt.Errorf("%q is not an IP address", value)
		}
		return hostMatcher{prefix: netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())}, nil
	case KindCIDR:
		p, err := netip.ParsePrefix(value)
		if err != nil {
			return hostMatcher{}, fmt.Errorf("%q is not a CIDR range", value)
		}
		return hostMatcher{prefix: p.Masked()}, nil
	}
	return hostMatcher{}, fmt.Errorf("unknown kind %q", kind)
}

// normalizeHost lower-cases a host and drops any port, so "API.Example.com:8443"
// and "api.example.com" are the same host.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(host, "[") { // [::1]:8080
		if end := strings.Index(host, "]"); end > 0 {
			return host[1:end]
		}
		return host
	}
	if h, _, ok := strings.Cut(host, ":"); ok {
		return h
	}
	return host
}

// compileGlob builds a host matcher for a wildcard: anchored and
// case-insensitive, with * matching any run of characters and ? any single
// one. A label-crossing * means *.example.com also covers deep subdomains.
func compileGlob(glob string) (*regexp.Regexp, error) {
	return regexp.Compile("(?i)^" + wildcardToRegex(glob) + "$")
}

func wildcardToRegex(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}
