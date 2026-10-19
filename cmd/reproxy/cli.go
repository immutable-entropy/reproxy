package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"reproxy/internal/cli"
	"reproxy/internal/config"
	"reproxy/internal/hub"
	"reproxy/internal/secrets"
)

// The command line's client half. It holds no logic of its own: it splits
// the arguments, finds the running reproxy through the hub, and prints what
// comes back. The command itself is resolved, parsed and run in that
// process, so core commands and plugin commands behave the same way and
// there is one place where a command's flags are known.
//
// The exceptions are the author tools (cli.LocalCommands): they work on a
// directory of the user's, so they run here, in this process, with the
// user's working directory and no reproxy needed.

// commandTimeout bounds one invocation. It is longer than the plugin
// Manager's own budget for a command, so a plugin that runs to its limit is
// cut off there, with its own message, rather than here.
const commandTimeout = 6 * time.Minute

// runCLI is `reproxy <namespace> <action> [--flags]`.
func runCLI(args []string, stdout, stderr io.Writer) int {
	req, glob, err := cli.Split(args)
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", cli.Message(err))
		return 2
	}
	switch cli.Canonical(req.Namespace) {
	case "version":
		fmt.Fprintln(stdout, "reproxy", version)
		return 0
	case "shell":
		return runShell(glob, stdout, stderr)
	}
	if cli.IsLocal(req.Namespace, req.Action) {
		return runLocal(req, stdout, stderr)
	}
	req.Stdin = pipedInput(req.Argv)
	glob.Instance = targetInstance(glob.Instance)

	c, err := dial()
	if err != nil {
		return reportNoInstance(req, err, stdout, stderr)
	}
	return printResult(c.run(glob.Instance, req, stderr), req, stdout, stderr)
}

// runLocal runs an author tool here. Its arguments are parsed against the
// same declaration the app's help prints, so the two can't drift.
func runLocal(req cli.Request, stdout, stderr io.Writer) int {
	cmd, _ := cli.LocalCommand(req.Namespace, req.Action)
	if req.Help {
		fmt.Fprint(stdout, cli.Help(cmd))
		return 0
	}
	vals, err := cli.ParseFlags(cmd, req.Argv)
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", cli.Message(err))
		fmt.Fprint(stderr, "\n"+cli.Help(cmd))
		return 2
	}
	if len(vals.Args()) != 1 {
		fmt.Fprintf(stderr, "reproxy: %s %s needs one directory\n", cmd.Namespace, cmd.Action)
		fmt.Fprint(stderr, "\n"+cli.Help(cmd))
		return 2
	}
	return runAuthorTool(cmd.Action, vals, stdout, stderr)
}

// reportNoInstance explains that nothing is running, and still answers the
// questions that can be answered without one.
func reportNoInstance(req cli.Request, err error, stdout, stderr io.Writer) int {
	if errors.Is(err, errNotRunning) {
		if req.Namespace == "" || cli.Canonical(req.Namespace) == "help" {
			fmt.Fprint(stdout, cli.StaticHelp())
			return 0
		}
		fmt.Fprintln(stderr, "reproxy: no reproxy instance is running; start one and try again")
		return 1
	}
	fmt.Fprintln(stderr, "reproxy:", err)
	return 1
}

