// Package bodyview makes an HTTP message body readable, and puts an edited
// one back the way it was framed.
//
// A body on the wire is rarely readable as it stands: it may be cut into
// chunks, compressed with gzip, deflate, brotli or zstd, and written without
// a line break from one end to the other. Decode undoes the transfer and
// content codings; Render also pretty-prints JSON and XML. Restore reverses
// both, so a held message can be edited in the readable form and still go out
// correctly framed.
//
// Which coding to undo, and which to re-apply, comes from the message's own
// headers (see Headers): Restore follows the headers it is given, so removing
// Content-Encoding while editing a held message really does send the body
// uncompressed.
//
// Nothing here runs on the proxy's hot path: a body is decoded when someone
// looks at it or edits it, never while it is relayed.
package bodyview

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Limits on the work done for one body.
const (
	// maxDecoded caps what one body decompresses to. A small compressed body
	// can expand without bound, and nothing a person or an agent reads needs
	// more than this.
	maxDecoded = 16 << 20
	// maxFormat is the largest body that is pretty-printed; past it the cost
	// outweighs the readability.
	maxFormat = 4 << 20
)

// Headers are the header fields that decide how a body is framed and what it
// holds. ParseHeaders reads them from a raw message head.
type Headers struct {
	ContentType      string
	ContentEncoding  string
	TransferEncoding string
}

// Rendered is a body made readable.
type Rendered struct {
	// Text is the readable body, with \n line endings. It is empty when
	// Binary is set.
	Text string
	// Decoded is the body with its transfer and content codings undone: the
	// bytes Text was made from, before any pretty-printing.
	Decoded []byte
	// Coding names the content coding that was undone ("gzip", "br", ...),
	// or "" when there was none to undo.
	Coding string
	// Chunked reports that chunked transfer framing was undone.
	Chunked bool
	// Format names the pretty-printing that was applied ("JSON", "XML"), or
	// "" when the text is as it was decoded.
	Format string
	// Binary reports that the body is not readable as text even decoded, so
	// the caller should show a placeholder instead of Text.
	Binary bool
	// Partial reports that decoding stopped early: the capture was cut short
	// or the body expanded past maxDecoded.
	Partial bool
}

// Note describes in a few words what was done to make the body readable, for
// a view to show next to it. It is empty when the body is shown as it
// arrived.
func (r Rendered) Note() string {
	var parts []string
	if r.Chunked {
		parts = append(parts, "dechunked")
	}
	if r.Coding != "" {
		parts = append(parts, "decoded from "+codingNames(r.Coding))
	}
	if r.Format != "" {
		parts = append(parts, "formatted as "+r.Format)
	}
	if r.Partial {
		parts = append(parts, "incomplete")
	}
	return strings.Join(parts, " · ")
}

// Render decodes a body and pretty-prints it when it is JSON or XML.
// truncated says whether the capture is known to be cut short, which allows a
// multi-byte character chopped in half at the end.
func Render(body []byte, h Headers, truncated bool) Rendered {
	r := Decode(body, h, truncated)
	if r.Binary || len(r.Text) == 0 || len(r.Text) > maxFormat {
		return r
	}
	if text, kind, ok := format(h.ContentType, r.Text); ok {
		r.Text, r.Format = text, kind
	}
	return r
}

// Decode undoes a body's chunked framing and content coding, without
// reformatting what comes out.
func Decode(body []byte, h Headers, truncated bool) Rendered {
	r := Rendered{Decoded: body}
	if len(body) == 0 {
		return r
	}

	if isChunked(h.TransferEncoding) {
		if data, ok := dechunk(body); ok {
			r.Decoded, r.Chunked = data.body, true
			r.Partial = r.Partial || data.partial
		}
	}

	// Codings are listed in the order they were applied, so they come off
	// back to front. A coding we can't undo is left alone: the bytes may be
	// readable anyway (servers do mislabel bodies), and if they aren't, the
	// body is reported as binary below.
	list := codings(h.ContentEncoding)
	for i := len(list) - 1; i >= 0; i-- {
		data, partial, ok := decompress(list[i], r.Decoded)
		if !ok {
			break
		}
		r.Decoded = data
		r.Coding = strings.Join(list[i:], ", ")
		r.Partial = r.Partial || partial
	}

	data := r.Decoded
	if !IsText(data) {
		trimmed, ok := trimCutRune(data, truncated || r.Partial)
		if !ok {
			r.Binary = true
			return r
		}
		data = trimmed
	}
	r.Text = strings.ReplaceAll(string(data), "\r\n", "\n")
	return r
}

