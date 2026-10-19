package target

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Importing a Burp Suite scope
//
// Burp keeps its scope in the project's options as two lists, include and
// exclude, and HackerOne hands a program's scope out in the same shape. An
// entry is matched the way Burp matches it: host, file (the path) and port
// are regular expressions and protocol is a name, or — in simple mode — one
// "prefix" that a URL has to start with.
//
// The shapes don't line up one to one, so the import is deliberate about it:
//
//   - A target has no port or protocol, because the proxy matches a host
//     whatever port it is reached on. HackerOne writes one entry per scheme
//     (http on 80, https on 443), so entries that differ only there collapse
//     into the one target; a port that is neither is kept in the notes.
//   - A host pattern that is a plain anchored literal becomes a Domain, and
//     ".*\.example\.com" a Wildcard, because those read far better in the UI
//     than a regular expression. Anything else stays a Regex, rewritten so
//     that Burp's unanchored search still means the same thing once reproxy
//     anchors it. Nothing is ever widened to make it fit.
//   - A file pattern narrows the target with rules, which are first-match-wins
//     (see Set.Match): the paths in scope are allowed and a Deny ".*" below
//     them puts everything else out of scope.
//   - An exclude for a host that is included becomes a Deny rule above that
//     target's allows. One for a host that is not becomes a target of its own
//     that denies, ordered before the rest, which is how a single host is
//     taken back out of a wildcard.

// ScopeImport is what reading a scope file produced: the targets it describes,
// the entries that could not become one, and anything the user has to know
// about what was lost in translation.
type ScopeImport struct {
	// Targets are ready for Store.AddAll: no ID or colour yet.
	Targets []Target
	// Skipped holds the entries no target could be made from.
	Skipped []SkippedEntry
	// Notes are things the import did that the user should see.
	Notes []string
}

// SkippedEntry is one scope entry the import left out, and why.
type SkippedEntry struct {
	Entry  string
	Reason string
}

// burpFile is the part of a Burp project's options (or of a scope file on its
// own) that holds the scope. All three nestings are accepted, because what a
// file holds depends on whether it came from the project, from the project
// options, or from HackerOne.
type burpFile struct {
	Target *struct {
		Scope *burpScope `json:"scope"`
	} `json:"target"`
	Scope   *burpScope  `json:"scope"`
	Include []burpEntry `json:"include"`
	Exclude []burpEntry `json:"exclude"`
}

type burpScope struct {
	AdvancedMode bool        `json:"advanced_mode"`
	Include      []burpEntry `json:"include"`
	Exclude      []burpEntry `json:"exclude"`
}

// burpEntry is one line of Burp's scope. In advanced mode Host, File, Port
// and Protocol are set; in simple mode Prefix is a URL the request must start
// with. Enabled is a pointer so that an entry that doesn't say defaults to on,
// as Burp's own UI does, rather than importing a disabled target.
type burpEntry struct {
	Enabled  *bool  `json:"enabled"`
	Host     string `json:"host"`
	File     string `json:"file"`
	Port     string `json:"port"`
	Protocol string `json:"protocol"`
	Prefix   string `json:"prefix"`
}

func (e burpEntry) on() bool { return e.Enabled == nil || *e.Enabled }

// describe writes an entry the way the report names it.
func (e burpEntry) describe() string {
	if e.Prefix != "" {
		return e.Prefix
	}
	if e.Host == "" {
		return "(no host)"
	}
	s := e.Host
	if e.File != "" && !allPaths(e.File) {
		s += " " + e.File
	}
	return s
}

// ParseBurpScope reads a Burp Suite scope file, or a HackerOne scope export in
// Burp's format, and returns the targets it describes. The error is only for a
// file that isn't one; an entry that can't be imported is reported in Skipped
// so that the rest still comes in.
func ParseBurpScope(r io.Reader) (*ScopeImport, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxScopeFile))
	if err != nil {
		return nil, fmt.Errorf("reading the scope file: %w", err)
	}
	var f burpFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("this is not a Burp Suite scope file: %w", err)
	}
	scope := &burpScope{Include: f.Include, Exclude: f.Exclude}
	switch {
	case f.Target != nil && f.Target.Scope != nil:
		scope = f.Target.Scope
	case f.Scope != nil:
		scope = f.Scope
	}
	if len(scope.Include) == 0 && len(scope.Exclude) == 0 {
		return nil, fmt.Errorf("this file holds no scope: expected a target.scope with an include list")
	}
	return buildScope(scope), nil
}

// maxScopeFile caps what is read: a scope list is tens of kilobytes, and a
// file far bigger than that is not one.
const maxScopeFile = 8 << 20

