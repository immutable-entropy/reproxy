// Package editassist provides the body editor's typing assists for JSON, XML
// and HTML: closing brackets and quotes as they are opened, stepping over a
// closer that is already there, closing tags, keeping indentation on Enter,
// and tag-name completions.
//
// Each assist takes the text and a cursor position (byte offsets) and returns
// the Edit to make instead of plain typing, or false to let the key through.
//
// It has no UI dependency so the assists can be tested directly and, later,
// reused by plugins.
package editassist

import (
	"sort"
	"strings"
)

// Lang is a language the assists understand.
type Lang int

const (
	JSON Lang = iota + 1
	XML
	HTML
)

// Edit replaces text[Start:End] with Text and puts the cursor at Cursor, a
// byte offset into the edited text between Start and Start+len(Text).
type Edit struct {
	Start, End int
	Text       string
	Cursor     int
}

func insert(pos int, text string, cursor int) Edit {
	return Edit{Start: pos, End: pos, Text: text, Cursor: pos + cursor}
}

// stepOver moves past the character at pos, which is the one being typed.
func stepOver(text string, pos int) Edit {
	return Edit{Start: pos, End: pos + 1, Text: text[pos : pos+1], Cursor: pos + 1}
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

// Type handles typing ch (one character) at pos.
func Type(lang Lang, text string, pos int, ch string) (Edit, bool) {
	if len(ch) != 1 || pos < 0 || pos > len(text) {
		return Edit{}, false
	}
	if lang == JSON {
		return typeJSON(text, pos, ch[0])
	}
	return typeMarkup(lang, text, pos, ch[0])
}

// canPair reports whether an opener typed before next should get its closer
// too: not when it would glue onto a following word.
func canPair(next byte) bool {
	return next == 0 || next == ' ' || next == '\t' || next == '\n' || next == '\r' ||
		strings.IndexByte("}]),:", next) >= 0
}

func typeJSON(text string, pos int, ch byte) (Edit, bool) {
	next := byteAt(text, pos)
	inString := inJSONString(text, pos)
	switch ch {
	case '{', '[':
		if !inString && canPair(next) {
			return insert(pos, string(ch)+closer(ch), 1), true
		}
	case '}', ']':
		if !inString && next == ch {
			return stepOver(text, pos), true
		}
	case '"':
		switch {
		case inString && next == '"':
			return stepOver(text, pos), true
		case !inString && canPair(next):
			return insert(pos, `""`, 1), true
		}
	}
	return Edit{}, false
}

func closer(open byte) string {
	switch open {
	case '{':
		return "}"
	case '[':
		return "]"
	}
	return string(open)
}

// inJSONString reports whether pos is inside a string. JSON strings can't
// span lines, so counting quotes from the start of the line is enough.
func inJSONString(text string, pos int) bool {
	in := false
	for i := lineStart(text, pos); i < pos; i++ {
		switch text[i] {
		case '\\':
			if in {
				i++
			}
		case '"':
			in = !in
		}
	}
	return in
}

func lineStart(text string, pos int) int {
	return strings.LastIndexByte(text[:pos], '\n') + 1
}

func typeMarkup(lang Lang, text string, pos int, ch byte) (Edit, bool) {
	next := byteAt(text, pos)
	tag, inTag := openTagText(text, pos)
	switch ch {
	case '>':
		if next == '>' && inTag {
			return stepOver(text, pos), true
		}
		name, ok := startTagName(lang, tag)
		if !ok || strings.HasPrefix(text[pos:], "</"+name+">") {
			break
		}
		return insert(pos, "></"+name+">", 1), true
	case '/':
		if byteAt(text, pos-1) != '<' {
			break
		}
		open := OpenElements(lang, text[:pos-1])
		if len(open) == 0 {
			break
		}
		name := open[len(open)-1]
		end := "</" + name + ">"
		if strings.HasPrefix(text[pos:], end) {
			// Its end tag was already inserted: drop the '<' just typed and
			// step over it.
			return Edit{Start: pos - 1, End: pos + len(end), Text: end, Cursor: pos - 1 + len(end)}, true
		}
		if next == '>' {
			return Edit{Start: pos, End: pos + 1, Text: "/" + name + ">", Cursor: pos + len(name) + 2}, true
		}
		return insert(pos, "/"+name+">", len(name)+2), true
	case '"', '\'':
		if !inTag {
			break
		}
		if next == ch && strings.Count(tag, string(ch))%2 == 1 {
			return stepOver(text, pos), true
		}
		if byteAt(text, pos-1) == '=' {
			return insert(pos, string(ch)+string(ch), 1), true
		}
	}
	return Edit{}, false
}

// openTagText returns the text of the tag the cursor is inside ("<a href=x"),
// if it is inside one.
func openTagText(text string, pos int) (string, bool) {
	lt := strings.LastIndexByte(text[:pos], '<')
	if lt < 0 || strings.IndexByte(text[lt:pos], '>') >= 0 {
		return "", false
	}
	return text[lt:pos], true
}

// startTagName returns the element name if tag ("<name attr…", without its
// '>') is a start tag that needs an end tag.
func startTagName(lang Lang, tag string) (string, bool) {
	if len(tag) < 2 || strings.HasSuffix(tag, "/") {
		return "", false
	}
	name, n := tagName(tag[1:])
	if name == "" {
		return "", false
	}
	// Unbalanced quotes mean the '>' is inside an attribute value.
	rest := tag[1+n:]
	if strings.Count(rest, `"`)%2 == 1 || strings.Count(rest, "'")%2 == 1 {
		return "", false
	}
	if lang == HTML && htmlVoid[strings.ToLower(name)] {
		return "", false
	}
	return name, true
}

// tagName reads a tag name at the start of s and returns it with its length.
func tagName(s string) (string, int) {
	n := 0
	for n < len(s) {
		c := s[n]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' ||
			n > 0 && (c >= '0' && c <= '9' || c == '-' || c == ':' || c == '.') {
			n++
			continue
		}
		break
	}
	return s[:n], n
}

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

var (
	htmlVoid    = set("area base br col embed hr img input link meta param source track wbr")
	htmlRawText = set("script style textarea title")
)

// OpenElements returns the elements still open at the end of text, outermost
// first.
func OpenElements(lang Lang, text string) []string {
	var stack []string
	i := 0
	for {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			return stack
		}
		i += lt
		rest := text[i:]
		skipTo := func(end string) bool {
			n := strings.Index(rest, end)
			if n < 0 {
				return false
			}
			i += n + len(end)
			return true
		}
		switch {
		case strings.HasPrefix(rest, "<!--"):
			if !skipTo("-->") {
				return stack
			}
		case strings.HasPrefix(rest, "<![CDATA["):
			if !skipTo("]]>") {
				return stack
			}
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			if !skipTo(">") {
				return stack
			}
		case strings.HasPrefix(rest, "</"):
			name, _ := tagName(rest[2:])
			if !skipTo(">") {
				return stack
			}
			for j := len(stack) - 1; j >= 0; j-- {
				if sameName(lang, stack[j], name) {
					stack = stack[:j]
					break
				}
			}
		default:
			end := tagEnd(rest)
			if end < 0 {
				return stack // still typing this tag
			}
			tag := rest[:end]
			i += end + 1
			name, ok := startTagName(lang, tag)
			if !ok {
				continue
			}
			stack = append(stack, name)
			if lang == HTML && htmlRawText[strings.ToLower(name)] {
				n := strings.Index(strings.ToLower(text[i:]), "</"+strings.ToLower(name))
				if n < 0 {
					return stack
				}
				i += n
			}
		}
	}
}

