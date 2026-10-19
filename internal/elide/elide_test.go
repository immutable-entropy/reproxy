package elide

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"cut", "hello world", 8, "hello w…"},
		{"trailing space dropped", "hello world", 7, "hello…"},
		{"one rune", "hello", 1, "…"},
		{"zero", "hello", 0, ""},
		{"negative", "hello", -1, ""},
		{"empty", "", 5, ""},
		{"multibyte counts runes", "日本語テスト", 4, "日本語…"},
		// Runes counts runes, not grapheme clusters: a combining mark is a
		// rune of its own, so "é" costs two of the three.
		{"combining", "ééé", 3, "é…"},
	} {
		if got := Runes(tc.in, tc.n); got != tc.want {
			t.Errorf("%s: Runes(%q, %d) = %q, want %q", tc.name, tc.in, tc.n, got, tc.want)
		}
	}
}

// TestRunesNeverSplitsARune is the regression this package exists for: the
// byte-sliced versions it replaced returned invalid UTF-8 for any
// non-ASCII input whose cut missed a rune boundary.
func TestRunesNeverSplitsARune(t *testing.T) {
	in := strings.Repeat("日", 200) // 3 bytes per rune
	for n := 1; n <= 200; n++ {
		out := Runes(in, n)
		if !utf8.ValidString(out) {
			t.Fatalf("Runes(日×200, %d) is not valid UTF-8: % x", n, out)
		}
		if c := utf8.RuneCountInString(out); c > n {
			t.Fatalf("Runes(日×200, %d) is %d runes, over the limit", n, c)
		}
		if !strings.ContainsRune(out, '�') != true {
		}
		if strings.ContainsRune(out, '�') {
			t.Fatalf("Runes(日×200, %d) = %q contains U+FFFD", n, out)
		}
	}
}

func TestTail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"/a/long/path", 6, "…/path"},
		{"日本語テスト", 3, "…スト"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"", 5, ""},
	} {
		if got := Tail(tc.in, tc.n); got != tc.want {
			t.Errorf("Tail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	// Tail keeps the end, Runes keeps the start.
	if a, b := Tail("abcdef", 3), Runes("abcdef", 3); a == b {
		t.Errorf("Tail and Runes agree (%q); they should cut opposite ends", a)
	}
}

func TestTailNeverSplitsARune(t *testing.T) {
	in := strings.Repeat("日", 200)
	for n := 1; n <= 200; n++ {
		if out := Tail(in, n); !utf8.ValidString(out) {
			t.Fatalf("Tail(日×200, %d) is not valid UTF-8: % x", n, out)
		}
	}
}

func TestBytes(t *testing.T) {
	const marker = "\n<truncated>"
	if got := Bytes("hello", 10, marker); got != "hello" {
		t.Errorf("Bytes did not leave a short string alone: %q", got)
	}
	if got := Bytes("hello", 5, marker); got != "hello" {
		t.Errorf("Bytes cut a string of exactly n bytes: %q", got)
	}
	// The marker is not counted against n, so n bytes of content survive.
	if got := Bytes("hello world", 5, marker); got != "hello"+marker {
		t.Errorf("Bytes = %q", got)
	}
	// A budget that lands mid-rune backs off to the boundary before it:
	// 3-byte runes under a 5-byte budget leave one rune, not one and a bit.
	if got := Bytes("日本語", 5, marker); got != "日"+marker {
		t.Errorf("Bytes(日本語, 5) = %q, want one rune plus the marker", got)
	}
	// A budget shorter than the first rune hands the text back whole
	// rather than empty, so the caller's mistake is visible.
	if got := Bytes("日本語", 2, marker); got != "日本語" {
		t.Errorf("Bytes with a sub-rune budget = %q, want the whole string", got)
	}
}

func TestBytesAndPrefixNeverSplitARune(t *testing.T) {
	in := strings.Repeat("日", 200) // 600 bytes, 3 per rune
	for n := 0; n <= 600; n++ {
		pre := Prefix(in, n)
		if !utf8.ValidString(pre) {
			t.Fatalf("Prefix(n=%d) is not valid UTF-8: % x", n, pre)
		}
		if n < len(in) && len(pre) > n {
			t.Fatalf("Prefix(n=%d) is %d bytes, over the budget", n, len(pre))
		}
		if want := n - n%3; len(pre) != want {
			t.Fatalf("Prefix(n=%d) = %d bytes, want %d", n, len(pre), want)
		}
		if b := Bytes(in, n, "!"); !utf8.ValidString(b) {
			t.Fatalf("Bytes(n=%d) is not valid UTF-8", n)
		}
	}
	if pre := Prefix(in, 700); pre != in {
		t.Error("Prefix shortened a string that was under its budget")
	}
}

func TestPrefixEdges(t *testing.T) {
	if got := Prefix("hello", 5); got != "hello" {
		t.Errorf("Prefix(hello, 5) = %q", got)
	}
	if got := Prefix("", 5); got != "" {
		t.Errorf("Prefix on an empty string = %q", got)
	}
	if got := Prefix("hello", -1); got != "" {
		t.Errorf("Prefix with a negative budget = %q", got)
	}
}
