package capture

import (
	"bytes"
	"fmt"
	"strings"

	"reproxy/internal/bodyview"
)

// DisplayText renders a raw message for a text view. Headers are shown
// verbatim; the body has its chunked framing and content coding undone (see
// bodyview) so that a compressed page reads as the page it is, with a note
// saying so. A body that still isn't readable text (images, encrypted or
// unknown formats) is replaced by a one-line placeholder so the view stays
// usable. Line endings are normalised to \n.
//
// The body is not reformatted: this is the text an agent reads over MCP, and
// laying out JSON would spend its budget on whitespace. The editor's view
// does reformat; see intercept.EditableView.
func DisplayText(m Message) string {
	if len(m.Raw) == 0 {
		return ""
	}

	head, body, hasBody := splitHead(m.Raw)

	var b strings.Builder
	b.WriteString(toValidUTF8(head))
	if hasBody {
		b.WriteString("\r\n\r\n")
		if len(body) > 0 {
			r := bodyview.Decode(body, bodyview.ParseHeaders(head), m.Truncated)
			if r.Binary {
				fmt.Fprintf(&b, "‹%s of binary data not shown›", FormatBytes(int64(len(r.Decoded))))
			} else {
				b.WriteString(r.Text)
				if note := r.Note(); note != "" {
					fmt.Fprintf(&b, "\r\n\r\n‹body %s; the headers describe it as it was on the wire›", note)
				}
			}
		}
	}
	if m.Truncated {
		fmt.Fprintf(&b, "\r\n\r\n‹truncated: showing %s of %s›", FormatBytes(int64(len(m.Raw))), FormatBytes(m.Size))
	}
	return strings.ReplaceAll(b.String(), "\r\n", "\n")
}

// DecodedBody returns a message's body with its chunked framing and content
// coding undone (see bodyview), and whether that body is readable (non-empty
// and not binary). It is the bytes behind DisplayText's body section, for a
// caller that wants the body alone — a JSON source map, say — rather than the
// rendered message.
func DecodedBody(m Message) (body []byte, ok bool) {
	if len(m.Raw) == 0 {
		return nil, false
	}
	head, raw, hasBody := splitHead(m.Raw)
	if !hasBody || len(raw) == 0 {
		return nil, false
	}
	r := bodyview.Decode(raw, bodyview.ParseHeaders(head), m.Truncated)
	if r.Binary || len(r.Decoded) == 0 {
		return nil, false
	}
	return r.Decoded, true
}

// splitHead splits a message at the blank line that ends its headers.
func splitHead(raw []byte) (head, body []byte, hasBody bool) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i], raw[i+4:], true
	}
	return raw, nil, false
}

func toValidUTF8(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

// FormatBytes renders a byte count for humans, e.g. "1.4 KB".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
