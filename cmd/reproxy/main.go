// Command reproxy launches reProxy: a local proxy (SOCKS5, SOCKS4/4a, HTTP
// and HTTPS on one port) that records HTTP
// traffic from your browser, and a desktop UI for inspecting it.
//
//	reproxy [flags]
//	reproxy <namespace> <action> [--flags]   (see reproxy help)
//	reproxy shell
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"reproxy/internal/app"
	"reproxy/internal/config"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// Windows only, and before anything reads os.Stderr: a GUI-subsystem
	// binary starts with no standard streams, so this hands them back when
	// there is a console to write to. A no-op everywhere else.
	attachConsole()
	os.Exit(run(os.Args[1:], os.Stderr))
}

// isCommandLine decides which of the two programs the arguments are for.
// A bare word where a flag would be is a namespace, so it is the command
// line (`reproxy plugins ls`, `reproxy shell`, `reproxy help`), and the
// command line's own global flags may come before it (`reproxy --json
// findings ls`). Anything else — no arguments, or a flag the command line
// doesn't own — is the app.
func isCommandLine(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--instance" || a == "-i":
			i++ // its value, whatever it looks like
		case a == "--json" || a == "-h" || a == "--help":
		case strings.HasPrefix(a, "--instance="):
		case strings.HasPrefix(a, "-") && a != "-":
			return false // an app flag
		default:
			return true // a namespace
		}
	}
	return false
}

func run(args []string, stderr io.Writer) int {
	if isCommandLine(args) {
		return runCLI(args, os.Stdout, stderr)
	}
	fs := flag.NewFlagSet("reproxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: reproxy [flags]\n\n"+
			"Starts the proxy and opens the reProxy window.\n"+
			"Point your browser's proxy at 127.0.0.1:<port>: SOCKS5, SOCKS4/4a,\n"+
			"HTTP and HTTPS are all served on that port.\n\n"+
			"With -headless there is no window: the proxy, the hub and the MCP\n"+
			"server run until the process is signalled. This is what the\n"+
			"reproxy-mcp service runs.\n\n"+
			"  reproxy help                 the command line: act on a running reproxy\n"+
			"  reproxy shell                the same commands, interactively\n\nFlags:\n")
		fs.PrintDefaults()
	}

	defaultConfig, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", err)
		return 1
	}
	var (
		port        = fs.Int("port", 0, fmt.Sprintf("proxy listen port for this session (default: saved setting, initially %d)", config.DefaultProxyPort))
		configPath  = fs.String("config", defaultConfig, "path to the settings file")
		verbose     = fs.Bool("verbose", false, "log connection-level detail to stderr")
		headless    = fs.Bool("headless", false, "run without the window: proxy, hub and MCP server only")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "reproxy: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(os.Stdout, "reproxy", version)
		return 0
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	a, err := app.New(app.Options{ConfigPath: *configPath, PortOverride: *port, Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "reproxy:", err)
		return 1
	}
	a.Start()
	defer a.Close()

	if *headless {
		return runHeadless(a, logger)
	}
	return launchUI(a)
}

// runHeadless keeps the app alive with no UI until the process is asked to
// stop. app.Start has already brought up the proxy and joined the reproxy
// group; if this process holds the hub role it is also serving MCP, which is
// the point of running this way (see the reproxy-mcp service).
//
// Nothing here touches Qt: internal/app is Qt-free, so a headless process
// never opens a display connection.
func runHeadless(a *app.App, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("reproxy running headless", "mcp", a.MCPStatus().Addr, "version", version)
	<-ctx.Done()
	log.Info("reproxy shutting down")
	return 0
}
