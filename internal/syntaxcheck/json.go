package syntaxcheck

import (
	"regexp"
	"strings"
)

type jsonKind int

const (
	jEOF jsonKind = iota
	jString
	jNumber
	jLiteral // true, false, null
	jWord    // any other bare word
	jLBrace
	jRBrace
	jLBrack
	jRBrack
	jColon
	jComma
	jInvalid
)

type jsonToken struct {
	kind     jsonKind
	off, end int
	// broken marks a token the lexer already reported (unterminated string),
	// so the parser doesn't pile a second complaint on the next token.
	broken bool
}

// What a container expects next.
type jsonState int

const (
	expKeyOrClose   jsonState = iota // just after '{'
	expKey                           // after ',' in an object
	expColon                         // after a key
	expValue                         // after ':', or after ',' in an array
	expValueOrClose                  // just after '['
	expCommaOrClose                  // after a member or element
)

type jsonFrame struct {
	open  jsonToken
	obj   bool
	state jsonState
	comma int // offset of the last ',', for trailing-comma reports
}

type jsonChecker struct {
	reporter
	pos   int
	stack []jsonFrame
	done  bool // the top-level value is complete
	prev  jsonToken
}

var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// JSON checks s as a JSON document. An empty or all-whitespace body is valid
// (it means "no body").
func JSON(s string) []Issue {
	c := &jsonChecker{reporter: reporter{src: s}}
	seen := false
	for !c.full() {
		t := c.next()
		if t.kind == jEOF {
			break
		}
		seen = true
		if !c.step(t) {
			break
		}
		c.prev = t
	}
	if !c.full() {
		c.finish(seen)
	}
	return c.result()
}

// finish reports what is still open at the end of the text.
func (c *jsonChecker) finish(seen bool) {
	end := len(c.src)
	if len(c.stack) > 0 {
		top := c.stack[len(c.stack)-1]
		switch top.state {
		case expColon:
			c.add(end, 0, "Expected ':' after the key")
		case expValue:
			c.add(end, 0, "Expected a value")
		}
	}
	for _, f := range c.stack {
		c.add(f.open.off, 1, "Unclosed '"+c.src[f.open.off:f.open.end]+"'")
	}
	if seen && !c.done && len(c.stack) == 0 && len(c.issues) == 0 {
		c.add(end, 0, "Expected a JSON value")
	}
}

// step feeds one token to the parser. It returns false to stop checking.
func (c *jsonChecker) step(t jsonToken) bool {
	if len(c.stack) == 0 {
		if c.done {
			c.add(t.off, t.end-t.off, "Unexpected content after the JSON value")
			return false
		}
		if startsValue(t.kind) {
			c.value(t)
		} else {
			c.unexpected(t)
		}
		return true
	}
	f := &c.stack[len(c.stack)-1]
	switch f.state {
	case expKeyOrClose, expKey:
		switch t.kind {
		case jString:
			f.state = expColon
		case jWord, jNumber, jLiteral:
			c.add(t.off, t.end-t.off, "Object keys must be double-quoted strings")
			f.state = expColon
		case jRBrace:
			if f.state == expKey {
				c.add(f.comma, 1, "Trailing comma")
			}
			c.pop()
		case jColon:
			c.add(t.off, 1, "Missing key before ':'")
			f.state = expValue
		case jComma:
			c.add(t.off, 1, "Expected a key")
		default:
			c.closerOrUnexpected(t)
		}
	case expColon:
		switch {
		case t.kind == jColon:
			f.state = expValue
		case startsValue(t.kind):
			c.add(t.off, t.end-t.off, "Expected ':' after the key")
			f.state = expValue
			return c.step(t)
		case t.kind == jComma:
			c.add(t.off, 1, "Expected ':' and a value after the key")
			f.state, f.comma = expKey, t.off
		case t.kind == jRBrace:
			c.add(t.off, 1, "Expected ':' and a value after the key")
			c.pop()
		default:
			c.closerOrUnexpected(t)
		}
	case expValue, expValueOrClose:
		switch {
		case startsValue(t.kind):
			c.value(t)
		case f.obj && t.kind == jRBrace:
			c.add(t.off, 1, "Expected a value")
			c.pop()
		case !f.obj && t.kind == jRBrack:
			if f.state == expValue {
				c.add(f.comma, 1, "Trailing comma")
			}
			c.pop()
		case t.kind == jComma:
			c.add(t.off, 1, "Expected a value")
			if f.obj {
				f.state = expKey
			}
			f.comma = t.off
		default:
			c.closerOrUnexpected(t)
		}
	case expCommaOrClose:
		switch {
		case t.kind == jComma:
			f.comma = t.off
			if f.obj {
				f.state = expKey
			} else {
				f.state = expValue
			}
		case f.obj && t.kind == jRBrace, !f.obj && t.kind == jRBrack:
			c.pop()
		case startsValue(t.kind):
			if !c.prev.broken {
				c.add(t.off, t.end-t.off, "Missing ',' before this")
			}
			if f.obj {
				f.state = expKey
			} else {
				f.state = expValue
			}
			return c.step(t)
		default:
			c.closerOrUnexpected(t)
		}
	}
	return true
}

func startsValue(k jsonKind) bool {
	switch k {
	case jString, jNumber, jLiteral, jWord, jLBrace, jLBrack:
		return true
	}
	return false
}

// value consumes a token that starts a value in the current position.
func (c *jsonChecker) value(t jsonToken) {
	if t.kind == jWord {
		c.add(t.off, t.end-t.off, "Unexpected '"+c.src[t.off:t.end]+"': strings must be double-quoted")
	}
	c.completed()
	switch t.kind {
	case jLBrace:
		c.stack = append(c.stack, jsonFrame{open: t, obj: true, state: expKeyOrClose})
	case jLBrack:
		c.stack = append(c.stack, jsonFrame{open: t, state: expValueOrClose})
	}
}

