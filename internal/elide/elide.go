// Package elide shortens text without ever splitting a rune.
//
// Everything reproxy shows or hands on is arbitrary captured traffic, so a
// non-ASCII body is the normal case: cutting a string at a byte offset
// splits a multi-byte rune whenever the offset misses a boundary, and the
// result is invalid UTF-8 that renders as U+FFFD — and, once it reaches a
// session file or an agent over MCP, is stored and read that way.
//
// Two budgets, because the callers want different things. Runes and Tail
// count runes, for text going on screen, where what matters is how wide it
// is. Bytes and Prefix count bytes, for text going to a model or a log,
// where what matters is how much of it there is.
package elide

import (
	"strings"
	"unicode/utf8"
)

// Ellipsis is the character appended to text that has been cut.
const Ellipsis = "…"

// Runes shortens s to at most n runes, ending with Ellipsis when it cuts.
// The ellipsis counts toward n, so the result is never wider than n.
// Trailing space before the cut is dropped, so "foo  …" comes back "foo…".
func Runes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " \t\n\r") + Ellipsis
}

// Tail shortens s to at most n runes, keeping the end and starting with
// Ellipsis when it cuts. It is for right-aligned text, where the end of a
// path or URL is the part that identifies it.
func Tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return Ellipsis + string(r[len(r)-n+1:])
}

// Bytes shortens s to at most n bytes, appending marker when it cuts. The
// marker is not counted against n: it is a note about the cut itself
// ("\n<truncated>", "… (12345 bytes total)") rather than a bare ellipsis,
// so n stays the budget for the content.
//
// When one rune is longer than the whole budget, s is returned whole
// rather than empty: a budget too small to hold anything is a caller's
// mistake, and dropping the text would hide it.
func Bytes(s string, n int, marker string) string {
	if len(s) <= n {
		return s
	}
	cut := Prefix(s, n)
	if cut == "" {
		return s
	}
	return cut + marker
}

// Prefix returns the longest prefix of s that is at most n bytes and ends
// on a rune boundary, so it is always valid UTF-8. It is "" when n is
// shorter than s's first rune.
func Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
