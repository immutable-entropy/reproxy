package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// The line editor the interactive shell reads with: history on the arrow
// keys, completion on Tab, and the editing keys a shell user's fingers
// already know. It is written here rather than taken from a library
// because what it has to do is small and the alternative is another
// dependency on every user's machine for a prompt.
//
// It takes a reader and a writer, not a terminal, so it can be driven by a
// test with a pipe. Putting the real terminal into raw mode is the
// caller's job (cmd/reproxy), because that is where the file descriptor
// is; without raw mode the terminal would send whole lines and none of
// this would be reached.

// ErrInterrupted is returned when the line was abandoned with Ctrl-C. The
// shell discards it and prompts again, as a shell does.
var ErrInterrupted = errors.New("interrupted")

// Completer answers what the word under the cursor could become: the word
// itself, and every candidate that starts with it.
type Completer func(line string, cursor int) (word string, candidates []string)

// Editor reads one line at a time from a terminal in raw mode.
type Editor struct {
	in     *bufio.Reader
	out    io.Writer
	prompt string

	// Width reports the terminal's width in columns, for a line too long
	// to fit; nil or 0 means "assume 80".
	Width func() int
	// Complete offers completions on Tab; nil means Tab inserts nothing.
	Complete Completer

	history []string
	// browsing is where the arrow keys have walked to in the history: one
	// past the end means the line being typed, which is kept in pending so
	// walking back returns to it.
	browsing int
	pending  string

	line []rune
	pos  int // in runes, 0..len(line)
}

// NewEditor returns an editor reading from in and drawing on out.
func NewEditor(in io.Reader, out io.Writer, prompt string) *Editor {
	return &Editor{in: bufio.NewReader(in), out: out, prompt: prompt}
}

// History returns the lines remembered, oldest first.
func (e *Editor) History() []string { return append([]string(nil), e.history...) }

// SetHistory replaces the remembered lines, as a shell is given the
// history file it kept last time.
func (e *Editor) SetHistory(lines []string) {
	e.history = nil
	for _, l := range lines {
		e.remember(l)
	}
	e.browsing = len(e.history)
}

// remember adds a line to the history, skipping blanks and a line that
// just repeats the one before it.
func (e *Editor) remember(line string) {
	line = strings.TrimRight(line, " \t")
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == line {
		return
	}
	e.history = append(e.history, line)
}

// ReadLine reads one line, returning it without its newline. It returns
// io.EOF when the input ends (or Ctrl-D on an empty line) and
// ErrInterrupted when the line was abandoned with Ctrl-C.
func (e *Editor) ReadLine() (string, error) {
	e.line, e.pos = nil, 0
	e.browsing, e.pending = len(e.history), ""
	e.redraw()

	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			if len(e.line) > 0 && errors.Is(err, io.EOF) {
				return e.finish()
			}
			return "", err
		}
		switch r {
		case '\r', '\n':
			return e.finish()
		case 0x03: // Ctrl-C: throw the line away and start again
			e.write("^C\r\n")
			return "", ErrInterrupted
		case 0x04: // Ctrl-D: end of input on an empty line, else delete
			if len(e.line) == 0 {
				e.write("\r\n")
				return "", io.EOF
			}
			e.deleteAt(e.pos)
		case 0x7f, 0x08: // Backspace
			if e.pos > 0 {
				e.pos--
				e.deleteAt(e.pos)
			}
		case '\t':
			e.completeWord()
		case 0x01: // Ctrl-A
			e.pos = 0
			e.redraw()
		case 0x05: // Ctrl-E
			e.pos = len(e.line)
			e.redraw()
		case 0x0b: // Ctrl-K: kill to the end
			e.line = e.line[:e.pos]
			e.redraw()
		case 0x15: // Ctrl-U: kill to the start
			e.line = append([]rune(nil), e.line[e.pos:]...)
			e.pos = 0
			e.redraw()
		case 0x17: // Ctrl-W: kill the word behind the cursor
			e.killWord()
		case 0x0c: // Ctrl-L: clear the screen, keep the line
			e.write("\x1b[H\x1b[2J")
			e.redraw()
		case 0x1b: // an escape sequence: the arrow and editing keys
			e.escape()
		default:
			if unicode.IsPrint(r) {
				e.insert(r)
			}
		}
	}
}

func (e *Editor) finish() (string, error) {
	line := string(e.line)
	e.write("\r\n")
	e.remember(line)
	return line, nil
}

