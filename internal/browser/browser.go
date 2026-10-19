// Package browser finds installed browsers and launches them preconfigured
// to use the proxy, with a profile of their own so the user's everyday
// browser is never touched. This is the approach Burp Suite takes: no browser
// engine is embedded in the app.
//
// It isn't core (intercept/edit/replay; see CLAUDE.md) and has no Qt or
// proxy dependency, so it can move into a bundled plugin once plugins exist.
package browser

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Kind is the browser family, which decides how it is configured.
type Kind int

const (
	Chromium Kind = iota // Chrome, Chromium, Edge, Brave: command-line flags
	Firefox              // a profile with a user.js
)

// Browser is an installed browser.
type Browser struct {
	Name string
	Kind Kind
	Path string
}

// Options configure a launch.
type Options struct {
	// ProxyAddr is the address the proxy listens on. Wildcard hosts
	// (0.0.0.0, ::) are reached via loopback.
	ProxyAddr string
	// ProfileRoot is the directory holding one profile per browser, e.g.
	// ~/.reproxy/browser. Snap-packaged browsers can't write there, so theirs
	// lives in the snap's own data directory instead.
	ProfileRoot string
	// URL is opened on launch; empty opens the browser's start page.
	URL string
	// TrustedCA, when set, is a PEM CA certificate installed into the
	// browser's trust store before launch so the proxy's HTTPS interception
	// is trusted. Installing it is best-effort (see Session.Warning).
	TrustedCA []byte
}

// candidate is a place a browser may be installed.
type candidate struct {
	name string
	kind Kind
	// paths are absolute paths, or bare names looked up on PATH.
	paths []string
}

// Detect returns the browsers installed on this machine, in order of
// preference: Chromium-family first (the most common QA target), then
// Firefox.
func Detect() []Browser {
	var found []Browser
	seen := map[string]bool{}
	for _, c := range candidates() {
		for _, p := range c.paths {
			path, ok := resolve(p)
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			found = append(found, Browser{Name: c.name, Kind: c.kind, Path: path})
			break
		}
	}
	return found
}

// FromPath describes a browser at an explicit path. The family is guessed
// from the file name: anything with "firefox" in it is Firefox, everything
// else is treated as Chromium-based.
func FromPath(path string) (Browser, error) {
	resolved, ok := resolve(path)
	if !ok {
		return Browser{}, fmt.Errorf("no browser executable at %s", path)
	}
	kind, name := Chromium, filepath.Base(path)
	if strings.Contains(strings.ToLower(name), "firefox") {
		kind = Firefox
	}
	return Browser{Name: name, Kind: kind, Path: resolved}, nil
}

func resolve(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		found, err := exec.LookPath(p)
		if err != nil {
			return "", false
		}
		p = found
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return "", false
	}
	return p, true
}

// Command prepares b's profile and returns the command that launches it.
// It doesn't start anything, so it can be inspected and tested.
func (b Browser) Command(o Options) (*exec.Cmd, error) {
	host, port, err := net.SplitHostPort(o.ProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy address %q: %w", o.ProxyAddr, err)
	}
	host = dialHost(host)

	profile := b.profileDir(o.ProfileRoot)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("creating browser profile: %w", err)
	}

	var args []string
	switch b.Kind {
	case Firefox:
		if err := writeFirefoxPrefs(profile, host, port); err != nil {
			return nil, err
		}
		args = []string{"-profile", profile, "-no-remote", "-new-instance"}
	default:
		args = []string{
			"--user-data-dir=" + profile,
			"--proxy-server=socks5://" + net.JoinHostPort(host, port),
			// Chrome never proxies loopback by default; that's exactly what
			// we test, so remove the implicit bypass.
			"--proxy-bypass-list=<-loopback>",
			// Every name is resolved by the proxy (SOCKS5 carries names),
			// where it is recorded and can be intercepted. Make sure the
			// browser itself resolves nothing: no DNS prefetch, no secure
			// DNS, nothing that skips the proxy. The proxy is dialled by
			// address, so it needs no lookup either.
			"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE " + host,
			// WebRTC would otherwise reach STUN servers over UDP, around the
			// proxy.
			"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
			"--no-first-run",
			"--no-default-browser-check",
			// --host-resolver-rules is on Chrome's list of flags that raise
			// the yellow "you are using an unsupported command-line flag"
			// infobar. The flag is deliberate and the profile exists only
			// for QA, so mark the launch as a test run (what ChromeDriver
			// does) to keep the warning off every page.
			"--test-type",
			// The proxy only records plain HTTP; don't let the browser
			// quietly try HTTPS first. NetworkTimeServiceQuerying is a
			// plain-HTTP clock check that would clutter the request list.
			"--disable-features=HttpsUpgrades,HttpsFirstBalancedModeAutoEnable,NetworkTimeServiceQuerying",
			// Keep the browser's own update and sync chatter out of the
			// request list; this profile only exists for QA.
			"--disable-background-networking",
			"--disable-component-update",
			"--disable-sync",
		}
		if noGPU() {
			// Without a GPU device (e.g. in a container) Chrome's accelerated
			// compositor maps a window that never paints anything the X
			// server can read back, so it looks invisible. Software rendering
			// fixes it. Real desktops keep hardware acceleration.
			args = append(args, "--disable-gpu")
		}
	}
	if o.URL != "" {
		args = append(args, o.URL)
	}
	return exec.Command(b.Path, args...), nil
}