func buildScope(scope *burpScope) *ScopeImport {
	imp := &ScopeImport{}

	// The includes, collapsed by address: HackerOne writes an entry per
	// scheme, and a target covers every port either way.
	type group struct {
		kind    Kind
		value   string
		paths   []string // the file patterns in scope, "" for all of them
		all     bool     // some entry covered every path
		on      bool     // some entry was enabled
		notes   []string
		pattern string // the host pattern as written, when it is not the value
	}
	var order []string
	groups := map[string]*group{}

	for _, e := range scope.Include {
		kind, value, note, err := address(e)
		if err != nil {
			imp.Skipped = append(imp.Skipped, SkippedEntry{e.describe(), err.Error()})
			continue
		}
		key := string(kind) + "\x00" + strings.ToLower(value)
		g := groups[key]
		if g == nil {
			g = &group{kind: kind, value: value}
			if kind == KindRegex && e.Host != "" && e.Host != value {
				g.pattern = e.Host
			}
			groups[key] = g
			order = append(order, key)
		}
		g.on = g.on || e.on()
		if note != "" && !contains(g.notes, note) {
			g.notes = append(g.notes, note)
		}
		if p := pathPattern(e); p == "" {
			g.all = true
		} else if !contains(g.paths, p) {
			g.paths = append(g.paths, p)
		}
	}

	// The excludes: a Deny on the target that holds the host, or a target of
	// its own that denies, so one host can be taken out of a wildcard.
	denies := map[string][]string{} // group key -> patterns to deny
	var loose []*Target
	for _, e := range scope.Exclude {
		if !e.on() {
			continue // an exclusion Burp isn't applying either
		}
		kind, value, _, err := address(e)
		if err != nil {
			imp.Skipped = append(imp.Skipped, SkippedEntry{e.describe(), err.Error()})
			continue
		}
		key := string(kind) + "\x00" + strings.ToLower(value)
		pattern := pathPattern(e)
		if pattern == "" {
			pattern = DefaultRule().Pattern // the whole host is excluded
		}
		if _, ok := groups[key]; ok {
			denies[key] = append(denies[key], pattern)
			continue
		}
		// Not a host the includes name: a target that denies, which Match
		// consults first because it is ordered above them.
		t := Target{Kind: kind, Value: value, Enabled: true,
			Rules: []Rule{{Action: Deny, Pattern: pattern}},
			Notes: "Excluded by the imported scope file."}
		loose = append(loose, &t)
	}
	for _, t := range loose {
		imp.Targets = append(imp.Targets, *t)
	}

	for _, key := range order {
		g := groups[key]
		var rules []Rule
		for _, p := range denies[key] {
			rules = append(rules, Rule{Action: Deny, Pattern: p})
		}
		// An exclusion of every path leaves nothing in scope.
		if excludesEverything(denies[key]) {
			imp.Skipped = append(imp.Skipped, SkippedEntry{g.value,
				"the scope file excludes the whole host"})
			continue
		}
		if !g.all && len(g.paths) > 0 {
			for _, p := range g.paths {
				rules = append(rules, Rule{Action: Allow, Pattern: p})
			}
			// Nothing the file didn't name is in scope. Normalized puts the
			// undeletable Allow ".*" after this, where it is never reached.
			rules = append(rules, Rule{Action: Deny, Pattern: DefaultRule().Pattern})
		}
		notes := g.notes
		if g.pattern != "" {
			notes = append([]string{"Host pattern in the scope file: " + g.pattern}, notes...)
		}
		imp.Targets = append(imp.Targets, Target{
			Kind:    g.kind,
			Value:   g.value,
			Enabled: g.on,
			Rules:   rules,
			Notes:   strings.Join(notes, "\n"),
		})
	}

	imp.Notes = summarize(imp.Targets, scope)
	return imp
}

// summarize says what the import did that the user cannot see from the list
// of targets alone.
func summarize(targets []Target, scope *burpScope) []string {
	var notes []string
	if n := len(scope.Include) - len(targets); n > 0 && len(targets) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%s became %s: entries that differ only by protocol or port are one target, because a target covers a host on every port.",
			count(len(scope.Include), "scope entry", "scope entries"),
			count(len(targets), "target", "targets")))
	}
	var paths, off, regex int
	for _, t := range targets {
		if len(t.Rules) > 0 {
			paths++
		}
		if !t.Enabled {
			off++
		}
		if t.Kind == KindRegex {
			regex++
		}
	}
	switch {
	case paths == 1:
		notes = append(notes, "One of them restricts paths, so it carries Allow rules with a Deny below them; everything the file didn't name is out of scope.")
	case paths > 1:
		notes = append(notes, fmt.Sprintf(
			"%d of them restrict paths, so they carry Allow rules with a Deny below them; everything the file didn't name is out of scope.", paths))
	}
	switch {
	case off == 1:
		notes = append(notes, "One was disabled in the scope file and comes in disabled.")
	case off > 1:
		notes = append(notes, fmt.Sprintf("%d were disabled in the scope file and come in disabled.", off))
	}
	switch {
	case regex == 1:
		notes = append(notes, "One kept a regular expression, because its host pattern was more than a domain or a *.wildcard.")
	case regex > 1:
		notes = append(notes, fmt.Sprintf(
			"%d kept regular expressions, because their host patterns were more than a domain or a *.wildcard.", regex))
	}
	return notes
}

