package bodyview

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const html = "<!doctype html>\n<html><body>hello</body></html>"

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func br(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zst(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlb(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeContentCodings(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		encoding string
		coding   string
	}{
		{"gzip", gz(t, html), "gzip", "gzip"},
		{"gzip cased and spaced", gz(t, html), " GZIP ", "gzip"},
		{"x-gzip", gz(t, html), "x-gzip", "x-gzip"},
		{"brotli", br(t, html), "br", "br"},
		{"zstd", zst(t, html), "zstd", "zstd"},
		{"deflate as zlib", zlb(t, html), "deflate", "deflate"},
		{"identity", []byte(html), "identity", ""},
		{"none", []byte(html), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Decode(c.body, Headers{ContentType: "text/html", ContentEncoding: c.encoding}, false)
			if r.Binary {
				t.Fatalf("body reported as binary")
			}
			if r.Text != html {
				t.Errorf("text = %q, want %q", r.Text, html)
			}
			if r.Coding != c.coding {
				t.Errorf("coding = %q, want %q", r.Coding, c.coding)
			}
			if r.Partial {
				t.Errorf("partial set for a complete body")
			}
		})
	}
}

// A brotli-encoded HTML page is what a site behind a CDN serves, and the
// reason a body that Chrome shows as HTML used to arrive here as binary.
func TestDecodeNoteNamesBrotli(t *testing.T) {
	r := Decode(br(t, html), Headers{ContentType: "text/html", ContentEncoding: "br"}, false)
	if got := r.Note(); got != "decoded from brotli" {
		t.Errorf("note = %q", got)
	}
}

func TestDecodeStackedCodings(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(br(t, html))
	w.Close()

	r := Decode(buf.Bytes(), Headers{ContentEncoding: "br, gzip"}, false)
	if r.Text != html {
		t.Fatalf("text = %q, want %q", r.Text, html)
	}
	if r.Coding != "br, gzip" {
		t.Errorf("coding = %q, want %q", r.Coding, "br, gzip")
	}
}

func TestDecodeChunked(t *testing.T) {
	body := "f\r\n<!doctype html>\r\n20\r\n\n<html><body>hello</body></html>\r\n0\r\n\r\n"
	r := Decode([]byte(body), Headers{ContentType: "text/html", TransferEncoding: "chunked"}, false)
	if !r.Chunked {
		t.Fatal("chunked framing not undone")
	}
	if r.Text != html {
		t.Errorf("text = %q, want %q", r.Text, html)
	}
	if r.Partial {
		t.Error("partial set for a complete body")
	}
}

func TestDecodeChunkedAndEncoded(t *testing.T) {
	enc := gz(t, html)
	var body bytes.Buffer
	body.WriteString("4\r\n")
	body.Write(enc[:4])
	body.WriteString("\r\n")
	fmt.Fprintf(&body, "%x\r\n%s\r\n", len(enc[4:]), enc[4:])
	body.WriteString("0\r\n\r\n")

	r := Decode(body.Bytes(), Headers{ContentEncoding: "gzip", TransferEncoding: "chunked"}, false)
	if r.Text != html {
		t.Fatalf("text = %q, want %q", r.Text, html)
	}
	if got := r.Note(); got != "dechunked · decoded from gzip" {
		t.Errorf("note = %q", got)
	}
}

// Trailers belong to the message, not the body, so they are left out of the
// readable body. The raw view still shows them.
func TestDecodeChunkedWithTrailers(t *testing.T) {
	body := "5\r\nhello\r\n0\r\nExpires: soon\r\n\r\n"
	r := Decode([]byte(body), Headers{TransferEncoding: "chunked"}, false)
	if r.Text != "hello" {
		t.Errorf("text = %q, want %q", r.Text, "hello")
	}
}

// A capture cut short in the middle of the framing still shows what arrived.
func TestDecodeChunkedTruncated(t *testing.T) {
	body := "5\r\nhello\r\n5\r\nwor"
	r := Decode([]byte(body), Headers{TransferEncoding: "chunked"}, true)
	if r.Text != "hellowor" {
		t.Errorf("text = %q, want %q", r.Text, "hellowor")
	}
	if !r.Partial {
		t.Error("partial not reported for a body cut short")
	}
}

// A body whose Transfer-Encoding says chunked but which isn't is left alone,
// rather than being thrown away.
func TestDecodeNotActuallyChunked(t *testing.T) {
	r := Decode([]byte("hello"), Headers{TransferEncoding: "chunked"}, false)
	if r.Chunked {
		t.Error("claimed to undo framing that wasn't there")
	}
	if r.Text != "hello" {
		t.Errorf("text = %q", r.Text)
	}
}

// Servers do label bodies with a coding they didn't apply. The bytes are
// shown as they are rather than reported as unreadable.
func TestDecodeMislabelledCoding(t *testing.T) {
	r := Decode([]byte(html), Headers{ContentEncoding: "gzip"}, false)
	if r.Binary {
		t.Fatal("readable text reported as binary")
	}
	if r.Coding != "" {
		t.Errorf("coding = %q, want none undone", r.Coding)
	}
	if r.Text != html {
		t.Errorf("text = %q", r.Text)
	}
}

func TestDecodeTruncatedGzipKeepsWhatArrived(t *testing.T) {
	full := gz(t, strings.Repeat("hello world. ", 500))
	r := Decode(full[:len(full)-20], Headers{ContentEncoding: "gzip"}, true)
	if r.Binary {
		t.Fatal("reported as binary")
	}
	if !strings.HasPrefix(r.Text, "hello world. hello world.") {
		t.Errorf("text = %.40q", r.Text)
	}
	if !r.Partial {
		t.Error("partial not reported")
	}
}

func TestDecodeBinaryStaysBinary(t *testing.T) {
	r := Decode([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, Headers{ContentType: "image/png"}, false)
	if !r.Binary {
		t.Fatal("binary body not reported as binary")
	}
	if r.Text != "" {
		t.Errorf("text = %q, want empty", r.Text)
	}
}

func TestDecodeEmpty(t *testing.T) {
	r := Decode(nil, Headers{ContentEncoding: "gzip", TransferEncoding: "chunked"}, false)
	if r.Binary || r.Text != "" || r.Note() != "" {
		t.Errorf("empty body rendered as %+v", r)
	}
}

func TestDecodeNormalisesLineEndings(t *testing.T) {
	r := Decode([]byte("a\r\nb\r\n"), Headers{ContentType: "text/plain"}, false)
	if r.Text != "a\nb\n" {
		t.Errorf("text = %q", r.Text)
	}
}

func TestDecodeTolerantOfCharacterCutInHalf(t *testing.T) {
	body := []byte("h€llo") // the euro sign takes three bytes
	r := Decode(body[:3], Headers{}, true)
	if r.Binary {
		t.Fatal("reported as binary")
	}
	if r.Text != "h" {
		t.Errorf("text = %q, want %q", r.Text, "h")
	}
}

func TestDecodeUntruncatedInvalidUTF8IsBinary(t *testing.T) {
	r := Decode([]byte("hello\xff"), Headers{}, false)
	if !r.Binary {
		t.Error("invalid UTF-8 shown as text")
	}
}

func TestParseHeaders(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"content-encoding: br\r\n" +
		"Transfer-Encoding: chunked\r\n"
	h := ParseHeaders([]byte(head))
	if h.ContentType != "text/html; charset=utf-8" {
		t.Errorf("content type = %q", h.ContentType)
	}
	if h.ContentEncoding != "br" {
		t.Errorf("content encoding = %q", h.ContentEncoding)
	}
	if h.TransferEncoding != "chunked" {
		t.Errorf("transfer encoding = %q", h.TransferEncoding)
	}
}

// The start line is not a header, even when it contains a colon.
func TestParseHeadersSkipsStartLine(t *testing.T) {
	h := ParseHeaders([]byte("GET http://x/content-type:1 HTTP/1.1\r\nContent-Type: text/plain\r\n"))
	if h.ContentType != "text/plain" {
		t.Errorf("content type = %q", h.ContentType)
	}
}

func TestParseHeadersRepeatedField(t *testing.T) {
	h := ParseHeaders([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: br\r\nContent-Encoding: gzip\r\n"))
	if h.ContentEncoding != "br, gzip" {
		t.Errorf("content encoding = %q", h.ContentEncoding)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, h := range []Headers{
		{},
		{ContentEncoding: "gzip"},
		{ContentEncoding: "br"},
		{ContentEncoding: "zstd"},
		{ContentEncoding: "deflate"},
		{TransferEncoding: "chunked"},
		{ContentEncoding: "br", TransferEncoding: "chunked"},
		{ContentEncoding: "gzip, br"},
	} {
		t.Run(h.ContentEncoding+"/"+h.TransferEncoding, func(t *testing.T) {
			wire, ok := Restore(html, h)
			if !ok {
				t.Fatal("Restore reported failure")
			}
			r := Decode(wire, h, false)
			if r.Text != html {
				t.Errorf("round trip gave %q, want %q", r.Text, html)
			}
		})
	}
}

func TestRestoreRejectsUnknownCoding(t *testing.T) {
	if _, ok := Restore("hello", Headers{ContentEncoding: "exi"}); ok {
		t.Error("Restore accepted a coding it cannot apply")
	}
}

func TestRestoreEmptyChunkedBody(t *testing.T) {
	b, ok := Restore("", Headers{TransferEncoding: "chunked"})
	if !ok || string(b) != "0\r\n\r\n" {
		t.Errorf("Restore = %q, %v", b, ok)
	}
}

func TestSameFraming(t *testing.T) {
	cases := []struct {
		a, b Headers
		want bool
	}{
		{Headers{}, Headers{}, true},
		{Headers{ContentEncoding: "gzip"}, Headers{ContentEncoding: "GZIP"}, true},
		{Headers{ContentEncoding: "gzip"}, Headers{}, false},
		{Headers{ContentEncoding: "identity"}, Headers{}, true},
		{Headers{TransferEncoding: "chunked"}, Headers{}, false},
		{Headers{ContentEncoding: "gzip, br"}, Headers{ContentEncoding: "br, gzip"}, false},
	}
	for _, c := range cases {
		if got := SameFraming(c.a, c.b); got != c.want {
			t.Errorf("SameFraming(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A decompression bomb is cut off rather than filling memory.
func TestDecodeCapsExpansion(t *testing.T) {
	r := Decode(gz(t, strings.Repeat("a", maxDecoded+1024)), Headers{ContentEncoding: "gzip"}, false)
	if len(r.Text) != maxDecoded {
		t.Errorf("decoded %d bytes, want the %d-byte cap", len(r.Text), maxDecoded)
	}
	if !r.Partial {
		t.Error("partial not reported after hitting the cap")
	}
}