// Restore turns readable text back into body bytes, applying the content
// coding and the chunked framing h describes. It reports false when a coding
// in h can't be applied, leaving the caller to send the text as it is.
func Restore(text string, h Headers) ([]byte, bool) {
	b := []byte(text)
	for _, c := range codings(h.ContentEncoding) {
		out, ok := compress(c, b)
		if !ok {
			return nil, false
		}
		b = out
	}
	if isChunked(h.TransferEncoding) {
		b = chunk(b)
	}
	return b, true
}

// SameFraming reports whether two sets of headers frame a body the same way.
// When they don't, even an untouched body has to be re-encoded, so that what
// goes out matches the headers going out with it.
func SameFraming(a, b Headers) bool {
	return slices.Equal(codings(a.ContentEncoding), codings(b.ContentEncoding)) &&
		isChunked(a.TransferEncoding) == isChunked(b.TransferEncoding)
}

// IsText reports whether b can be shown, and edited, as text: valid UTF-8
// with no NUL or other C0 control character besides tab, newline and
// carriage return.
func IsText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return true
}

// ParseHeaders reads the fields bodyview needs from a raw message head (the
// start line and its header lines, with or without the blank line that ends
// them).
func ParseHeaders(head []byte) Headers {
	var h Headers
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if len(lines) > 0 {
		lines = lines[1:] // the start line
	}
	return h.addLines(lines)
}

// ParseHeaderLines reads the fields bodyview needs from header lines that are
// already split up, as an editor holds them (no start line).
func ParseHeaderLines(lines []string) Headers {
	var h Headers
	return h.addLines(lines)
}

func (h Headers) addLines(lines []string) Headers {
	for _, l := range lines {
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		// A field may be repeated; the values join as if sent on one line.
		switch {
		case strings.EqualFold(strings.TrimSpace(name), "Content-Type"):
			h.ContentType = appendValue(h.ContentType, value)
		case strings.EqualFold(strings.TrimSpace(name), "Content-Encoding"):
			h.ContentEncoding = appendValue(h.ContentEncoding, value)
		case strings.EqualFold(strings.TrimSpace(name), "Transfer-Encoding"):
			h.TransferEncoding = appendValue(h.TransferEncoding, value)
		}
	}
	return h
}

func appendValue(have, add string) string {
	if have == "" {
		return add
	}
	return have + ", " + add
}