// count writes "1 target" or "3 targets": the forms differ enough between
// these notes that each one says which words it wants.
func count(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

// address works out a target's kind and value from a scope entry, with a note
// about anything in the entry a target cannot hold.
func address(e burpEntry) (Kind, string, string, error) {
	if e.Prefix != "" { // simple mode: a URL the request starts with
		return fromPrefix(e.Prefix)
	}
	if strings.TrimSpace(e.Host) == "" {
		return "", "", "", fmt.Errorf("the entry names no host")
	}
	kind, value := fromHostPattern(e.Host)
	if value == "" {
		return "", "", "", fmt.Errorf("%q is not a host pattern a target can hold", e.Host)
	}
	return kind, value, portNote(e), nil
}

// fromPrefix reads a simple-mode entry: the host of the URL, and the path as
// a note, since a prefix is a string match rather than a pattern.
func fromPrefix(prefix string) (Kind, string, string, error) {
	u, err := url.Parse(strings.TrimSpace(prefix))
	if err != nil || u.Host == "" {
		return "", "", "", fmt.Errorf("%q is not a URL", prefix)
	}
	note := ""
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		note = "The scope file covered only " + u.Path + " on this host."
	}
	host := normalizeHost(u.Host)
	return DetectKind(host), host, note, nil
}

// portNote describes a port the target cannot express. A target matches a
// host on any port, which is right for the 80/443 pair HackerOne writes, but
// a scope limited to one other port is wider once imported, so it is said.
func portNote(e burpEntry) string {
	port := strings.TrimSpace(e.Port)
	switch port {
	case "", "^80$", "^443$", "^(80|443)$", "^(443|80)$", ".*", "^.*$", "^\\d+$":
		return ""
	}
	return fmt.Sprintf("The scope file limited this to port %s; a target matches the host on every port.", port)
}

// pathPattern is the entry's file pattern, or "" when it covers every path.
func pathPattern(e burpEntry) string {
	p := strings.TrimSpace(e.File)
	if allPaths(p) {
		return ""
	}
	return p
}

// allPaths reports whether a file pattern covers everything, so that it needs
// no rule of its own.
func allPaths(pattern string) bool {
	switch strings.TrimSpace(pattern) {
	case "", "^/.*", "^/.*$", "/.*", ".*", "^.*$", "^.*", ".*$", "^/", "/":
		return true
	}
	return false
}

// excludesEverything reports whether these denials leave no path in scope.
func excludesEverything(patterns []string) bool {
	for _, p := range patterns {
		if allPaths(p) {
			return true
		}
	}
	return false
}

// literalHostRe is a host pattern that is only labels and escaped dots, so it
// names one host and can be a Domain.
var literalHostRe = regexp.MustCompile(`^[A-Za-z0-9_*?-]+(?:\\?\.[A-Za-z0-9_*?-]+)*$`)

// fromHostPattern turns Burp's host regular expression into a target address,
// preferring the kinds that read well: a plain literal is a Domain (or an IP),
// ".*\.example\.com" is a Wildcard, and anything else stays a Regex.
//
// Burp searches with the pattern rather than anchoring it, so an unanchored
// pattern is padded with ".*" on the open side: reproxy anchors a Regex
// target, and without the padding the import would narrow the scope.
func fromHostPattern(pattern string) (Kind, string) {
	pattern = strings.TrimSpace(pattern)
	body := pattern
	head := strings.HasPrefix(body, "^")
	body = strings.TrimPrefix(body, "^")
	tail := strings.HasSuffix(body, "$") && !strings.HasSuffix(body, `\$`)
	body = strings.TrimSuffix(body, "$")

	if head && tail {
		// A literal host, or a *.glob written as one.
		if lit, ok := unescapeHost(body); ok {
			if strings.ContainsAny(lit, "*?") {
				return KindWildcard, lit
			}
			return DetectKind(lit), lit
		}
		// ".*\.example\.com" and "[^.]*\.example\.com" are how a wildcard
		// comes out of Burp; both mean a subdomain of the literal rest.
		for _, prefix := range []string{`.*\.`, `[^.]*\.`, `.+\.`, `[^.]+\.`} {
			if rest, ok := strings.CutPrefix(body, prefix); ok {
				if lit, ok := unescapeHost(rest); ok && !strings.ContainsAny(lit, "*?") {
					return KindWildcard, "*." + lit
				}
			}
		}
		return KindRegex, body
	}

	// Unanchored: keep Burp's search semantics once reproxy anchors it.
	switch {
	case head:
		return KindRegex, body + ".*"
	case tail:
		return KindRegex, ".*" + body
	}
	return KindRegex, ".*" + body + ".*"
}

// unescapeHost undoes the backslashes in a host pattern, reporting false if
// what is left is not a plain host (or a glob of one).
func unescapeHost(body string) (string, bool) {
	if !literalHostRe.MatchString(body) {
		return "", false
	}
	return strings.ReplaceAll(body, `\.`, "."), true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
