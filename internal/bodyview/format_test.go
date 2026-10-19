package bodyview

import (
	"strings"
	"testing"
)

func TestRenderFormatsJSON(t *testing.T) {
	body := `{"id":7,"tags":["a","b"],"user":{"name":"kay","admin":false},"empty":{},"none":[]}`
	want := `{
  "id": 7,
  "tags": [
    "a",
    "b"
  ],
  "user": {
    "name": "kay",
    "admin": false
  },
  "empty": {},
  "none": []
}`
	r := Render([]byte(body), Headers{ContentType: "application/json; charset=utf-8"}, false)
	if r.Format != "JSON" {
		t.Fatalf("format = %q", r.Format)
	}
	if r.Text != want {
		t.Errorf("text =\n%s\nwant\n%s", r.Text, want)
	}
	if got := r.Note(); got != "formatted as JSON" {
		t.Errorf("note = %q", got)
	}
}

// Whatever whitespace the body arrived with, the layout is the same.
func TestRenderReindentsBadlyLaidOutJSON(t *testing.T) {
	r := Render([]byte("{\n\t\t\"a\":\n1,   \"b\":[\n\n2]}"), Headers{ContentType: "application/json"}, false)
	if r.Text != "{\n  \"a\": 1,\n  \"b\": [\n    2\n  ]\n}" {
		t.Errorf("text = %q", r.Text)
	}
}

// Numbers and strings are copied as they were written: a security tool must
// not round 64-bit ids or re-escape what a server sent.
func TestRenderJSONKeepsValuesVerbatim(t *testing.T) {
	body := `{"big":12345678901234567890123,"exp":1.0E+2,"esc":"aA\/b","uni":"héllo"}`
	r := Render([]byte(body), Headers{ContentType: "application/json"}, false)
	for _, want := range []string{`12345678901234567890123`, `1.0E+2`, `"aA\/b"`, `"héllo"`} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("%s missing from\n%s", want, r.Text)
		}
	}
}

// Key order is how a request was written, which may be what matters about it.
func TestRenderJSONKeepsKeyOrder(t *testing.T) {
	r := Render([]byte(`{"z":1,"a":2,"m":3}`), Headers{ContentType: "application/json"}, false)
	if want := "{\n  \"z\": 1,\n  \"a\": 2,\n  \"m\": 3\n}"; r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
}

func TestRenderBrokenJSONLeftAlone(t *testing.T) {
	body := `{"a":1,}`
	r := Render([]byte(body), Headers{ContentType: "application/json"}, false)
	if r.Format != "" {
		t.Errorf("format = %q, want none", r.Format)
	}
	if r.Text != body {
		t.Errorf("text = %q, want it unchanged", r.Text)
	}
}

func TestRenderJSONVariants(t *testing.T) {
	for _, ct := range []string{"application/json", "APPLICATION/JSON", "text/json",
		"application/ld+json", "application/vnd.api+json", "application/problem+json"} {
		r := Render([]byte(`{"a":[1]}`), Headers{ContentType: ct}, false)
		if r.Format != "JSON" {
			t.Errorf("%s: format = %q", ct, r.Format)
		}
	}
}

// With no Content-Type to go by, the body's own shape decides.
func TestRenderSniffsJSONWithoutContentType(t *testing.T) {
	r := Render([]byte(`[{"a":1}]`), Headers{}, false)
	if r.Format != "JSON" {
		t.Errorf("format = %q", r.Format)
	}
}

func TestRenderLeavesOtherTypesAlone(t *testing.T) {
	for _, ct := range []string{"text/plain", "application/javascript", "text/css",
		"application/x-www-form-urlencoded"} {
		body := `{"a":1}`
		r := Render([]byte(body), Headers{ContentType: ct}, false)
		if r.Format != "" || r.Text != body {
			t.Errorf("%s: format = %q, text = %q", ct, r.Format, r.Text)
		}
	}
}

func TestRenderFormatsXML(t *testing.T) {
	body := `<?xml version="1.0"?><!-- a note --><feed xmlns:a="urn:x"><a:entry id="1"><title>Home</title><empty/></a:entry><raw><![CDATA[<b>not markup</b>]]></raw></feed>`
	want := `<?xml version="1.0"?>
<!-- a note -->
<feed xmlns:a="urn:x">
  <a:entry id="1">
    <title>Home</title>
    <empty/>
  </a:entry>
  <raw><![CDATA[<b>not markup</b>]]></raw>
</feed>`
	r := Render([]byte(body), Headers{ContentType: "application/xml"}, false)
	if r.Format != "XML" {
		t.Fatalf("format = %q", r.Format)
	}
	if r.Text != want {
		t.Errorf("text =\n%s\nwant\n%s", r.Text, want)
	}
}