// codings splits a Content-Encoding value into the codings it names, in the
// order they were applied. "identity" carries no coding, so it is left out.
func codings(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(value, ",") {
		c := strings.ToLower(strings.TrimSpace(part))
		if c == "" || c == "identity" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// isChunked reports whether a Transfer-Encoding value ends in "chunked", the
// only framing that is applied last.
func isChunked(value string) bool {
	parts := strings.Split(value, ",")
	return strings.EqualFold(strings.TrimSpace(parts[len(parts)-1]), "chunked")
}

// codingNames spells a coding list the way a person reads it.
func codingNames(list string) string {
	var out []string
	for _, c := range codings(list) {
		switch c {
		case "br":
			out = append(out, "brotli")
		case "x-gzip":
			out = append(out, "gzip")
		default:
			out = append(out, c)
		}
	}
	return strings.Join(out, ", ")
}

// dechunked is the result of undoing chunked framing.
type dechunked struct {
	body    []byte
	partial bool
}

// dechunk joins the chunks of a chunked body. It reports false when the body
// isn't chunked after all, so the caller can leave it alone. Trailers are not
// part of the body and are left out; the raw view still shows them.
func dechunk(body []byte) (dechunked, bool) {
	var out []byte
	rest, started := body, false
	for {
		line, tail, found := cutLine(rest)
		if !found {
			// The capture stops inside the framing.
			return dechunked{out, true}, started
		}
		field, _, _ := strings.Cut(strings.TrimSpace(string(line)), ";")
		size, err := strconv.ParseInt(field, 16, 64)
		if err != nil || size < 0 {
			return dechunked{out, true}, started
		}
		started = true
		if size == 0 {
			return dechunked{out, false}, true
		}
		if int64(len(tail)) < size {
			return dechunked{append(out, tail...), true}, true // cut short
		}
		out = append(out, tail[:size]...)
		rest = tail[size:]
		// The CRLF that ends the chunk data.
		line, tail, found = cutLine(rest)
		if !found || len(bytes.TrimSpace(line)) != 0 {
			return dechunked{out, true}, true
		}
		rest = tail
	}
}

// cutLine splits off the first line, without its terminator.
func cutLine(b []byte) (line, rest []byte, found bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return nil, b, false
	}
	return bytes.TrimSuffix(b[:i], []byte("\r")), b[i+1:], true
}

// chunk frames b as a single chunk. Any framing is as good as any other to
// the receiver, and one chunk is the simplest to read in the raw view.
func chunk(b []byte) []byte {
	if len(b) == 0 {
		return []byte("0\r\n\r\n")
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "%x\r\n", len(b))
	out.Write(b)
	out.WriteString("\r\n0\r\n\r\n")
	return out.Bytes()
}

// decompress undoes one content coding. partial reports that the stream ended
// early, which a truncated capture does; what came out before that is kept.
func decompress(coding string, b []byte) (out []byte, partial, ok bool) {
	src := bytes.NewReader(b)
	var r io.Reader
	switch coding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, false, false
		}
		r = zr
	case "deflate":
		// HTTP's "deflate" is zlib-wrapped deflate, but servers do send it
		// raw, so fall back to that.
		if zr, err := zlib.NewReader(src); err == nil {
			r = zr
		} else {
			src.Seek(0, io.SeekStart)
			r = flate.NewReader(src)
		}
	case "br":
		r = brotli.NewReader(src)
	case "zstd":
		zr, err := zstd.NewReader(src)
		if err != nil {
			return nil, false, false
		}
		defer zr.Close()
		r = zr
	default:
		return nil, false, false
	}

	out, err := io.ReadAll(io.LimitReader(r, maxDecoded+1))
	switch {
	case len(out) > maxDecoded:
		return out[:maxDecoded], true, true
	case err != nil:
		return out, true, len(out) > 0
	}
	return out, false, true
}

// compress applies one content coding.
func compress(coding string, b []byte) ([]byte, bool) {
	var buf bytes.Buffer
	var w io.WriteCloser
	switch coding {
	case "gzip", "x-gzip":
		w = gzip.NewWriter(&buf)
	case "deflate":
		w = zlib.NewWriter(&buf) // the coding as HTTP defines it
	case "br":
		w = brotli.NewWriter(&buf)
	case "zstd":
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			return nil, false
		}
		w = zw
	default:
		return nil, false
	}
	if _, err := w.Write(b); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// trimCutRune drops up to three bytes from the end to recover text whose last
// character was chopped in half, which happens when a capture is cut short
// mid-character. It only does so when allowed, so a body that merely looks
// like text isn't trimmed into one.
func trimCutRune(b []byte, allowed bool) ([]byte, bool) {
	if !allowed {
		return nil, false
	}
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if IsText(b) {
			return b, true
		}
		b = b[:len(b)-1]
	}
	return nil, false
}
