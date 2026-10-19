package editassist

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// apply runs an edit on text with the cursor marked by '|' and returns the
// result with the new cursor marked the same way.
func apply(t *testing.T, text string, fn func(text string, pos int) (Edit, bool)) string {
	t.Helper()
	pos := strings.IndexByte(text, '|')
	text = text[:pos] + text[pos+1:]
	e, ok := fn(text, pos)
	if !ok {
		return "(no assist)"
	}
	out := text[:e.Start] + e.Text + text[e.End:]
	if e.Cursor < e.Start || e.Cursor > e.Start+len(e.Text) {
		t.Fatalf("cursor %d outside edit %+v", e.Cursor, e)
	}
	return out[:e.Cursor] + "|" + out[e.Cursor:]
}

func typing(lang Lang, ch string) func(string, int) (Edit, bool) {
	return func(text string, pos int) (Edit, bool) { return Type(lang, text, pos, ch) }
}

func newline(lang Lang) func(string, int) (Edit, bool) {
	return func(text string, pos int) (Edit, bool) { return Newline(lang, text, pos) }
}

func backspace(lang Lang) func(string, int) (Edit, bool) {
	return func(text string, pos int) (Edit, bool) { return Backspace(lang, text, pos) }
}

func TestAssists(t *testing.T) {
	cases := []struct {
		name, in string
		fn       func(string, int) (Edit, bool)
		want     string
	}{
		// JSON pairing and stepping over.
		{"brace", `|`, typing(JSON, "{"), `{|}`},
		{"bracket in object", `{"a": |}`, typing(JSON, "["), `{"a": [|]}`},
		{"no pair before word", `|abc`, typing(JSON, "{"), "(no assist)"},
		{"no pair in string", `{"a|"}`, typing(JSON, "{"), "(no assist)"},
		{"quote", `{|}`, typing(JSON, `"`), `{"|"}`},
		{"close quote steps over", `{"ab|"}`, typing(JSON, `"`), `{"ab"|}`},
		{"escaped quote stays in string", `{"a\"|"}`, typing(JSON, `"`), `{"a\""|}`},
		{"closer steps over", `[1|]`, typing(JSON, "]"), `[1]|`},
		{"closer types when absent", `[1|`, typing(JSON, "]"), "(no assist)"},
		{"backspace pair", `{"a": [|]}`, backspace(JSON), `{"a": |}`},
		{"backspace quotes", `{"|"}`, backspace(JSON), `{|}`},
		{"backspace plain", `{"a|"}`, backspace(JSON), "(no assist)"},

		// Enter keeps and deepens indentation.
		{"enter between braces", `{|}`, newline(JSON), "{\n  |\n}"},
		{"enter keeps indent", "{\n    \"a\": 1,|\n}", newline(JSON), "{\n    \"a\": 1,\n    |\n}"},
		{"enter after opener", "{\n\t\"a\": [|", newline(JSON), "{\n\t\"a\": [\n\t\t|"},
		{"enter nested", "{\n  \"a\": {|}\n}", newline(JSON), "{\n  \"a\": {\n    |\n  }\n}"},
		{"enter between tags", `<a>|</a>`, newline(XML), "<a>\n  |\n</a>"},
		{"enter after void", `<br>|`, newline(HTML), "<br>\n|"},

		// Closing tags.
		{"close tag", `<item id="1"|`, typing(XML, ">"), `<item id="1">|</item>`},
		{"self closing", `<item/|`, typing(XML, ">"), "(no assist)"},
		{"html void", `<img src="x"|`, typing(HTML, ">"), "(no assist)"},
		{"gt inside attribute", `<a title="x|`, typing(HTML, ">"), "(no assist)"},
		{"gt in text", `<a>b |`, typing(HTML, ">"), "(no assist)"},
		{"end tag already there", `<a|</a>`, typing(XML, ">"), "(no assist)"},
		{"end tag completion", `<a><b>x<|`, typing(XML, "/"), `<a><b>x</b>|`},
		{"end tag skips closed", `<a><b>x</b><|`, typing(XML, "/"), `<a><b>x</b></a>|`},
		{"end tag html void", `<div><br><|`, typing(HTML, "/"), `<div><br></div>|`},
		{"end tag already inserted", `<li>one<|</li>`, typing(HTML, "/"), `<li>one</li>|`},
		{"end tag none open", `<a></a><|`, typing(XML, "/"), "(no assist)"},
		{"attr quotes", `<a href=|`, typing(HTML, `"`), `<a href="|"`},
		{"attr quote steps over", `<a href="x|"`, typing(HTML, `"`), `<a href="x"|`},
		{"quote in text", `<p>it|`, typing(HTML, "'"), "(no assist)"},
	}
	for _, c := range cases {
		if got := apply(t, c.in, c.fn); got != c.want {
			t.Errorf("%s: %q → %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestOpenElements(t *testing.T) {
	for _, c := range []struct {
		lang Lang
		in   string
		want []string
	}{
		{XML, `<?xml version="1.0"?><a><b x="1>2"><c/></b><d><!-- <e> --><![CDATA[<f>]]>`, []string{"a", "d"}},
		{HTML, `<html><body><p>a<br><img src=x><script>if (a<b) {}</script><div>`, []string{"html", "body", "p", "div"}},
		{HTML, `<DIV><span></div>`, nil},
		{XML, `<a><b`, []string{"a"}},
	} {
		if got := OpenElements(c.lang, c.in); len(got) != len(c.want) || len(got) > 0 && !reflect.DeepEqual(got, c.want) {
			t.Errorf("OpenElements(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTagCompletion(t *testing.T) {
	if p, ok := TagPrefix("<a><di", 6); !ok || p != "di" {
		t.Errorf("TagPrefix = %q, %v", p, ok)
	}
	if p, ok := TagPrefix("<a>", 1); !ok || p != "" {
		t.Errorf("TagPrefix after '<' = %q, %v", p, ok)
	}
	if _, ok := TagPrefix("a < b", 5); ok {
		t.Error("TagPrefix matched text that isn't a tag")
	}
	if got := TagNames(XML, `<order><line/><Line/></order>`); !reflect.DeepEqual(got, []string{"Line", "line", "order"}) {
		t.Errorf("TagNames(XML) = %q", got)
	}
	html := TagNames(HTML, `<my-widget>`)
	if !slices.Contains(html, "div") || !slices.Contains(html, "my-widget") {
		t.Errorf("TagNames(HTML) missing standard or custom names: %q", html)
	}
}

// FuzzAssists makes sure no text or cursor position panics an assist or
// yields an edit outside the text.
func FuzzAssists(f *testing.F) {
	for _, s := range []string{`{"a": [1, "x"]}`, `<a b="c"><d/></a>`, "<p>\n\t<br>", `<![CDATA[`, `"\`} {
		f.Add(s, uint(3))
	}
	f.Fuzz(func(t *testing.T, s string, at uint) {
		pos := int(at % uint(len(s)+1))
		for _, lang := range []Lang{JSON, XML, HTML} {
			edits := []func() (Edit, bool){
				func() (Edit, bool) { return Newline(lang, s, pos) },
				func() (Edit, bool) { return Backspace(lang, s, pos) },
			}
			for _, ch := range []string{"{", "}", "[", "]", `"`, "'", ">", "/"} {
				edits = append(edits, func() (Edit, bool) { return Type(lang, s, pos, ch) })
			}
			for _, fn := range edits {
				e, ok := fn()
				if ok && (e.Start < 0 || e.End < e.Start || e.End > len(s) || e.Cursor < e.Start || e.Cursor > e.Start+len(e.Text)) {
					t.Fatalf("bad edit %+v for %q at %d", e, s, pos)
				}
			}
			_, _ = TagPrefix(s, pos)
			_ = TagNames(lang, s)
			_ = OpenElements(lang, s)
		}
	})
}
