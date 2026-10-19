package terminal

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
)

// start runs the shell on a new pseudo-terminal and begins pumping it.
func (s *Session) start(shell, dir string, extraEnv []string) error {
	cmd := exec.Command(shell, shellArgs(shell)...) //nolint:gosec // the user's own shell
	cmd.Dir = dir
	cmd.Env = environ(extraEnv)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(s.cols), Rows: uint16(s.rows)})
	if err != nil {
		if errors.Is(err, pty.ErrUnsupported) {
			return fmt.Errorf("the terminal isn't supported on this system yet: %w", err)
		}
		return fmt.Errorf("starting %s: %w", shell, err)
	}
	s.pty, s.cmd = f, cmd
	close(s.ready)

	go s.readOut()
	go s.writeIn()
	go s.wait()
	return nil
}

// readOut drains the shell's output into the emulator. It is the only
// goroutine that writes the screen, and it ends when the pty closes.
func (s *Session) readOut() {
	buf := make([]byte, readChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			_, _ = s.emu.Write(buf[:n])
			s.mu.Unlock()
			s.mgr.emit(Event{Kind: Output, ID: s.id})
		}
		if err != nil {
			return // the shell ended, or the session was closed
		}
	}
}

// writeIn sends what the emulator has to say to the shell: the keys the user
// pressed, pasted text, and the replies a program asked the terminal for
// (its size, its colours, where the cursor is).
//
// The emulator's read side is a pipe, so this is the one call on it made
// without the session's lock — it blocks until there is something to send.
// That is also why the session is never ended by closing the emulator:
// closing it would write a flag this read is reading. stop() wakes this
// loop with a byte of its own instead, and the loop has exactly one way
// out, so that byte is always collected.
func (s *Session) writeIn() {
	buf := make([]byte, 4<<10)
	for {
		n, err := s.emu.Read(buf)
		if s.stopped.Load() {
			return
		}
		if n > 0 {
			// A failed write is a shell that has gone: the input is
			// dropped and the loop carries on until stop(), rather than
			// leaving the emulator's pipe with no reader.
			if _, werr := s.pty.Write(buf[:n]); werr != nil {
				s.log.Debug("terminal input dropped", "err", werr)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("terminal input stopped", "err", err)
			}
			time.Sleep(10 * time.Millisecond) // nothing to read and no reason to spin
		}
	}
}

// wait reaps the shell and takes the session off the list, as a terminal
// that has been exited out of should be.
func (s *Session) wait() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.exited = true
	s.mu.Unlock()
	if err != nil {
		s.log.Debug("terminal shell ended", "shell", s.name, "err", err)
	}
	s.mgr.emit(Event{Kind: Exited, ID: s.id})
	s.stop() // ends readOut and writeIn
	s.mgr.remove(s.id)
}

// stop ends the shell and everything pumping it. It is safe to call twice,
// and after it nothing more is sent to the emulator (see Key and Text), so
// the input loop's pipe is left with no one waiting on it.
func (s *Session) stop() {
	s.once.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			// The shell leads the pty's session, so this reaches whatever
			// it was running too.
			_ = s.cmd.Process.Kill()
		}
		s.mu.Lock()
		s.stopped.Store(true)
		s.mu.Unlock()
		if s.pty != nil {
			_ = s.pty.Close() // ends readOut
		}
		// Wake the input loop so it sees the flag. It is still reading
		// (its only way out is that flag), so this write is collected and
		// the goroutine ends.
		go func() { _, _ = s.emu.InputPipe().Write([]byte{0}) }()
	})
}

// Resize tells the shell the screen is a different size. Nothing happens
// when it is the size it already was, so a view may call it on every layout.
func (s *Session) Resize(cols, rows int) {
	if cols < 1 || rows < 1 {
		return
	}
	s.mu.Lock()
	if cols == s.cols && rows == s.rows {
		s.mu.Unlock()
		return
	}
	s.cols, s.rows = cols, rows
	s.emu.Resize(cols, rows)
	f := s.pty
	s.mu.Unlock()

	if f != nil {
		_ = pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	s.mgr.emit(Event{Kind: Output, ID: s.id})
}

// Text types ordinary text, as the keyboard produced it.
func (s *Session) Text(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	if !s.stopped.Load() {
		s.emu.SendText(text)
	}
	s.mu.Unlock()
}

// Paste puts text in as a paste, which a program that asked for bracketed
// paste sees as one block rather than as typing. The text is cleaned first
// (cleanPaste): a clipboard is whatever the user last copied, from a web
// page or a captured response as often as not.
func (s *Session) Paste(text string) {
	text = cleanPaste(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	if !s.stopped.Load() {
		s.emu.Paste(text)
	}
	s.mu.Unlock()
}

// cleanPaste drops every control character but tab, newline and carriage
// return, so a paste is only ever text. Without this, an ESC[201~ in the
// clipboard ends a bracketed paste early and the rest runs as if typed,
// any other escape sequence reaches the shell as a key, and ^C, ^D or ^Z
// act on the program. C1 controls (U+0080 to U+009F, among them the
// one-character CSI) and bytes that aren't UTF-8 go too: some programs
// read a raw 0x9b as CSI.
func cleanPaste(text string) string {
	var b strings.Builder
	dropped := false
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		drop := r == utf8.RuneError && size == 1 || // not UTF-8
			r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f || 0x80 <= r && r <= 0x9f
		switch {
		case drop && !dropped:
			dropped = true
			b.Grow(len(text))
			b.WriteString(text[:i])
		case !drop && dropped:
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	if !dropped {
		return text
	}
	return b.String()
}

// Send writes bytes straight to the shell, for callers that have the
// sequence already.
func (s *Session) Send(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	f, gone := s.pty, s.stopped.Load()
	s.mu.Unlock()
	if f != nil && !gone {
		_, _ = f.Write(p)
	}
}
