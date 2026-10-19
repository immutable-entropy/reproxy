package syntaxcheck

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// openTag is an element waiting for its end tag.
type openTag struct {
	name     string
	off, end int // the start tag's range
}

// XML checks s as a well-formed XML document: syntax, matching end tags, a
// single root element. An empty body is valid.
func XML(s string) []Issue {
	r := &reporter{src: s}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = true
	var stack []openTag
	roots := 0
	for !r.full() {
		start := int(d.InputOffset())
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			msg := err.Error()
			var se *xml.SyntaxError
			if errors.As(err, &se) {
				msg = se.Msg
			}
			off := int(d.InputOffset())
			if off > 0 && off >= len(s) {
				off = len(s) - 1
			}
			r.add(off, runeLen(s, off), capitalise(msg))
			return r.result() // the decoder can't continue past a syntax error
		}
		end := int(d.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				roots++
				if roots == 2 {
					r.add(start, end-start, "Only one root element is allowed")
				}
			}
			stack = append(stack, openTag{qname(t.Name), start, end})
		case xml.EndElement:
			stack = closeTag(r, stack, qname(t.Name), start, end, nil)
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(t)) != "" {
				r.add(start, end-start, "Text outside the root element")
			}
		}
	}
	reportOpenTags(r, stack, nil)
	return r.result()
}

func qname(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// closeTag handles end tag name at [off,end): it closes the nearest open
// element of that name, reporting any it skips over as unclosed (unless
// optional says their end tag may be omitted). An end tag with no open
// element is reported.
func closeTag(r *reporter, stack []openTag, name string, off, end int, optional map[string]bool) []openTag {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].name != name {
			continue
		}
		reportOpenTags(r, stack[i+1:], optional)
		return stack[:i]
	}
	r.add(off, end-off, "Unexpected </"+name+">: no matching open tag")
	return stack
}

func reportOpenTags(r *reporter, open []openTag, optional map[string]bool) {
	for _, o := range open {
		if !optional[o.name] {
			r.add(o.off, o.end-o.off, "<"+o.name+"> is never closed")
		}
	}
}

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

var (
	htmlVoid = set("area base br col embed hr img input link meta param source track wbr")
	// Elements whose end tag HTML lets you leave out.
	htmlOptionalEnd = set("html head body p li dt dd option optgroup rb rt rtc rp tr td th thead tbody tfoot colgroup caption")
	// Elements whose content is raw text, not markup.
	htmlRawText = set("script style textarea title")
)

// HTML checks s for structural problems: unclosed or mismatched tags,
// unterminated tags, attribute values and comments. It follows HTML's rules
// for void elements and optional end tags, so valid HTML isn't flagged.
func HTML(s string) []Issue {
	r := &reporter{src: s}
	var stack []openTag
	i := 0
	for i < len(s) && !r.full() {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := s[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				r.add(i, 4, "Unterminated comment")
				return r.result()
			}
			i += 4 + end + 3
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				r.add(i, 2, "Unterminated tag")
				return r.result()
			}
			i += end + 1
		case strings.HasPrefix(rest, "</"):
			name, n := tagName(rest[2:])
			if name == "" {
				i += 2
				continue
			}
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				r.add(i, 2+n, "Unterminated tag </"+name)
				return r.result()
			}
			if !htmlVoid[name] {
				stack = closeTag(r, stack, name, i, i+end+1, htmlOptionalEnd)
			}
			i += end + 1
		default:
			name, n := tagName(rest[1:])
			if name == "" {
				i++ // a lone '<' in text
				continue
			}
			end, selfClose, ok := scanTag(r, s, i, 1+n, name)
			if !ok {
				return r.result()
			}
			if selfClose || htmlVoid[name] {
				i = end
				continue
			}
			stack = append(stack, openTag{name, i, end})
			i = end
			if htmlRawText[name] {
				// Skip the content up to the end tag, which the loop then
				// handles normally.
				close := strings.Index(strings.ToLower(s[i:]), "</"+name)
				if close < 0 {
					i = len(s) // reported below as never closed
					break
				}
				i += close
			}
		}
	}
	reportOpenTags(r, stack, htmlOptionalEnd)
	return r.result()
}

// tagName reads a tag name at the start of s, lower-cased, and its length.
func tagName(s string) (string, int) {
	n := 0
	for n < len(s) {
		c := s[n]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || n > 0 && (c >= '0' && c <= '9' || c == '-' || c == ':' || c == '_' || c == '.') {
			n++
			continue
		}
		break
	}
	return strings.ToLower(s[:n]), n
}

// scanTag reads the attributes of the start tag beginning at s[start] up to
// its '>', returning the offset just after it and whether it was
// self-closing ("/>"). ok is false when the text ends inside the tag.
func scanTag(r *reporter, s string, start, nameLen int, name string) (end int, selfClose, ok bool) {
	i := start + nameLen
	for i < len(s) {
		switch c := s[i]; c {
		case '"', '\'':
			close := strings.IndexByte(s[i+1:], c)
			if close < 0 {
				r.add(i, 1, "Unterminated attribute value")
				return 0, false, false
			}
			i += close + 2
		case '>':
			return i + 1, i > start && s[i-1] == '/', true
		case '<':
			// A new tag starts before this one ended: report and resume there.
			r.add(start, nameLen, "<"+name+"> is missing its closing '>'")
			return i, false, true
		default:
			i++
		}
	}
	r.add(start, nameLen, "Unterminated tag <"+name)
	return 0, false, false
}
