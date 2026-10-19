package bodyview

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// indent is one level of nesting in formatted JSON and XML.
const indent = "  "

// inlineText is the longest text content kept on the same line as the element
// that holds it, e.g. <title>Home</title>. Past it the text gets its own
// line, so a long value doesn't push the closing tag out of sight.
const inlineText = 120

// format pretty-prints a body that is JSON or XML, reporting the kind it
// recognised. It reports false when the body is neither, or isn't valid
// enough to reformat without guessing — the text is then shown as it
// arrived, which is what a reader of broken markup wants to see anyway.
func format(contentType, text string) (out, kind string, ok bool) {
	switch formatKind(contentType, text) {
	case "JSON":
		if out, ok := formatJSON(text); ok {
			return out, "JSON", true
		}
	case "XML":
		if out, ok := formatXML(text); ok {
			return out, "XML", true
		}
	}
	return "", "", false
}

// formatKind picks the formatting a body gets from its content type, falling
// back to the shape of the body when there is no usable type.
//
// HTML is deliberately not reformatted: its whitespace is significant inside
// elements like <pre> and <textarea>, and re-indenting a document that is
// being examined for an injection flaw would change the very thing being
// looked at.
func formatKind(contentType, text string) string {
	mt := mediaType(contentType)
	switch {
	case strings.Contains(mt, "html"): // including application/xhtml+xml
		return ""
	case strings.Contains(mt, "json"):
		return "JSON"
	case strings.Contains(mt, "xml"):
		return "XML"
	case mt != "":
		return ""
	}
	// No content type to go by: the body's first character is a good guess,
	// and formatting only goes ahead if it then parses.
	switch t := strings.TrimSpace(text); {
	case strings.HasPrefix(t, "{"), strings.HasPrefix(t, "["):
		return "JSON"
	case strings.HasPrefix(t, "<?xml"):
		return "XML"
	}
	return ""
}

// mediaType is a Content-Type reduced to its lower-case type/subtype.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// formatJSON indents JSON, one element per line. Strings and numbers are
// copied as they were written: encoding/json's indenter only moves
// whitespace, so nothing is re-escaped, rounded or reordered.
func formatJSON(text string) (string, bool) {
	src := []byte(text)
	if !json.Valid(src) {
		return "", false
	}
	var out bytes.Buffer
	if err := json.Indent(&out, src, "", indent); err != nil {
		return "", false
	}
	s := out.String()
	if s == text {
		return "", false // already laid out this way
	}
	return s, true
}

// formatXML re-indents an XML document, one element per line. Every token is
// copied from the source verbatim, so attributes, entity references,
// namespace prefixes, comments and CDATA come out exactly as they went in and
// only the whitespace between tags changes.
//
// It reports false unless the whole document parses and its tags balance:
// re-indenting markup that isn't well formed would move text to lines it
// doesn't belong on.
func formatXML(text string) (string, bool) {
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = true

	var out strings.Builder
	// open holds the names of the elements being nested through, so end tags
	// can be checked against them: RawToken keeps every token's text intact
	// but doesn't check that the tags match.
	var open []string
	// pending collects the character data since the last tag, so an element
	// that holds nothing but a short string keeps it on one line.
	pending := ""
	afterStart := false

	line := func(s string) {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(strings.Repeat(indent, len(open)))
		out.WriteString(s)
	}
	flush := func() {
		// Text that spans lines keeps the layout it came with: it may be
		// CDATA, or whitespace that counts.
		if t := strings.TrimSpace(pending); t != "" {
			line(t)
		}
		pending = ""
	}

	for {
		start := d.InputOffset()
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false
		}
		raw := text[start:d.InputOffset()]

		switch t := tok.(type) {
		case xml.CharData:
			pending += raw
			continue
		case xml.StartElement:
			flush()
			line(raw)
			open = append(open, name(t.Name))
			afterStart = true
		case xml.EndElement:
			if len(open) == 0 || open[len(open)-1] != name(t.Name) {
				return "", false // an end tag that doesn't close the element it is in
			}
			if raw == "" {
				// A self-closing element: RawToken reports the end of
				// <name/> without any text of its own.
				open = open[:len(open)-1]
				pending, afterStart = "", false
				continue
			}
			if txt := strings.TrimSpace(pending); afterStart && txt != "" &&
				len(txt) <= inlineText && !strings.ContainsAny(txt, "\n\r") {
				// Short text with no element beside it stays on the start
				// tag's line, e.g. <title>Home</title>.
				out.WriteString(txt)
				pending = ""
				open = open[:len(open)-1]
				out.WriteString(raw)
			} else {
				flush() // the element's own content, at its own depth
				open = open[:len(open)-1]
				line(raw)
			}
			afterStart = false
		default: // comment, processing instruction, doctype
			flush()
			line(raw)
			afterStart = false
		}
	}
	if len(open) != 0 {
		return "", false // a start tag with no end tag
	}
	flush()

	s := out.String()
	if s == text {
		return "", false // already laid out this way
	}
	return s, true
}

// name is an element's name as it was written, prefix and all.
func name(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}
