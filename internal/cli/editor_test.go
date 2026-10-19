package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// The editor is driven with the bytes a terminal would send, so these
// tests are what a person pressing the keys would get.

const (
	up    = "\x1b[A"
	down  = "\x1b[B"
	left  = "\x1b[D"
	right = "\x1b[C"
	home  = "\x1b[H"
	del   = "\x1b[3~"
	bs    = "\x7f"
	ctrlC = "\x03"
	ctrlD = "\x04"
	ctrlA = "\x01"
	ctrlE = "\x05"
	ctrlU = "\x15"
	ctrlK = "\x0b"
	ctrlW = "\x17"
)

// editor reads keys and throws the drawing away, which is what a test
// cares about unless it is testing the drawing.
func editor(keys string) (*Editor, *strings.Builder) {
	out := &strings.Builder{}
	e := NewEditor(strings.NewReader(keys), out, "> ")
	return e, out
}

func readLine(t *testing.T, keys string) string {
	t.Helper()
	e, _ := editor(keys)
	line, err := e.ReadLine()
	if err != nil {
		t.Fatalf("reading %q: %v", keys, err)
	}
	return line
}

func TestEditorTypesAndEdits(t *testing.T) {
	for _, tc := range []struct{ keys, want string }{
		{keys: "plugins ls\r", want: "plugins ls"},
		{keys: "flows\n", want: "flows"},
		{keys: "flowss" + bs + "\r", want: "flows"},
		{keys: "flows ls" + left + left + "X\r", want: "flows Xls"},
		{keys: "ab" + home + "X\r", want: "Xab"},
		{keys: "abc" + left + del + "\r", want: "ab"},
		{keys: "abc" + ctrlA + "X" + ctrlE + "Y\r", want: "XabcY"},
		{keys: "one two" + ctrlW + "three\r", want: "one three"},
		{keys: "keep this" + ctrlU + "all\r", want: "all"},
		{keys: "keep this" + left + left + left + left + ctrlK + "\r", want: "keep "},
		// A rune that takes more than one byte is one character to edit.
		{keys: "héllo" + bs + "\r", want: "héll"},
	} {
		if got := readLine(t, tc.keys); got != tc.want {
			t.Errorf("%q gave %q, want %q", tc.keys, got, tc.want)
		}
	}
}