// noGPU reports whether this machine has no usable GPU device, which on
// Linux means Chrome must fall back to software rendering to display at all.
// Other platforms always have a window server, so it returns false.
func noGPU() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	entries, err := os.ReadDir("/dev/dri")
	return err != nil || len(entries) == 0
}

// dialHost turns a wildcard listen address into one a browser can connect to.
func dialHost(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return host
}

func (b Browser) profileDir(root string) string {
	slug := strings.ToLower(strings.ReplaceAll(b.Name, " ", "-"))
	// Snap confinement only allows the snap's own data directory (and not
	// hidden directories such as ~/.reproxy).
	if after, ok := strings.CutPrefix(b.Path, "/snap/bin/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "snap", strings.Split(after, ".")[0], "common", "reproxy-profile")
		}
	}
	return filepath.Join(root, slug)
}

// writeFirefoxPrefs writes a user.js, which Firefox applies on every start,
// so a changed proxy port takes effect on the next launch.
func writeFirefoxPrefs(profile, host, port string) error {
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("proxy port %q: %w", port, err)
	}
	prefs := []struct {
		name  string
		value any
	}{
		{"network.proxy.type", 1}, // manual
		{"network.proxy.socks", host},
		{"network.proxy.socks_port", portNum},
		{"network.proxy.socks_version", 5},
		{"network.proxy.socks_remote_dns", true},
		// Names are resolved by the proxy, where they are recorded and can
		// be intercepted: no DNS over HTTPS, no prefetching, and WebRTC
		// only through the proxy.
		{"network.proxy.socks5_remote_dns", true},
		{"network.trr.mode", 5}, // off, by choice
		{"network.dns.disablePrefetch", true},
		{"network.dns.disablePrefetchFromHTTPS", true},
		{"network.predictor.enabled", false},
		{"media.peerconnection.ice.proxy_only", true},
		{"network.proxy.no_proxies_on", ""},
		{"network.proxy.allow_hijacking_localhost", true},
		{"network.proxy.share_proxy_settings", false},
		{"dom.security.https_only_mode", false},
		// Quiet first run, and none of Firefox's own background checks in
		// the request list.
		{"browser.shell.checkDefaultBrowser", false},
		{"browser.aboutwelcome.enabled", false},
		{"termsofuse.bypassNotification", true}, // first-run Terms of Use modal
		{"datareporting.policy.dataSubmissionPolicyBypassNotification", true},
		{"browser.startup.homepage_override.mstone", "ignore"},
		{"datareporting.policy.dataSubmissionEnabled", false},
		{"network.captive-portal-service.enabled", false},
		{"network.connectivity-service.enabled", false},
	}
	var b strings.Builder
	b.WriteString("// Written by reProxy on every launch; edits are overwritten.\n")
	for _, p := range prefs {
		v := fmt.Sprint(p.value)
		if s, ok := p.value.(string); ok {
			v = strconv.Quote(s)
		}
		fmt.Fprintf(&b, "user_pref(%q, %s);\n", p.name, v)
	}
	return os.WriteFile(filepath.Join(profile, "user.js"), []byte(b.String()), 0o600)
}

// Session is a launched browser process.
type Session struct {
	Browser Browser
	cmd     *exec.Cmd
	stderr  *tail
	warn    string
	done    chan struct{}
	err     error
	once    sync.Once
}

// Launch starts b. The returned session ends when the browser exits.
//
// If a TrustedCA is given, it is installed into the browser's trust store
// first. A failure there is not fatal (the browser still launches, but HTTPS
// interception will show certificate warnings); it is reported via Warning.
func Launch(b Browser, o Options) (*Session, error) {
	cmd, err := b.Command(o)
	if err != nil {
		return nil, err
	}
	var warn string
	if len(o.TrustedCA) > 0 {
		if err := installCA(b, b.profileDir(o.ProfileRoot), o.TrustedCA); err != nil {
			warn = "could not install the proxy CA: " + err.Error()
		}
	}
	s := &Session{Browser: b, cmd: cmd, stderr: &tail{max: 4 << 10}, warn: warn, done: make(chan struct{})}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", b.Name, err)
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			if msg := s.stderr.lastLine(); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		s.err = err
		close(s.done)
	}()
	return s, nil
}

// Warning reports a non-fatal problem from launch, such as the CA not being
// installable, or "" if there was none.
func (s *Session) Warning() string { return s.warn }

// Done is closed when the browser process has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err reports why the browser exited; only valid after Done is closed.
func (s *Session) Err() error { return s.err }

// FailedEarly waits up to d for the browser to exit, and returns its error
// if it did so unsuccessfully: typically a browser that can't start at all
// (no display, a broken install). A browser that is running, or that handed
// off to an instance already open on the same profile, returns nil.
func (s *Session) FailedEarly(d time.Duration) error {
	select {
	case <-s.done:
		return s.err
	case <-time.After(d):
		return nil
	}
}

// Stop asks the browser to quit and waits briefly for it, killing it if it
// doesn't.
func (s *Session) Stop() {
	s.once.Do(func() {
		select {
		case <-s.done:
			return
		default:
		}
		if runtime.GOOS == "windows" {
			_ = s.cmd.Process.Kill()
		} else {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
}

// ErrNoBrowser is returned when no supported browser is installed.
var ErrNoBrowser = errors.New("no supported browser found (Chrome, Chromium, Edge, Brave or Firefox)")

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