// completed records that a value has been read where the current container
// (or the top level) expected one.
func (c *jsonChecker) completed() {
	if len(c.stack) == 0 {
		c.done = true
		return
	}
	c.stack[len(c.stack)-1].state = expCommaOrClose
}

func (c *jsonChecker) pop() {
	c.stack = c.stack[:len(c.stack)-1]
}

// closerOrUnexpected handles a token that doesn't fit: a closing bracket that
// matches an outer container closes it (the inner ones are reported unclosed),
// anything else is reported and skipped.
func (c *jsonChecker) closerOrUnexpected(t jsonToken) {
	if t.kind == jRBrace || t.kind == jRBrack {
		obj := t.kind == jRBrace
		for i := len(c.stack) - 1; i >= 0; i-- {
			if c.stack[i].obj != obj {
				continue
			}
			for j := len(c.stack) - 1; j > i; j-- {
				o := c.stack[j].open
				c.add(o.off, 1, "Unclosed '"+c.src[o.off:o.end]+"'")
			}
			c.stack = c.stack[:i]
			return
		}
	}
	c.unexpected(t)
}

func (c *jsonChecker) unexpected(t jsonToken) {
	if t.kind == jInvalid {
		return // the lexer already reported it
	}
	c.add(t.off, t.end-t.off, "Unexpected "+quoteChar(c.src, t.off))
}

// next lexes the next token, reporting lexical problems as it goes.
func (c *jsonChecker) next() jsonToken {
	s := c.src
	for c.pos < len(s) {
		ch := s[c.pos]
		if isSpace(ch) {
			c.pos++
			continue
		}
		if ch == '/' && c.pos+1 < len(s) && (s[c.pos+1] == '/' || s[c.pos+1] == '*') {
			c.comment()
			continue
		}
		break
	}
	start := c.pos
	if start >= len(s) {
		return jsonToken{kind: jEOF, off: start, end: start}
	}
	tok := func(k jsonKind, end int) jsonToken {
		c.pos = end
		return jsonToken{kind: k, off: start, end: end}
	}
	switch ch := s[start]; {
	case ch == '{':
		return tok(jLBrace, start+1)
	case ch == '}':
		return tok(jRBrace, start+1)
	case ch == '[':
		return tok(jLBrack, start+1)
	case ch == ']':
		return tok(jRBrack, start+1)
	case ch == ':':
		return tok(jColon, start+1)
	case ch == ',':
		return tok(jComma, start+1)
	case ch == '"':
		return c.str()
	case ch == '\'':
		end := start + 1
		for end < len(s) && s[end] != '\'' && s[end] != '\n' {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		if end < len(s) && s[end] == '\'' {
			end++
		}
		c.add(start, end-start, "Strings must use double quotes")
		return tok(jString, end)
	case ch == '-' || ch >= '0' && ch <= '9':
		end := start + 1
		for end < len(s) && (isIdentPart(s[end]) || s[end] == '.' || s[end] == '+' || s[end] == '-') {
			end++
		}
		if !jsonNumber.MatchString(s[start:end]) {
			c.add(start, end-start, "Invalid number '"+s[start:end]+"'")
		}
		return tok(jNumber, end)
	case isIdentStart(ch):
		end := start + 1
		for end < len(s) && isIdentPart(s[end]) {
			end++
		}
		switch s[start:end] {
		case "true", "false", "null":
			return tok(jLiteral, end)
		}
		return tok(jWord, end)
	}
	n := runeLen(s, start)
	c.add(start, n, "Unexpected "+quoteChar(s, start))
	return tok(jInvalid, start+n)
}

// str lexes a double-quoted string starting at c.pos.
func (c *jsonChecker) str() jsonToken {
	s := c.src
	start := c.pos
	i := start + 1
	for i < len(s) {
		switch ch := s[i]; {
		case ch == '"':
			c.pos = i + 1
			return jsonToken{kind: jString, off: start, end: i + 1}
		case ch == '\\':
			if i+1 >= len(s) {
				i++
				continue
			}
			switch s[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 <= len(s) && isHex4(s[i+2:i+6]) {
					i += 6
				} else {
					c.add(i, 2, "Invalid \\u escape: needs four hex digits")
					i += 2
				}
			case '\n':
				i++ // the newline ends the string below
			default:
				n := 1 + runeLen(s, i+1)
				c.add(i, n, "Invalid escape '"+s[i:i+n]+"'")
				i += n
			}
		case ch == '\n':
			c.add(start, i-start, "Unterminated string")
			c.pos = i
			return jsonToken{kind: jString, off: start, end: i, broken: true}
		case ch < 0x20:
			c.add(i, 1, "Control characters must be escaped in strings")
			i++
		default:
			i++
		}
	}
	c.add(start, len(s)-start, "Unterminated string")
	c.pos = len(s)
	return jsonToken{kind: jString, off: start, end: len(s), broken: true}
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
			return false
		}
	}
	return true
}

// comment skips a // or /* */ comment, which JSON doesn't allow.
func (c *jsonChecker) comment() {
	s, start := c.src, c.pos
	end := len(s)
	if s[start+1] == '/' {
		if i := strings.IndexByte(s[start:], '\n'); i >= 0 {
			end = start + i
		}
	} else if i := strings.Index(s[start+2:], "*/"); i >= 0 {
		end = start + 2 + i + 2
	}
	c.add(start, end-start, "Comments are not allowed in JSON")
	c.pos = end
}
