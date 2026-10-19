package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeBrowser writes an executable shell script named name into dir.
func fakeBrowser(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell-script fake browsers")
	}
}

func TestChromiumCommand(t *testing.T) {
	root := t.TempDir()
	b := Browser{Name: "Google Chrome", Kind: Chromium, Path: "/opt/chrome"}
	cmd, err := b.Command(Options{ProxyAddr: "0.0.0.0:7193", ProfileRoot: root, URL: "http://localhost:3000/"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--user-data-dir=" + filepath.Join(root, "google-chrome"),
		"--proxy-server=socks5://127.0.0.1:7193", // wildcard → loopback
		"--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--test-type", // suppresses Chrome's unsupported-flag infobar
	}
	for _, arg := range want {
		if !slices.Contains(cmd.Args, arg) {
			t.Errorf("missing %q in %q", arg, cmd.Args)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "http://localhost:3000/" {
		t.Errorf("URL not last: %q", cmd.Args)
	}
	if _, err := os.Stat(filepath.Join(root, "google-chrome")); err != nil {
		t.Errorf("profile dir not created: %v", err)
	}
}

func TestFirefoxCommandWritesPrefs(t *testing.T) {
	root := t.TempDir()
	b := Browser{Name: "Firefox", Kind: Firefox, Path: "/opt/firefox"}
	cmd, err := b.Command(Options{ProxyAddr: "[::]:8080", ProfileRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "firefox")
	if !slices.Equal(cmd.Args[1:], []string{"-profile", profile, "-no-remote", "-new-instance"}) {
		t.Errorf("args = %q", cmd.Args)
	}
	prefs, err := os.ReadFile(filepath.Join(profile, "user.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`user_pref("network.proxy.socks", "::1");`,
		`user_pref("network.proxy.socks_port", 8080);`,
		`user_pref("network.proxy.socks_version", 5);`,
		`user_pref("network.proxy.allow_hijacking_localhost", true);`,
	} {
		if !strings.Contains(string(prefs), line) {
			t.Errorf("user.js missing %s:\n%s", line, prefs)
		}
	}
}

func TestSnapProfileLivesInSnapDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := Browser{Name: "Chromium", Kind: Chromium, Path: "/snap/bin/chromium"}
	if got, want := b.profileDir("/home/x/.reproxy/browser"), filepath.Join(home, "snap", "chromium", "common", "reproxy-profile"); got != want {
		t.Fatalf("profileDir = %s, want %s", got, want)
	}
}

func TestDetectAndFromPath(t *testing.T) {
	skipOnWindows(t)
	if runtime.GOOS == "darwin" {
		t.Skip("macOS looks in /Applications, not PATH")
	}
	dir := t.TempDir()
	fakeBrowser(t, dir, "chromium-browser", "exit 0")
	ff := fakeBrowser(t, dir, "firefox", "exit 0")
	t.Setenv("PATH", dir)

	var names []string
	for _, b := range Detect() {
		names = append(names, b.Name)
	}
	if !slices.Equal(names, []string{"Chromium", "Firefox"}) {
		t.Fatalf("Detect = %v", names)
	}

	if b, err := FromPath(ff); err != nil || b.Kind != Firefox {
		t.Fatalf("FromPath(firefox) = %+v, %v", b, err)
	}
	if _, err := FromPath(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("FromPath accepted a missing file")
	}
}

func TestLaunchReportsEarlyFailure(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	b := Browser{Name: "Broken", Kind: Chromium, Path: fakeBrowser(t, dir, "broken", "echo 'Missing X server or $DISPLAY' >&2; exit 1")}
	s, err := Launch(b, Options{ProxyAddr: "127.0.0.1:7193", ProfileRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	err = s.FailedEarly(5 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "Missing X server") {
		t.Fatalf("FailedEarly = %v", err)
	}
}

func TestStopEndsRunningBrowser(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	b := Browser{Name: "Sleepy", Kind: Chromium, Path: fakeBrowser(t, dir, "sleepy", "exec sleep 60")}
	s, err := Launch(b, Options{ProxyAddr: "127.0.0.1:7193", ProfileRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailedEarly(200 * time.Millisecond); err != nil {
		t.Fatalf("running browser reported failure: %v", err)
	}
	s.Stop()
	select {
	case <-s.Done():
	default:
		t.Fatal("still running after Stop")
	}
}
