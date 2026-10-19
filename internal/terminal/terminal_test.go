package terminal

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls until cond holds, so a test doesn't race the shell.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// screenText is everything on the session's screen, for assertions.
func screenText(s *Session) string {
	var scr Screen
	s.Snapshot(0, &scr)
	return scr.Text(0, 0, scr.Cols-1, scr.Rows-1)
}

func TestSessionRunsAShellAndShowsItsOutput(t *testing.T) {
	m := NewManager(Options{Shell: "/bin/sh", Dir: func() string { return t.TempDir() }})
	t.Cleanup(m.CloseAll)

	// Events arrive on the pty goroutine; the count is all this needs.
	var events atomic.Int64
	m.Subscribe(func(Event) { events.Add(1) })

	s, err := m.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Name(); got != "sh" {
		t.Errorf("Name() = %q, want sh", got)
	}
	s.Resize(80, 24)

	s.Text("echo hello-from-reproxy\r")
	waitFor(t, "the echoed output", func() bool {
		return strings.Contains(screenText(s), "hello-from-reproxy")
	})

	// The shell's own exit takes the session off the list.
	s.Text("exit\r")
	waitFor(t, "the session to end", func() bool { return len(m.List()) == 0 })
	if !s.Exited() {
		t.Error("the session should know its shell has ended")
	}
	if events.Load() == 0 {
		t.Error("subscribers should have heard about the session")
	}
}

func TestResizeReachesTheShell(t *testing.T) {
	m := NewManager(Options{Shell: "/bin/sh", Dir: func() string { return t.TempDir() }})
	t.Cleanup(m.CloseAll)

	s, err := m.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Resize(100, 30)
	if cols, rows := s.Size(); cols != 100 || rows != 30 {
		t.Fatalf("Size() = %d×%d, want 100×30", cols, rows)
	}
	// The shell reports the size the pty was told about, not the default.
	s.Text("printf 'cols=%s\\n' \"$(tput cols 2>/dev/null || echo ?)\"\r")
	waitFor(t, "the shell's idea of the width", func() bool {
		return strings.Contains(screenText(s), "cols=100")
	})
}

func TestSnapshotKeepsColoursAndScrollback(t *testing.T) {
	m := NewManager(Options{Shell: "/bin/sh", Dir: func() string { return t.TempDir() }})
	t.Cleanup(m.CloseAll)

	s, err := m.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Resize(40, 5)
	// Red text, then enough lines to push it off a five-row screen.
	s.Text("printf '\\033[31mRED\\033[0m\\n'; for i in 1 2 3 4 5 6 7 8; do echo line$i; done\r")
	waitFor(t, "the last line", func() bool { return strings.Contains(screenText(s), "line8") })

	if s.ScrollbackLen() == 0 {
		t.Fatal("lines that scrolled off the top should be kept")
	}
	var scr Screen
	s.Snapshot(s.ScrollbackLen(), &scr) // scrolled all the way back
	if scr.Offset != s.ScrollbackLen() {
		t.Errorf("Offset = %d, want %d", scr.Offset, s.ScrollbackLen())
	}
	// SGR 31 is a name, not a value: the snapshot says "ANSI colour 1" and
	// leaves which red that is to the view's theme.
	red := func(scr *Screen) bool {
		for _, c := range scr.Cells {
			if c.Text == "R" && c.FG.Kind == ColorANSI && c.FG.Index == 1 {
				return true
			}
		}
		return false
	}
	// Not every shell echoes the command the same way, so look on the live
	// screen too: the colour is what matters, wherever the R ended up.
	found := red(&scr)
	if !found {
		s.Snapshot(0, &scr)
		found = red(&scr)
	}
	if !found {
		t.Error("a red R should come through as ANSI colour 1")
	}
}

func TestCleanPasteKeepsOnlyText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":                "plain text",
		"tabs\tand\nlines\r\n":      "tabs\tand\nlines\r\n",
		"héllo, 世界 �":               "héllo, 世界 �",
		"safe\x1b[201~rm -rf ~\r":   "safe[201~rm -rf ~\r", // ends a bracketed paste
		"a\x03b\x04c\x1ad\x00e\x7f": "abcde",               // ^C ^D ^Z NUL DEL
		"x\u009b31my":               "x31my",               // C1 CSI
		"x\x9b31my":                 "x31my",               // raw 0x9b, not UTF-8
		"\x1b":                      "",
	} {
		if got := cleanPaste(in); got != want {
			t.Errorf("cleanPaste(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPasteCannotSendEscapes(t *testing.T) {
	m := NewManager(Options{Shell: "/bin/sh", Dir: func() string { return t.TempDir() }})
	t.Cleanup(m.CloseAll)
	s, err := m.New()
	if err != nil {
		t.Fatal(err)
	}
	s.Resize(80, 24)

	// cat -v shows an ESC that reaches it as ^[.
	s.Text("cat -v\r")
	time.Sleep(200 * time.Millisecond)
	s.Paste("before\x1b[201~after\x03\r")
	waitFor(t, "the pasted line", func() bool { return strings.Contains(screenText(s), "before[201~after") })
	if text := screenText(s); strings.Contains(text, "^[") || strings.Contains(text, "^C") {
		t.Errorf("a control character reached the shell:\n%s", text)
	}
}
