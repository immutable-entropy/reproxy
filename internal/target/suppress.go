package target

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Suppression keeps hosts out of the Related list: a host one matches is
// never suggested as related to the targets, for as long as the
// suppression stands. Pattern is written like a target's address — a
// domain, a wildcard such as *.doubleclick.net, an IP address or a CIDR
// range — and matched the same way, ignoring case and port.
//
// Suppressions are a user setting (config.Targets), not part of an
// engagement: hosts not worth suggesting (analytics, ad networks, a CDN
// everyone uses) are the same from one engagement to the next.
type Suppression struct {
	Pattern string    `json:"pattern"`
	Note    string    `json:"note,omitempty"`
	Added   time.Time `json:"added,omitempty"`
}

// Normalized trims the pattern and note, and lower-cases the pattern.
func (s Suppression) Normalized() Suppression {
	s.Pattern = strings.ToLower(strings.TrimSpace(s.Pattern))
	s.Note = strings.TrimSpace(s.Note)
	return s
}

// Kind is how the pattern is matched.
func (s Suppression) Kind() Kind { return DetectKind(s.Pattern) }

// Validate reports whether the pattern compiles.
func (s Suppression) Validate() error {
	if strings.TrimSpace(s.Pattern) == "" {
		return fmt.Errorf("suppression has no domain, IP address or CIDR range")
	}
	p := strings.ToLower(strings.TrimSpace(s.Pattern))
	if k := s.Kind(); (k == KindDomain || k == KindWildcard) && !hostPatternRe.MatchString(p) {
		return fmt.Errorf("%q is not a domain, *.wildcard, IP address or CIDR range", p)
	}
	if _, err := compileValue(s.Kind(), p); err != nil {
		return fmt.Errorf("suppression %q: %w", p, err)
	}
	return nil
}

// hostPatternRe is what a suppressed domain or wildcard may hold: labels
// of letters, digits, dashes and underscores, and the * and ? of a glob.
var hostPatternRe = regexp.MustCompile(`^[a-z0-9_*?-]+(\.[a-z0-9_*?-]+)*\.?$`)

// Describe says what the pattern covers, for people.
func (s Suppression) Describe() string {
	switch s.Kind() {
	case KindWildcard:
		return "Every host matching " + s.Pattern
	case KindIP:
		return "The IP address " + s.Pattern
	case KindCIDR:
		return "Every IP address in " + s.Pattern
	}
	return "The host " + s.Pattern + " only (use *." + s.Pattern + " for its subdomains)"
}

// ValidateSuppressions checks a list: every pattern valid, none twice.
func ValidateSuppressions(list []Suppression) error {
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		if err := s.Validate(); err != nil {
			return err
		}
		p := s.Normalized().Pattern
		if seen[p] {
			return fmt.Errorf("%q is suppressed twice", p)
		}
		seen[p] = true
	}
	return nil
}

// Suppressor matches hosts against a compiled list of suppressions. It is
// immutable; a nil Suppressor matches nothing.
type Suppressor struct {
	matchers []hostMatcher
}

// CompileSuppressions builds a Suppressor. Patterns that don't compile
// are left out (the settings are validated before they are applied).
func CompileSuppressions(list []Suppression) *Suppressor {
	s := &Suppressor{}
	for _, sup := range list {
		sup = sup.Normalized()
		if m, err := compileValue(sup.Kind(), sup.Pattern); err == nil {
			s.matchers = append(s.matchers, m)
		}
	}
	return s
}

// Suppressed reports whether host (a name or an IP address, with or
// without a port) is suppressed.
func (s *Suppressor) Suppressed(host string) bool {
	if s == nil || len(s.matchers) == 0 {
		return false
	}
	name := normalizeHost(host)
	ip, _ := netip.ParseAddr(name)
	ip = ip.Unmap()
	for i := range s.matchers {
		c := compiled{host: s.matchers[i]}
		if c.covers(name, ip, netip.Addr{}) {
			return true
		}
	}
	return false
}

// Matches reports whether this one suppression covers host.
func (s Suppression) Matches(host string) bool {
	return CompileSuppressions([]Suppression{s}).Suppressed(host)
}

// ParseSuppressions reads a suppression list as ExportSuppressions writes
// it: one pattern per line, optionally followed by "# note". Blank lines
// and lines that are only a comment are skipped. Lines that aren't valid
// are returned as errors (with their line numbers) and left out; the rest
// are returned, each once, stamped with added.
func ParseSuppressions(r io.Reader, added time.Time) ([]Suppression, []error) {
	var out []Suppression
	var errs []error
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern, note, _ := strings.Cut(line, "#")
		s := Suppression{Pattern: pattern, Note: note, Added: added}.Normalized()
		if f := strings.Fields(s.Pattern); len(f) != 1 {
			errs = append(errs, fmt.Errorf("line %d: %q is not one domain, IP address or CIDR range", n, s.Pattern))
			continue
		}
		if err := s.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", n, err))
			continue
		}
		if seen[s.Pattern] {
			continue
		}
		seen[s.Pattern] = true
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, err)
	}
	return out, errs
}

// ExportSuppressions writes a list ParseSuppressions reads back.
func ExportSuppressions(w io.Writer, list []Suppression) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "# reProxy suppression list: hosts never suggested as related to a target.")
	fmt.Fprintln(bw, "# One domain, *.wildcard, IP address or CIDR range per line; text after # is a note.")
	for _, s := range list {
		s = s.Normalized()
		if s.Note != "" {
			fmt.Fprintf(bw, "%s # %s\n", s.Pattern, strings.ReplaceAll(s.Note, "\n", " "))
		} else {
			fmt.Fprintln(bw, s.Pattern)
		}
	}
	return bw.Flush()
}

// MergeSuppressions adds to list the suppressions of more whose pattern it
// doesn't hold yet, and returns the result and how many were added.
func MergeSuppressions(list, more []Suppression) ([]Suppression, int) {
	have := make(map[string]bool, len(list))
	for _, s := range list {
		have[s.Normalized().Pattern] = true
	}
	added := 0
	for _, s := range more {
		s = s.Normalized()
		if have[s.Pattern] {
			continue
		}
		have[s.Pattern] = true
		list = append(list, s)
		added++
	}
	return list, added
}