// printResult writes an answer the way the user asked for it: what the
// command produced on stdout, and anything that went wrong on stderr, so a
// listing can be piped while its complaints are still read.
func printResult(reply cli.HubReply, req cli.Request, stdout, stderr io.Writer) int {
	if reply.Error != "" {
		fmt.Fprintln(stderr, "reproxy:", reply.Error)
	}
	res := reply.Result
	if res == nil {
		return 1
	}
	if res.Error != "" {
		fmt.Fprintln(stderr, "reproxy:", res.Error)
	}
	switch {
	case req.JSON && len(res.JSON) > 0:
		var pretty bytes.Buffer
		if json.Indent(&pretty, res.JSON, "", "  ") == nil {
			fmt.Fprintln(stdout, pretty.String())
		} else {
			fmt.Fprintln(stdout, string(res.JSON))
		}
	case res.Output != "" && res.Error != "":
		// A complaint and the usage that answers it: the usage is help,
		// not output, so it follows the complaint on stderr.
		fmt.Fprint(stderr, "\n"+ensureNewline(res.Output))
	case res.Output != "":
		fmt.Fprint(stdout, ensureNewline(res.Output))
	}
	return res.Code
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// maxPiped bounds what is read from the pipe: a message, a scope file or a
// body, not a capture.
const maxPiped = 16 << 20

// pipedInput is what was piped into the command line, for a command that
// asked for it. A command running in another process has no other way to
// reach the shell's input, so it travels with the request.
//
// It is read only when an argument is exactly "-", which is how a flag says
// "the piped input". Reading it otherwise would hang every ordinary command
// run with a pipe on standard input and nothing coming down it — which is
// most of them, in a script.
func pipedInput(argv []string) string {
	if !slices.Contains(argv, "-") || isTerminal(os.Stdin) {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxPiped))
	if err != nil {
		return ""
	}
	return string(data)
}

// targetInstance is the instance to run in: the one named with --instance,
// else the one this terminal belongs to. A shell started from the Sessions
// panel carries REPROXY_INSTANCE, so a command typed there acts on the
// window it was typed in rather than on whichever process holds the hub
// role.
func targetInstance(flag string) string {
	if flag != "" {
		return flag
	}
	return strings.TrimSpace(os.Getenv("REPROXY_INSTANCE"))
}

// ---- Reaching the running reproxy ------------------------------------------

// errNotRunning means no process has the hub role: either none is running,
// or none has taken it yet.
var errNotRunning = errors.New("no reproxy instance is running")

// client talks to the hub over loopback HTTP with the shared bearer token,
// exactly as an agent does.
type client struct {
	addr  string
	token string
	http  *http.Client
}

// dataDir is where the group this command line talks to keeps hub.json and
// the secrets file. REPROXY_DATA_DIR points it at another one, which is how
// a second profile — or a test — is reached without the default being
// touched; it matches the app's own -config flag.
func dataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("REPROXY_DATA_DIR")); dir != "" {
		return dir, nil
	}
	path, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func dial() (*client, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, err
	}
	loc, err := hub.Locate(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNotRunning
		}
		return nil, err
	}
	// Read the token only once there is something to talk to: asking for it
	// creates one, and a command line that found no reproxy has no business
	// writing to the secrets file.
	token, err := secrets.New(dir).MCPToken()
	if err != nil {
		return nil, fmt.Errorf("reading the access token: %w", err)
	}
	return &client{addr: loc.Addr, token: token, http: &http.Client{}}, nil
}

// run sends one invocation and returns what came back. A hub that has died
// since hub.json was written is reported as nothing running, because from
// here that is what it is.
func (c *client) run(instance string, req cli.Request, stderr io.Writer) cli.HubReply {
	body, err := json.Marshal(cli.HubRequest{Instance: instance, Request: req})
	if err != nil {
		return cli.HubReply{Error: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	url := "http://" + c.addr + hub.HubPrefix + cli.HubPath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cli.HubReply{Error: err.Error()}
	}
	hreq.Header.Set("Authorization", "Bearer "+c.token)
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(hreq)
	if err != nil {
		return cli.HubReply{Error: errNotRunning.Error() + " (the hub stopped answering)"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return cli.HubReply{Error: fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))}
	}
	var reply cli.HubReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return cli.HubReply{Error: "the instance sent something unreadable: " + err.Error()}
	}
	return reply
}

// isTerminal reports whether f is a terminal, so a pipe is read as a pipe
// and a person is answered as a person.
func isTerminal(f *os.File) bool {
	return xterm.IsTerminal(f.Fd())
}
