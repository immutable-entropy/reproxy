package syntaxcheck

import (
	"strings"
	"testing"
)

// want is an expected issue: its line, column and a fragment of its message.
type want struct {
	line, col int
	msg       string
}

func check(t *testing.T, name string, fn func(string) []Issue, src string, wants ...want) {
	t.Helper()
	got := fn(src)
	if len(got) != len(wants) {
		t.Errorf("%s(%q): got %d issues %+v, want %d %+v", name, src, len(got), got, len(wants), wants)
		return
	}
	for i, w := range wants {
		g := got[i]
		if g.Line != w.line || g.Col != w.col || !strings.Contains(g.Message, w.msg) {
			t.Errorf("%s(%q) issue %d: got %d:%d %q, want %d:%d containing %q",
				name, src, i, g.Line, g.Col, g.Message, w.line, w.col, w.msg)
		}
		if g.Offset < 0 || g.Offset+g.Len > len(src) {
			t.Errorf("%s(%q) issue %d: range [%d,+%d) out of bounds", name, src, i, g.Offset, g.Len)
		}
	}
}

func TestJSONValid(t *testing.T) {
	for _, src := range []string{
		``, "  \n", `{}`, `[]`, `0`, `-1.5e+10`, `"x"`, `true`, `null`,
		`{"a":1,"b":[1,2,{"c":null}],"d":"é\n\"q\""}`,
		"{\n  \"name\": \"Zoë\",\n  \"tags\": []\n}\n",
	} {
		check(t, "JSON", JSON, src)
	}
}

func TestJSONErrors(t *testing.T) {
	check(t, "JSON", JSON, `{"a":1`, want{1, 1, "Unclosed '{'"})
	check(t, "JSON", JSON, `[1,2`, want{1, 1, "Unclosed '['"})
	check(t, "JSON", JSON, `{"a":[1,2}`, want{1, 6, "Unclosed '['"})
	check(t, "JSON", JSON, `{"a":"x}`, want{1, 1, "Unclosed '{'"}, want{1, 6, "Unterminated string"})
	check(t, "JSON", JSON, `{a:1}`, want{1, 2, "double-quoted"})
	check(t, "JSON", JSON, `{'a':1}`, want{1, 2, "double quotes"})
	check(t, "JSON", JSON, `{"a":1,}`, want{1, 7, "Trailing comma"})
	check(t, "JSON", JSON, `[1,]`, want{1, 3, "Trailing comma"})
	check(t, "JSON", JSON, `{"a":1 "b":2}`, want{1, 8, "Missing ','"})
	check(t, "JSON", JSON, `{"a" 1}`, want{1, 6, "Expected ':'"})
	check(t, "JSON", JSON, `{"a":}`, want{1, 6, "Expected a value"})
	check(t, "JSON", JSON, `{"a":01}`, want{1, 6, "Invalid number"})
	check(t, "JSON", JSON, `{"a":yes}`, want{1, 6, "must be double-quoted"})
	check(t, "JSON", JSON, `{"a":"\x"}`, want{1, 7, "Invalid escape"})
	check(t, "JSON", JSON, `{} {}`, want{1, 4, "after the JSON value"})
	check(t, "JSON", JSON, `]`, want{1, 1, "Unexpected ']'"})
	check(t, "JSON", JSON, `{"a":1} // c`, want{1, 9, "Comments"})
	// Errors on later lines report line and column; several are reported.
	check(t, "JSON", JSON, "{\n  \"a\": 1,\n  b: 2,\n  \"c\": [1, 2\n}",
		want{3, 3, "double-quoted"}, want{4, 8, "Unclosed '['"})
	// An unterminated string doesn't also report a missing comma after it.
	check(t, "JSON", JSON, "{\n\"a\": \"x,\n\"b\": 1\n}", want{2, 6, "Unterminated string"})
	// Columns count characters, not bytes.
	check(t, "JSON", JSON, `{"é":1,}`, want{1, 7, "Trailing comma"})
}