// escape reads the rest of an escape sequence and acts on the ones a line
// editor answers to. Anything else is swallowed rather than typed into the
// line, which is what keeps a stray key from writing rubbish.
func (e *Editor) escape() {
	b, err := e.in.ReadByte()
	if err != nil {
		return
	}
	if b != '[' && b != 'O' {
		return // Alt-something, or an escape on its own
	}
	b, err = e.in.ReadByte()
	if err != nil {
		return
	}
	switch b {
	case 'A':
		e.walkHistory(-1)
	case 'B':
		e.walkHistory(1)
	case 'C':
		if e.pos < len(e.line) {
			e.pos++
			e.redraw()
		}
	case 'D':
		if e.pos > 0 {
			e.pos--
			e.redraw()
		}
	case 'H':
		e.pos = 0
		e.redraw()
	case 'F':
		e.pos = len(e.line)
		e.redraw()
	default:
		// A numbered sequence (Delete is "[3~", Home "[1~"): read to its
		// end and act on the ones worth having.
		if b < '0' || b > '9' {
			return
		}
		num := string(b)
		for {
			c, err := e.in.ReadByte()
			if err != nil {
				return
			}
			if c >= '0' && c <= '9' {
				num += string(c)
				continue
			}
			if c == '~' {
				switch num {
				case "3": // Delete
					e.deleteAt(e.pos)
				case "1", "7": // Home
					e.pos = 0
					e.redraw()
				case "4", "8": // End
					e.pos = len(e.line)
					e.redraw()
				}
			}
			return
		}
	}
}

// walkHistory moves through the remembered lines: up (-1) towards the
// oldest, down (+1) back towards what was being typed, which is kept so
// that walking back returns it unharmed.
func (e *Editor) walkHistory(by int) {
	if len(e.history) == 0 {
		return
	}
	if e.browsing == len(e.history) {
		e.pending = string(e.line)
	}
	next := e.browsing + by
	switch {
	case next < 0:
		next = 0
	case next > len(e.history):
		next = len(e.history)
	}
	if next == e.browsing {
		return
	}
	e.browsing = next
	if next == len(e.history) {
		e.line = []rune(e.pending)
	} else {
		e.line = []rune(e.history[next])
	}
	e.pos = len(e.line)
	e.redraw()
}

// completeWord answers Tab: one candidate is inserted, several share their
// common prefix and are then listed, and none does nothing at all.
func (e *Editor) completeWord() {
	if e.Complete == nil {
		return
	}
	word, candidates := e.Complete(string(e.line), e.byteOffset())
	if len(candidates) == 0 {
		return
	}
	add := CommonPrefix(candidates)
	if len(candidates) == 1 {
		add += " "
	}
	if len(add) > len(word) {
		for _, r := range add[len(word):] {
			e.insert(r)
		}
		return
	}
	// No progress to make: show what it could be, then draw the line again
	// under the list.
	if len(candidates) > 1 {
		e.write("\r\n" + columns(candidates, e.width()) + "\r\n")
		e.redraw()
	}
}

// byteOffset is the cursor as an index into the line's bytes, which is
// what a completer is given.
func (e *Editor) byteOffset() int {
	return len(string(e.line[:e.pos]))
}

func (e *Editor) insert(r rune) {
	e.line = append(e.line, 0)
	copy(e.line[e.pos+1:], e.line[e.pos:])
	e.line[e.pos] = r
	e.pos++
	e.redraw()
}

func (e *Editor) deleteAt(i int) {
	if i < 0 || i >= len(e.line) {
		return
	}
	e.line = append(e.line[:i], e.line[i+1:]...)
	e.redraw()
}

func (e *Editor) killWord() {
	i := e.pos
	for i > 0 && e.line[i-1] == ' ' {
		i--
	}
	for i > 0 && e.line[i-1] != ' ' {
		i--
	}
	e.line = append(append([]rune(nil), e.line[:i]...), e.line[e.pos:]...)
	e.pos = i
	e.redraw()
}

func (e *Editor) width() int {
	if e.Width == nil {
		return 80
	}
	if w := e.Width(); w > 20 {
		return w
	}
	return 80
}

// redraw rewrites the prompt and the line, and puts the cursor back where
// it belongs. A line too long for the terminal is scrolled sideways around
// the cursor rather than wrapped, so the editor only ever owns one row and
// never has to work out where the rest of it went.
func (e *Editor) redraw() {
	width := e.width()
	room := width - len([]rune(e.prompt)) - 1
	if room < 10 {
		room = 10
	}
	line, pos := e.line, e.pos
	for pos > room {
		// Scroll a window along; the cursor sits at the window's end until
		// the line is walked back.
		line = line[1:]
		pos--
	}
	if len(line) > room {
		line = line[:room]
	}
	e.write("\r\x1b[K" + e.prompt + string(line))
	if back := len(line) - pos; back > 0 {
		e.write(fmt.Sprintf("\x1b[%dD", back))
	}
}

func (e *Editor) write(s string) { io.WriteString(e.out, s) }

// columns lays candidates out across the terminal, as a shell lists them.
func columns(words []string, width int) string {
	widest := 0
	for _, w := range words {
		widest = max(widest, len([]rune(w)))
	}
	perLine := max((width+2)/(widest+2), 1)
	var b strings.Builder
	for i, w := range words {
		if i > 0 && i%perLine == 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(w)
		if i%perLine != perLine-1 && i != len(words)-1 {
			b.WriteString(strings.Repeat(" ", widest-len([]rune(w))+2))
		}
	}
	return b.String()
}
