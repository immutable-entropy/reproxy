package intercept

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"reproxy/internal/bodyview"
)

// The editor shows messages with \n line endings and can't hold arbitrary
// bytes, so editing goes through a text form:
//
//   - EditableText renders the raw message exactly as it is on the wire;
//   - EditableView renders it with the body made readable (chunked framing
//     and content coding undone, JSON and XML laid out);
//   - ApplyEdit turns text in either form back into wire bytes.
//
// Header lines are always sent with CRLF. The body is sent exactly as it was
// unless the user changed it: text that still reads as it was rendered gives
// the original bytes back, byte for byte, whatever was undone to show it. A
// body that was changed is put back into the framing the headers being sent
// describe, so an edited gzip body is re-compressed, and dropping its
// Content-Encoding header sends it as it now reads. A binary body is shown as
// a placeholder line and is kept as long as that line is left alone.

const binaryPlaceholderFmt = "‹%d bytes of binary data — not editable as text; leave this line to keep it›"

// EditableText renders raw for editing, as it stands on the wire.
func EditableText(raw []byte) string {
	head, body, _ := splitMessage(raw)
	return headText(head) + "\n\n" + bodyText(body)
}

// EditableView renders raw for editing with the body made readable: see
// bodyview. note says in a few words what was done to it, for the editor to
// show beside it, and is empty when the body reads as it arrived.
func EditableView(raw []byte) (text, note string) {
	head, body, _ := splitMessage(raw)
	r := bodyview.Render(body, bodyview.ParseHeaders(head), false)
	return headText(head) + "\n\n" + renderedText(r), r.Note()
}

// ApplyEdit converts text produced by editing EditableText(original) or
// EditableView(original) back into a raw message. If updateContentLength is
// set, an existing Content-Length header is corrected to the new body size
// (unless the body is chunked).
func ApplyEdit(original []byte, text string, updateContentLength bool) []byte {
	if text == EditableText(original) {
		return original
	}
	origHead, origBody, _ := splitMessage(original)

	text = strings.ReplaceAll(text, "\r\n", "\n")
	headText, bodyEdited, _ := strings.Cut(text, "\n\n")
	lines := strings.Split(strings.TrimRight(headText, "\n"), "\n")

	body := editedBody(origHead, origBody, lines, bodyEdited)

	if updateContentLength && !isChunked(lines) {
		for idx, l := range lines[1:] {
			name, _, ok := strings.Cut(l, ":")
			if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				lines[idx+1] = name + ": " + strconv.Itoa(len(body))
			}
		}
	}

	var out bytes.Buffer
	out.WriteString(strings.Join(lines, "\r\n"))
	out.WriteString("\r\n\r\n")
	out.Write(body)
	return out.Bytes()
}

// editedBody works out the body bytes to send from the edited body text.
func editedBody(origHead, origBody []byte, lines []string, edited string) []byte {
	was := bodyview.ParseHeaders(origHead)
	sending := bodyview.ParseHeaderLines(lines[1:]) // lines[0] is the start line
	r := bodyview.Render(origBody, was, false)

	// The body counts as untouched if it still reads as either form rendered
	// it, or if its placeholder line is still there.
	untouched := edited == renderedText(r) || edited == bodyText(origBody)
	switch {
	case untouched && r.Binary:
		// A placeholder stands in for bytes that can't be edited as text;
		// there is nothing to put back together.
		return origBody
	case untouched && bodyview.SameFraming(was, sending):
		return origBody
	}

	wire := edited
	if bytes.Contains(r.Decoded, []byte("\r\n")) {
		// Formats like multipart need CRLF; keep the original convention.
		wire = strings.ReplaceAll(edited, "\n", "\r\n")
	}
	if b, ok := bodyview.Restore(wire, sending); ok {
		return b
	}
	return []byte(wire)
}

// renderedText is the body text a rendered body is shown as.
func renderedText(r bodyview.Rendered) string {
	if r.Binary {
		return fmt.Sprintf(binaryPlaceholderFmt, len(r.Decoded))
	}
	return r.Text
}

func headText(head []byte) string {
	return strings.ReplaceAll(string(head), "\r\n", "\n")
}

func bodyText(body []byte) string {
	if bodyview.IsText(body) {
		return strings.ReplaceAll(string(body), "\r\n", "\n")
	}
	return fmt.Sprintf(binaryPlaceholderFmt, len(body))
}

func splitMessage(raw []byte) (head, body []byte, ok bool) {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i], raw[i+4:], true
	}
	return bytes.TrimRight(raw, "\r\n"), nil, false
}

func isChunked(lines []string) bool {
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Transfer-Encoding") &&
			strings.Contains(strings.ToLower(value), "chunked") {
			return true
		}
	}
	return false
}

// RequestMethod is the method of a request's start line ("POST /x
// HTTP/1.1" → "POST"), or "" if the line has none.
func RequestMethod(startLine string) string {
	method, _, _ := strings.Cut(strings.TrimLeft(startLine, " "), " ")
	return method
}

// WithMethod is a request's start line with its method replaced, the
// request-target and version kept as they were. A method that isn't one
// token (empty, or holding a space) leaves the line as it is.
func WithMethod(startLine, method string) string {
	if method == "" || strings.ContainsAny(method, " \t\r\n") {
		return startLine
	}
	_, rest, ok := strings.Cut(strings.TrimLeft(startLine, " "), " ")
	if !ok {
		return method
	}
	return method + " " + rest
}
