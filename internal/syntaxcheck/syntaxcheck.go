// Package syntaxcheck validates message bodies against the language chosen in
// the body editor (JSON, XML, HTML, JavaScript, CSS) and reports problems with
// their positions, so the editor can underline them as the user types.
//
// The checkers are tolerant: they recover after an error and keep going, so
// one mistake doesn't hide the next. JSON and XML are checked strictly; HTML,
// JavaScript and CSS get a structural check (unclosed brackets, tags, strings
// and comments, and similar), not a full parse.
//
// It has no UI dependency so the checks can be tested directly and, later,
// reused by plugins.
package syntaxcheck

import (
	"sort"
	"unicode/utf8"
)

// Issue is one problem found in a body.
type Issue struct {
	// Offset and Len give the byte range the problem covers in the checked
	// text. Len is 0 only for a problem at the very end of the text.
	Offset, Len int
	// Line and Col are 1-based; Col counts characters, not bytes.
	Line, Col int
	Message   string
}

// maxIssues caps how many problems are reported: past a handful the rest are
// usually knock-on effects, and the editor has to draw every one.
const maxIssues = 50

// reporter collects issues for one check.
type reporter struct {
	src    string
	issues []Issue
}

func (r *reporter) add(off, length int, msg string) {
	if off > len(r.src) {
		off = len(r.src)
	}
	if off+length > len(r.src) {
		length = len(r.src) - off
	}
	r.issues = append(r.issues, Issue{Offset: off, Len: length, Message: msg})
}

func (r *reporter) full() bool { return len(r.issues) >= maxIssues }

// result sorts the issues by position, drops repeats at the same offset, caps
// the count and fills in line and column.
func (r *reporter) result() []Issue {
	if len(r.issues) == 0 {
		return nil
	}
	sort.SliceStable(r.issues, func(i, j int) bool { return r.issues[i].Offset < r.issues[j].Offset })
	out := r.issues[:0]
	for _, is := range r.issues {
		if len(out) > 0 && out[len(out)-1].Offset == is.Offset {
			continue
		}
		out = append(out, is)
	}
	if len(out) > maxIssues {
		out = out[:maxIssues]
	}
	line, col, pos := 1, 1, 0
	for i := range out {
		for pos < out[i].Offset {
			c, size := utf8.DecodeRuneInString(r.src[pos:])
			if c == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
			pos += size
		}
		out[i].Line, out[i].Col = line, col
	}
	return out
}

// runeLen is the byte length of the character at s[i], for highlighting a
// single unexpected character.
func runeLen(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	_, n := utf8.DecodeRuneInString(s[i:])
	return n
}

// quoteChar renders the character at s[i] for a message.
func quoteChar(s string, i int) string {
	return "'" + s[i:i+runeLen(s, i)] + "'"
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// bracket is an open bracket waiting for its closer.
type bracket struct {
	ch  byte
	off int
}

func closerFor(open byte) byte {
	switch open {
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	}
	return 0
}

// closeBracket handles closer c at off against stack, shared by the
// JavaScript and CSS checks: a match pops; a closer that matches further down
// reports the brackets it skips as unclosed; anything else is unexpected.
func closeBracket(r *reporter, stack []bracket, c byte, off int) []bracket {
	for i := len(stack) - 1; i >= 0; i-- {
		if closerFor(stack[i].ch) != c {
			continue
		}
		for j := len(stack) - 1; j > i; j-- {
			r.add(stack[j].off, 1, "Unclosed '"+string(stack[j].ch)+"'")
		}
		return stack[:i]
	}
	r.add(off, 1, "Unexpected '"+string(c)+"'")
	return stack
}

// reportUnclosed reports every bracket still open at the end of the text.
func reportUnclosed(r *reporter, stack []bracket) {
	for _, b := range stack {
		r.add(b.off, 1, "Unclosed '"+string(b.ch)+"'")
	}
}