func sameName(lang Lang, a, b string) bool {
	if lang == HTML {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// tagEnd finds the '>' that ends the tag starting at s[0], skipping quoted
// attribute values, or -1.
func tagEnd(s string) int {
	var quote byte
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// Newline handles Enter at pos: the new line keeps the current line's
// indentation, one level deeper after an opener ('{', '[' or a start tag).
// Between an opener and its closer the closer moves to a line of its own.
func Newline(lang Lang, text string, pos int) (Edit, bool) {
	if pos < 0 || pos > len(text) {
		return Edit{}, false
	}
	ls := lineStart(text, pos)
	line := text[ls:pos]
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	before := strings.TrimRight(line, " \t")
	unit := indentUnit(text)

	opens, between := false, false
	if lang == JSON {
		if n := len(before); n > 0 && (before[n-1] == '{' || before[n-1] == '[') {
			opens = true
			between = byteAt(text, pos) == closer(before[n-1])[0]
		}
	} else if strings.HasSuffix(before, ">") {
		lt := strings.LastIndexByte(before, '<')
		if lt >= 0 {
			if name, ok := startTagName(lang, before[lt:len(before)-1]); ok {
				opens = true
				between = strings.HasPrefix(text[pos:], "</"+name)
			}
		}
	}
	switch {
	case between:
		inner := "\n" + indent + unit
		return insert(pos, inner+"\n"+indent, len(inner)), true
	case opens:
		indent += unit
	}
	return insert(pos, "\n"+indent, 1+len(indent)), true
}

// indentUnit guesses the text's indentation step from its first indented
// line, defaulting to two spaces.
func indentUnit(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		if line[0] == '\t' {
			return "\t"
		}
		if n := len(line) - len(trimmed); n == 4 {
			return "    "
		}
		return "  "
	}
	return "  "
}

// Backspace handles Backspace at pos: deleting an opener whose closer
// directly follows (as just auto-inserted) deletes both.
func Backspace(lang Lang, text string, pos int) (Edit, bool) {
	if pos < 1 || pos >= len(text) {
		return Edit{}, false
	}
	pair := text[pos-1 : pos+1]
	pairs := []string{`""`}
	if lang == JSON {
		pairs = append(pairs, "{}", "[]")
	} else {
		pairs = append(pairs, "''")
	}
	for _, p := range pairs {
		if pair == p {
			return Edit{Start: pos - 1, End: pos + 1, Cursor: pos - 1}, true
		}
	}
	return Edit{}, false
}

// TagPrefix reports whether the cursor is on a start-tag name being typed
// ("<di|") and returns the part typed so far.
func TagPrefix(text string, pos int) (string, bool) {
	start := pos
	for start > 0 {
		c := text[start-1]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == ':' || c == '_' || c == '.' {
			start--
			continue
		}
		break
	}
	if start == 0 || text[start-1] != '<' {
		return "", false
	}
	return text[start:pos], true
}

// TagNames returns the element names to offer when completing a start tag:
// those already used in the text and, for HTML, the standard elements.
func TagNames(lang Lang, text string) []string {
	seen := map[string]bool{}
	if lang == HTML {
		for _, n := range strings.Fields(htmlElements) {
			seen[n] = true
		}
	}
	for i := 0; ; {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			break
		}
		i += lt + 1
		name, n := tagName(text[i:])
		i += n
		if name != "" {
			if lang == HTML {
				name = strings.ToLower(name)
			}
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

const htmlElements = `a abbr address area article aside audio b base bdi bdo blockquote body br
button canvas caption cite code col colgroup data datalist dd del details dfn dialog div dl dt
em embed fieldset figcaption figure footer form h1 h2 h3 h4 h5 h6 head header hgroup hr html i
iframe img input ins kbd label legend li link main map mark menu meta meter nav noscript object
ol optgroup option output p param picture pre progress q rp rt ruby s samp script search section
select slot small source span strong style sub summary sup svg table tbody td template textarea
tfoot th thead time title tr track u ul var video wbr`