func TestXML(t *testing.T) {
	check(t, "XML", XML, ``)
	check(t, "XML", XML, `<?xml version="1.0"?><a x="1"><b/><c>t</c></a>`)
	check(t, "XML", XML, `<a><b></a>`, want{1, 4, "<b> is never closed"})
	check(t, "XML", XML, `<a></b></a>`, want{1, 4, "Unexpected </b>"})
	check(t, "XML", XML, `<a>`, want{1, 1, "<a> is never closed"})
	check(t, "XML", XML, `<a/><b/>`, want{1, 5, "Only one root"})
	check(t, "XML", XML, `<a x=1/>`, want{1, 7, "Unquoted"})
}

func TestHTML(t *testing.T) {
	check(t, "HTML", HTML, `<!doctype html><html><head><meta charset="utf-8"><title>a < b</title></head>`+
		`<body><p>one<p>two<br><img src="x"><ul><li>a<li>b</ul><svg><path d="M0"/></svg>`+
		`<script>if (a < b) { x = "</div>" }</script><!-- <div> --></body></html>`)
	check(t, "HTML", HTML, `<div><span></div>`, want{1, 6, "<span> is never closed"})
	check(t, "HTML", HTML, `<div>`, want{1, 1, "<div> is never closed"})
	check(t, "HTML", HTML, `</span>`, want{1, 1, "Unexpected </span>"})
	check(t, "HTML", HTML, `<a href="x>link</a>`, want{1, 9, "Unterminated attribute"})
	check(t, "HTML", HTML, `<div class="a"<span></span></div>`, want{1, 1, "missing its closing '>'"})
	check(t, "HTML", HTML, `<p><!-- x`, want{1, 4, "Unterminated comment"})
	check(t, "HTML", HTML, `<script>x`, want{1, 1, "<script> is never closed"})
}

func TestJavaScript(t *testing.T) {
	check(t, "JS", JavaScript, "function f(a) {\n  return a.map(x => x / 2).filter(y => /[)}]/g.test(y)) // )\n}\n"+
		"const s = `a ${b ? `${c}` : '}'} d`; /* { */ let q = 'it\\'s';")
	check(t, "JS", JavaScript, `f(a, [1, 2)`, want{1, 6, "Unclosed '['"})
	check(t, "JS", JavaScript, `if (x) {`, want{1, 8, "Unclosed '{'"})
	check(t, "JS", JavaScript, `x = 1)`, want{1, 6, "Unexpected ')'"})
	check(t, "JS", JavaScript, "x = 'abc\ny = 2", want{1, 5, "Unterminated string"})
	check(t, "JS", JavaScript, "x = `abc", want{1, 5, "Unterminated template"})
	check(t, "JS", JavaScript, "x = `a ${b`", want{1, 8, "Unclosed '${'"}, want{1, 11, "Unterminated template"})
	check(t, "JS", JavaScript, "/* x", want{1, 1, "Unterminated comment"})
	check(t, "JS", JavaScript, "return /ab", want{1, 8, "Unterminated regular"})
}

func TestCSS(t *testing.T) {
	check(t, "CSS", CSS, `@import "a.css"; a:hover, .b > c { color: red; background: url("x.png") } `+
		`@media (min-width: 10px) { .x { margin: 0 } } .p { & .q { top: 0 } }`)
	check(t, "CSS", CSS, `a { color: red`, want{1, 3, "Unclosed '{'"})
	check(t, "CSS", CSS, `a { color: red } }`, want{1, 18, "Unexpected '}'"})
	check(t, "CSS", CSS, `a { color red; }`, want{1, 5, "Expected ':'"})
	check(t, "CSS", CSS, `a { content: "x }`, want{1, 3, "Unclosed '{'"}, want{1, 14, "Unterminated string"})
	check(t, "CSS", CSS, `/* a`, want{1, 1, "Unterminated comment"})
}

// FuzzCheckers makes sure no input panics a checker or yields an issue range
// outside the text.
func FuzzCheckers(f *testing.F) {
	for _, s := range []string{`{"a":[1,"xé",{}]}`, `<a b="c"><d/></a>`, "x = `${a}`/re/g", `a{b:c}`, "\"\\", "<!--", "{'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, fn := range []func(string) []Issue{JSON, XML, HTML, JavaScript, CSS} {
			for _, is := range fn(s) {
				if is.Offset < 0 || is.Len < 0 || is.Offset+is.Len > len(s) || is.Line < 1 || is.Col < 1 {
					t.Fatalf("bad issue %+v for %q", is, s)
				}
			}
		}
	})
}
