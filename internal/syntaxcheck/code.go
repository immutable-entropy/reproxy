package syntaxcheck

import "strings"

// JavaScript checks s for structural problems: unbalanced or mismatched
// brackets and unterminated strings, template literals, regular expressions
// and comments. It is a lexical check, not a full parse.
func JavaScript(s string) []Issue {
	r := &reporter{src: s}
	// '$' entries mark a template literal's ${…} substitution: its '}'
	// resumes the template rather than closing a brace.
	var stack []bracket
	regexOK := true // whether a '/' here would start a regular expression
	i := 0
	for i < len(s) && !r.full() {
		c := s[i]
		switch {
		case isSpace(c):
			i++
			continue
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
				i += n
			} else {
				i = len(s)
			}
			continue
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			n := strings.Index(s[i+2:], "*/")
			if n < 0 {
				r.add(i, 2, "Unterminated comment")
				return r.result()
			}
			i += 2 + n + 2
			continue
		case c == '/' && regexOK:
			i = scanRegex(r, s, i)
			regexOK = false
			continue
		case c == '"' || c == '\'':
			i = scanQuoted(r, s, i)
			regexOK = false
			continue
		case c == '`':
			var sub bool
			if i, sub = scanTemplate(r, s, i, i+1); sub {
				stack = append(stack, bracket{'$', i - 2})
				regexOK = true
			} else {
				regexOK = false
			}
			continue
		case c == '(' || c == '[' || c == '{':
			stack = append(stack, bracket{c, i})
			regexOK = true
		case c == '}' && len(stack) > 0 && stack[len(stack)-1].ch == '$':
			// End of a ${…} substitution: carry on with the template.
			stack = stack[:len(stack)-1]
			var sub bool
			if i, sub = scanTemplate(r, s, i, i+1); sub {
				stack = append(stack, bracket{'$', i - 2})
				regexOK = true
			} else {
				regexOK = false
			}
			continue
		case c == ')' || c == ']' || c == '}':
			stack = closeBracket(r, stack, c, i)
			regexOK = c == '}' // a block can end before a regex statement
		case isIdentPart(c):
			start := i
			for i < len(s) && (isIdentPart(s[i]) || s[i] == '.' && c >= '0' && c <= '9') {
				i++
			}
			regexOK = jsRegexAfter[s[start:i]]
			continue
		default:
			regexOK = true
		}
		i++
	}
	for _, b := range stack {
		if b.ch == '$' {
			r.add(b.off, 2, "Unclosed '${' in template literal")
		} else {
			r.add(b.off, 1, "Unclosed '"+string(b.ch)+"'")
		}
	}
	return r.result()
}

// jsRegexAfter lists the keywords after which a '/' starts a regular
// expression rather than being a division.
var jsRegexAfter = set("return typeof instanceof in of new delete void throw case do else yield await")

// scanQuoted skips a quoted string starting at s[start] and returns the offset
// after it. An unescaped newline ends it as unterminated.
func scanQuoted(r *reporter, s string, start int) int {
	q := s[start]
	i := start + 1
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case q:
			return i + 1
		case '\n':
			r.add(start, i-start, "Unterminated string")
			return i
		}
		i++
	}
	r.add(start, len(s)-start, "Unterminated string")
	return len(s)
}

// scanTemplate scans template-literal text from i (the literal opened at
// open) to its closing backtick, or to a "${" substitution, in which case sub
// is true and the returned offset is just after the "${".
func scanTemplate(r *reporter, s string, open, i int) (next int, sub bool) {
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '`':
			return i + 1, false
		case '$':
			if i+1 < len(s) && s[i+1] == '{' {
				return i + 2, true
			}
		}
		i++
	}
	r.add(open, 1, "Unterminated template literal")
	return len(s), false
}

// scanRegex skips a regular-expression literal starting at s[start], with its
// flags.
func scanRegex(r *reporter, s string, start int) int {
	i := start + 1
	inClass := false
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				i++
				for i < len(s) && isIdentPart(s[i]) {
					i++
				}
				return i
			}
		case '\n':
			r.add(start, i-start, "Unterminated regular expression")
			return i
		}
		i++
	}
	r.add(start, len(s)-start, "Unterminated regular expression")
	return len(s)
}

// CSS checks s for structural problems: unbalanced braces and brackets,
// unterminated strings and comments, and declarations missing their ':'.
func CSS(s string) []Issue {
	r := &reporter{src: s}
	var stack []bracket
	// The current statement inside a block: where it starts, whether it has
	// any content, whether it is an at-rule, and whether it has a ':'.
	segStart, segContent, segAt, segColon := 0, false, false, false
	reset := func(at int) { segStart, segContent, segAt, segColon = at, false, false, false }
	blockDepth := func() int {
		n := 0
		for _, b := range stack {
			if b.ch == '{' {
				n++
			}
		}
		return n
	}
	// endDecl checks the statement ending at end (a ';' or '}').
	endDecl := func(end int) {
		text := strings.TrimSpace(s[segStart:end])
		if text != "" && !segAt && !segColon && blockDepth() > 0 {
			off := segStart + strings.Index(s[segStart:end], text)
			r.add(off, len(text), "Expected ':' in declaration")
		}
	}
	i := 0
	for i < len(s) && !r.full() {
		c := s[i]
		switch {
		case isSpace(c):
			i++
			continue
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			n := strings.Index(s[i+2:], "*/")
			if n < 0 {
				r.add(i, 2, "Unterminated comment")
				return r.result()
			}
			i += 2 + n + 2
			continue
		}
		if !segContent {
			segStart, segContent, segAt = i, true, c == '@'
		}
		switch c {
		case '"', '\'':
			i = scanQuoted(r, s, i)
			continue
		case '{':
			stack = append(stack, bracket{c, i})
			reset(i + 1)
		case '}':
			endDecl(i)
			stack = closeBracket(r, stack, c, i)
			reset(i + 1)
		case ';':
			endDecl(i)
			reset(i + 1)
		case ':':
			segColon = true
		case '(', '[':
			stack = append(stack, bracket{c, i})
		case ')', ']':
			stack = closeBracket(r, stack, c, i)
		}
		i++
	}
	reportUnclosed(r, stack)
	return r.result()
}