func TestEditorHistory(t *testing.T) {
	e, _ := editor("flows ls\r" + "findings ls\r" + up + up + "\r" + up + down + down + "x\r")
	for _, want := range []string{"flows ls", "findings ls"} {
		if got, err := e.ReadLine(); err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	// Two presses of up walks back past the newest to the oldest.
	if got, _ := e.ReadLine(); got != "flows ls" {
		t.Errorf("up up gave %q, want the oldest line", got)
	}
	// Walking back down past the newest returns the line being typed,
	// which was empty, so only what is typed next remains.
	if got, _ := e.ReadLine(); got != "x" {
		t.Errorf("up then down gave %q, want what was being typed", got)
	}
	if h := e.History(); len(h) != 4 {
		t.Errorf("history is %v", h)
	}
}

// What was being typed is kept while the history is walked, and comes back
// untouched.
func TestEditorHistoryKeepsTheUnfinishedLine(t *testing.T) {
	e, _ := editor("first\r" + "half" + up + down + "-done\r")
	e.ReadLine()
	if got, _ := e.ReadLine(); got != "half-done" {
		t.Errorf("got %q, want the half-typed line back", got)
	}
}

func TestEditorSetHistory(t *testing.T) {
	e, _ := editor(up + "\r")
	e.SetHistory([]string{"", "findings ls", "findings ls", "targets ls"})
	if h := e.History(); len(h) != 2 {
		t.Fatalf("blank and repeated lines were kept: %v", h)
	}
	if got, _ := e.ReadLine(); got != "targets ls" {
		t.Errorf("up gave %q, want the last line of the history", got)
	}
}

func TestEditorInterruptAndEOF(t *testing.T) {
	e, _ := editor("half typed" + ctrlC + "flows ls\r" + ctrlD)
	if _, err := e.ReadLine(); !errors.Is(err, ErrInterrupted) {
		t.Errorf("Ctrl-C gave %v, want ErrInterrupted", err)
	}
	if got, err := e.ReadLine(); err != nil || got != "flows ls" {
		t.Errorf("after Ctrl-C: %q, %v", got, err)
	}
	if _, err := e.ReadLine(); !errors.Is(err, io.EOF) {
		t.Errorf("Ctrl-D on an empty line gave %v, want EOF", err)
	}
	// An interrupted line is not remembered: it was thrown away.
	if h := e.History(); len(h) != 1 || h[0] != "flows ls" {
		t.Errorf("history is %v", h)
	}
}

func TestEditorCompletes(t *testing.T) {
	cat := Catalogue{
		Commands: []Command{
			{Namespace: "findings", Action: "ls", Flags: []Flag{{Name: "type"}, {Name: "limit"}}},
			{Namespace: "findings", Action: "get"},
			{Namespace: "flows", Action: "ls"},
		},
		Builtins: Builtins,
		Globals:  GlobalFlags,
	}
	// One candidate: completed, with a space, so the next word can follow.
	e, _ := editor("find\t\r")
	e.Complete = cat.Complete
	if got, _ := e.ReadLine(); got != "findings " {
		t.Errorf("Tab on \"find\" gave %q", got)
	}
	// Several: the common prefix is filled in and the line waits.
	e, _ = editor("f\tlows ls\r")
	e.Complete = cat.Complete
	if got, _ := e.ReadLine(); got != "flows ls" {
		t.Errorf("Tab on \"f\" gave %q", got)
	}
	// An action, then a flag.
	e, _ = editor("findings l\t--ty\t\r")
	e.Complete = cat.Complete
	if got, _ := e.ReadLine(); got != "findings ls --type " {
		t.Errorf("completing an action and a flag gave %q", got)
	}
	// Nothing to complete leaves the line exactly as it was.
	e, _ = editor("nonsense\t\r")
	e.Complete = cat.Complete
	if got, _ := e.ReadLine(); got != "nonsense" {
		t.Errorf("Tab on a word with no candidates gave %q", got)
	}
}

// A line longer than the terminal is scrolled, not wrapped, so the editor
// only ever draws on one row.
func TestEditorScrollsALongLine(t *testing.T) {
	e, out := editor(strings.Repeat("x", 60) + "\r")
	e.Width = func() int { return 30 }
	if got, err := e.ReadLine(); err != nil || len(got) != 60 {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, line := range strings.Split(out.String(), "\r") {
		if len([]rune(line)) > 120 { // allow for the escape sequences
			t.Errorf("a drawn line is %d characters wide", len([]rune(line)))
		}
	}
}

func TestCatalogueComplete(t *testing.T) {
	cat := Catalogue{
		Commands: []Command{
			{Namespace: "findings", Action: "ls", Flags: []Flag{{Name: "type"}, {Name: "limit"}}},
			{Namespace: "flows", Action: "ls"},
			{Namespace: "fuzzer", Action: "run", Plugin: "fuzzer"},
		},
		Builtins: Builtins,
		Globals:  GlobalFlags,
	}
	for _, tc := range []struct {
		line string
		want []string
	}{
		{line: "fi", want: []string{"findings"}},
		{line: "f", want: []string{"findings", "flows", "fuzzer"}},
		{line: "findings ", want: []string{"ls"}},
		{line: "findings l", want: []string{"ls"}},
		{line: "findings ls --", want: []string{"--type", "--limit", "--instance", "--json", "--help"}},
		{line: "findings ls --t", want: []string{"--type"}},
		// A flag already given is not offered again.
		{line: "findings ls --type x --", want: []string{"--limit", "--instance", "--json", "--help"}},
		// A plugin's namespace completes like core's.
		{line: "fuzz", want: []string{"fuzzer"}},
		{line: "nothing", want: nil},
		// The third word is a value, and nothing guesses at those.
		{line: "findings ls x", want: nil},
	} {
		_, got := cat.Complete(tc.line, len(tc.line))
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("Complete(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
	// "h" completes the builtins too.
	if _, got := cat.Complete("he", 2); strings.Join(got, " ") != "help" {
		t.Errorf("builtins are not completed: %v", got)
	}
}

func TestCommonPrefix(t *testing.T) {
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{words: []string{"findings", "flows"}, want: "f"},
		{words: []string{"ls"}, want: "ls"},
		{words: []string{"ls", "get"}, want: ""},
		{words: nil, want: ""},
	} {
		if got := CommonPrefix(tc.words); got != tc.want {
			t.Errorf("CommonPrefix(%v) = %q, want %q", tc.words, got, tc.want)
		}
	}
}