// Namespace prefixes, entity references and attribute quoting come through
// exactly as they were sent: the formatter copies each tag's own text.
func TestRenderXMLKeepsTokensVerbatim(t *testing.T) {
	body := `<s:Envelope xmlns:s="u"><s:Body><v a='1' b="&amp;&lt;">&#65;&amp;b</v></s:Body></s:Envelope>`
	r := Render([]byte(body), Headers{ContentType: "text/xml"}, false)
	for _, want := range []string{`<s:Envelope xmlns:s="u">`, `<v a='1' b="&amp;&lt;">`, `&#65;&amp;b`, `</s:Envelope>`} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("%s missing from\n%s", want, r.Text)
		}
	}
}

func TestRenderXMLWithDoctype(t *testing.T) {
	body := `<!DOCTYPE note SYSTEM "note.dtd"><note><to>you</to></note>`
	want := "<!DOCTYPE note SYSTEM \"note.dtd\">\n<note>\n  <to>you</to>\n</note>"
	r := Render([]byte(body), Headers{ContentType: "application/xml"}, false)
	if r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
}

func TestRenderXMLLongTextGetsItsOwnLine(t *testing.T) {
	long := strings.Repeat("x", inlineText+1)
	r := Render([]byte("<a><b>"+long+"</b></a>"), Headers{ContentType: "application/xml"}, false)
	want := "<a>\n  <b>\n    " + long + "\n  </b>\n</a>"
	if r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
}

func TestRenderXMLMixedContent(t *testing.T) {
	r := Render([]byte("<p>before<b>bold</b>after</p>"), Headers{ContentType: "application/xml"}, false)
	want := "<p>\n  before\n  <b>bold</b>\n  after\n</p>"
	if r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
}

func TestRenderBrokenXMLLeftAlone(t *testing.T) {
	for _, body := range []string{
		"<a><b></a>",      // tags don't match
		"<a><b></b>",      // never closed
		"</a>",            // closed but never opened
		"<a>&nosuch;</a>", // undefined entity
		"<a attr=1></a>",  // unquoted attribute
		"<a><b>text</a></b>",
	} {
		r := Render([]byte(body), Headers{ContentType: "application/xml"}, false)
		if r.Format != "" || r.Text != body {
			t.Errorf("%q: format = %q, text = %q", body, r.Format, r.Text)
		}
	}
}

// HTML is left as it came: its whitespace counts inside <pre> and
// <textarea>, and re-indenting a page under examination would change the
// thing being examined.
func TestRenderLeavesHTMLAlone(t *testing.T) {
	body := "<html><body><pre>keep\n  me</pre></body></html>"
	for _, ct := range []string{"text/html", "text/html; charset=utf-8", "application/xhtml+xml"} {
		r := Render([]byte(body), Headers{ContentType: ct}, false)
		if r.Format != "" {
			t.Errorf("%s: format = %q", ct, r.Format)
		}
		if r.Text != body {
			t.Errorf("%s: text = %q", ct, r.Text)
		}
	}
}

// Formatting is skipped when it would change nothing, so the note stays
// quiet about it.
func TestRenderAlreadyFormatted(t *testing.T) {
	body := "{\n  \"a\": 1\n}"
	r := Render([]byte(body), Headers{ContentType: "application/json"}, false)
	if r.Format != "" || r.Note() != "" {
		t.Errorf("format = %q, note = %q", r.Format, r.Note())
	}
}

func TestRenderBinaryBodyIsNotFormatted(t *testing.T) {
	r := Render([]byte{0x00, 0x01, 0x02}, Headers{ContentType: "application/json"}, false)
	if !r.Binary || r.Format != "" {
		t.Errorf("binary = %v, format = %q", r.Binary, r.Format)
	}
}

func TestRenderSkipsHugeBodies(t *testing.T) {
	body := `{"a":"` + strings.Repeat("x", maxFormat) + `"}`
	r := Render([]byte(body), Headers{ContentType: "application/json"}, false)
	if r.Format != "" {
		t.Errorf("format = %q, want none for a body over the cap", r.Format)
	}
}

// Decoding and formatting happen together: a compressed JSON API response is
// both decompressed and laid out.
func TestRenderDecodesThenFormats(t *testing.T) {
	r := Render(gz(t, `{"a":[1,2]}`), Headers{ContentType: "application/json", ContentEncoding: "gzip"}, false)
	if r.Text != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Errorf("text = %q", r.Text)
	}
	if got := r.Note(); got != "decoded from gzip · formatted as JSON" {
		t.Errorf("note = %q", got)
	}
}
