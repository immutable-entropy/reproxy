package browser

import (
	"os"
	"path/filepath"
	"runtime"
)

// candidates lists where each supported browser is usually installed on
// this OS, in order of preference.
func candidates() []candidate {
	switch runtime.GOOS {
	case "windows":
		return windowsCandidates()
	case "darwin":
		return macCandidates()
	}
	// Linux and the BSDs: look on PATH. /snap/bin is listed explicitly
	// because it's often missing from PATH in non-login shells.
	return []candidate{
		{"Google Chrome", Chromium, []string{"google-chrome", "google-chrome-stable"}},
		{"Chromium", Chromium, []string{"chromium", "chromium-browser", "/snap/bin/chromium"}},
		{"Microsoft Edge", Chromium, []string{"microsoft-edge", "microsoft-edge-stable"}},
		{"Brave", Chromium, []string{"brave-browser", "brave"}},
		{"Firefox", Firefox, []string{"firefox", "firefox-esr", "/snap/bin/firefox"}},
	}
}

func macCandidates() []candidate {
	apps := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		apps = append(apps, filepath.Join(home, "Applications"))
	}
	in := func(bundle, binary string) []string {
		var out []string
		for _, dir := range apps {
			out = append(out, filepath.Join(dir, bundle, "Contents", "MacOS", binary))
		}
		return out
	}
	return []candidate{
		{"Google Chrome", Chromium, in("Google Chrome.app", "Google Chrome")},
		{"Chromium", Chromium, in("Chromium.app", "Chromium")},
		{"Microsoft Edge", Chromium, in("Microsoft Edge.app", "Microsoft Edge")},
		{"Brave", Chromium, in("Brave Browser.app", "Brave Browser")},
		{"Firefox", Firefox, in("Firefox.app", "firefox")},
	}
}

func windowsCandidates() []candidate {
	var roots []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		if dir := os.Getenv(env); dir != "" {
			roots = append(roots, dir)
		}
	}
	in := func(rel ...string) []string {
		var out []string
		for _, root := range roots {
			out = append(out, filepath.Join(append([]string{root}, rel...)...))
		}
		return out
	}
	return []candidate{
		{"Google Chrome", Chromium, in("Google", "Chrome", "Application", "chrome.exe")},
		{"Chromium", Chromium, in("Chromium", "Application", "chrome.exe")},
		{"Microsoft Edge", Chromium, in("Microsoft", "Edge", "Application", "msedge.exe")},
		{"Brave", Chromium, in("BraveSoftware", "Brave-Browser", "Application", "brave.exe")},
		{"Firefox", Firefox, in("Mozilla Firefox", "firefox.exe")},
	}
}
