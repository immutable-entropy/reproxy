package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	xterm "github.com/charmbracelet/x/term"

	"reproxy/internal/atomicfile"
	"reproxy/internal/cli"
)

// `reproxy shell`: the same commands, typed one after another without the
// program's name, against the same instance throughout.
//
// On a terminal it reads with cli.Editor — history on the arrow keys,
// completion on Tab — which needs the terminal in raw mode, so this is
// where raw mode is turned on and, importantly, off again: only while a
// line is being read, never while a command runs, so a command's output,
// its signals and anything it prints behave exactly as they do outside the
// shell.
//
// Piped input is read line by line instead. A shell script driving the
// shell wants none of the editing, and a pipe has no terminal to put into
// raw mode.

// historyLines is how much of the history is kept between sessions.
const historyLines = 500

func runShell(glob cli.Globals, stdout, stderr io.Writer) int {
	glob.Instance = targetInstance(glob.Instance)
	c, err := dial()
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", err)
		return 1
	}
	s := &shell{client: c, glob: glob, stdout: stdout, stderr: stderr}
	if !xterm.IsTerminal(os.Stdin.Fd()) {
		return s.runPiped()
	}
	return s.runInteractive()
}

// shell is one interactive session.
type shell struct {
	client *client
	glob   cli.Globals
	stdout io.Writer
	stderr io.Writer

	// catalogue is what the instance can run, for completion. It is
	// fetched once and again whenever a command might have changed it, so
	// a plugin enabled in the shell can be used in the next line.
	catalogue cli.Catalogue
}

// runPiped reads lines with no editing, for a script.
func (s *shell) runPiped() int {
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if line == "" && err != nil {
			return 0
		}
		if done := s.handle(strings.TrimRight(line, "\r\n")); done {
			return 0
		}
	}
}

// runInteractive reads with the line editor.
func (s *shell) runInteractive() int {
	where := "the hub's instance"
	if s.glob.Instance != "" {
		where = "instance " + s.glob.Instance
	}
	fmt.Fprintf(s.stdout, "reproxy %s — commands run in %s.\n", version, where)
	fmt.Fprintln(s.stdout, "Type a command without the leading \"reproxy\". Tab completes, the arrow keys")
	fmt.Fprintln(s.stdout, "walk back through what you have typed, \"help\" lists the commands, \"exit\" leaves.")

	s.refreshCatalogue()
	ed := cli.NewEditor(os.Stdin, s.stdout, "reproxy> ")
	ed.Complete = func(line string, cursor int) (string, []string) {
		return s.catalogue.Complete(line, cursor)
	}
	ed.Width = func() int {
		w, _, err := xterm.GetSize(os.Stdout.Fd())
		if err != nil {
			return 0
		}
		return w
	}
	path := historyPath()
	ed.SetHistory(readHistory(path))

	for {
		line, err := s.readLine(ed)
		switch {
		case errors.Is(err, cli.ErrInterrupted):
			continue // Ctrl-C threw the line away; prompt again
		case err != nil:
			// End of input: Ctrl-D, or the terminal went away.
			writeHistory(path, ed.History())
			return 0
		}
		if done := s.handle(line); done {
			writeHistory(path, ed.History())
			return 0
		}
	}
}

// readLine puts the terminal into raw mode for exactly as long as it takes
// to read one line. A command then runs with the terminal as it found it,
// which is what lets a command print, be interrupted, or be piped into
// something without the editor's rules getting in the way.
func (s *shell) readLine(ed *cli.Editor) (string, error) {
	state, err := xterm.MakeRaw(os.Stdin.Fd())
	if err != nil {
		// No raw mode (an odd terminal, or none): fall back to a plain
		// read, so the shell still works without the editing.
		return bufio.NewReader(os.Stdin).ReadString('\n')
	}
	defer xterm.Restore(os.Stdin.Fd(), state)
	return ed.ReadLine()
}

// handle runs one line; done is true when the shell should end.
func (s *shell) handle(line string) (done bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	switch line {
	case "exit", "quit":
		return true
	case "version":
		fmt.Fprintln(s.stdout, "reproxy", version)
		return false
	}
	words, err := cli.Tokenize(line)
	if err != nil {
		fmt.Fprintln(s.stderr, "reproxy:", cli.Message(err))
		return false
	}
	req, lineGlob, err := cli.Split(words)
	if err != nil {
		fmt.Fprintln(s.stderr, "reproxy:", cli.Message(err))
		return false
	}
	if cli.Canonical(req.Namespace) == "shell" {
		fmt.Fprintln(s.stderr, "reproxy: already in the shell")
		return false
	}
	if cli.IsLocal(req.Namespace, req.Action) {
		runLocal(req, s.stdout, s.stderr)
		return false
	}
	target := lineGlob.Instance
	if target == "" {
		target = s.glob.Instance
	}
	printResult(s.client.run(target, req, s.stderr), req, s.stdout, s.stderr)
	// Enabling, disabling or reloading a plugin changes what can be run.
	if cli.Canonical(req.Namespace) == "plugins" {
		s.refreshCatalogue()
	}
	return false
}

// refreshCatalogue asks the instance what it can run. A shell that can't
// reach it still works; it just completes nothing.
func (s *shell) refreshCatalogue() {
	reply := s.client.run(s.glob.Instance, cli.Request{Namespace: "help", JSON: true}, io.Discard)
	if reply.Result == nil || len(reply.Result.JSON) == 0 {
		return
	}
	var cat cli.Catalogue
	if err := json.Unmarshal(reply.Result.JSON, &cat); err == nil {
		// The words the shell answers itself complete like any other.
		s.catalogue = cat.WithWords("exit", "quit")
	}
}

// ---- The history file -------------------------------------------------------

// historyPath is where the shell's history is kept: in the data directory,
// like everything else reproxy remembers. It holds whatever was typed,
// which can include a value a command was given, so it is written private
// to the user.
func historyPath() string {
	dir, err := dataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "shell-history")
}

func readHistory(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimRight(line, "\r"); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func writeHistory(path string, lines []string) {
	if path == "" || len(lines) == 0 {
		return
	}
	if len(lines) > historyLines {
		lines = lines[len(lines)-historyLines:]
	}
	// Private: a command line can carry a token, a password or a finding's
	// value, and this is a file in the user's own directory.
	_ = atomicfile.Write(path, []byte(strings.Join(lines, "\n")+"\n"), atomicfile.Private, 0o755)
}
