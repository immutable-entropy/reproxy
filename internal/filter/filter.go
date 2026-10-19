// Package filter matches request-list rows against a user's filter query.
//
// It supports two modes, chosen by the filter bar's regex toggle: a plain
// case-insensitive substring search, and (regex on) a Go regular expression.
// It is Qt-free and has no internal dependencies, so the UI — and later
// plugins and LLM tooling — can share one definition of what a filter
// matches.
package filter

import (
	"regexp"
	"strings"
)

// Matcher decides whether a row's searchable text passes a filter. The zero
// value, and a Matcher compiled from empty text, match everything.
type Matcher struct {
	plain string         // lower-cased needle, substring mode
	re    *regexp.Regexp // non-nil in regex mode
	all   bool           // match everything (empty query)
}

// Compile builds a Matcher for text. When useRegex is true, text is a Go
// regular expression and a parse error is returned if it is malformed;
// matching is then case-sensitive (add "(?i)" for insensitive, as in VS
// Code). Otherwise text is a plain substring matched case-insensitively.
// Empty or whitespace-only text matches everything.
func Compile(text string, useRegex bool) (*Matcher, error) {
	if strings.TrimSpace(text) == "" {
		return &Matcher{all: true}, nil
	}
	if useRegex {
		re, err := regexp.Compile(text)
		if err != nil {
			return nil, err
		}
		return &Matcher{re: re}, nil
	}
	return &Matcher{plain: strings.ToLower(text)}, nil
}

// MatchNothing returns a matcher that rejects every row. The filter bar uses
// it while a regex is malformed, so the list shows nothing and the error is
// unambiguous (as in VS Code), rather than silently falling back to no
// filter.
func MatchNothing() *Matcher {
	return &Matcher{re: regexp.MustCompile(`\b\B`)} // a position can't be both; never matches
}

// Match reports whether subject passes the filter.
func (m *Matcher) Match(subject string) bool {
	switch {
	case m == nil || m.all:
		return true
	case m.re != nil:
		return m.re.MatchString(subject)
	default:
		return strings.Contains(strings.ToLower(subject), m.plain)
	}
}

// MatchesAll reports whether the matcher passes every row, i.e. the query is
// empty. The UI uses this to tell "no filter" from "a filter that happens to
// match nothing".
func (m *Matcher) MatchesAll() bool {
	return m == nil || m.all
}
