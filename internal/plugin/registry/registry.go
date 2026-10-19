// Package registry lists and downloads plugins from remote repositories, which
// is where plugins come from (the Plugins window's +, or the browse window).
//
// A repository is a GitHub repository holding one plugin per top-level
// directory (or a single plugin at its root). The registry reads the repo's
// file tree, finds the directories with a plugin.json, and reads each one's
// manifest for its name, version and description. Downloading a plugin fetches
// its files — the manifest, the .ts it is built from, and src/ — into a
// directory the caller gives.
//
// It is Qt-free and depends on nothing internal. All network access goes
// through an injected Getter, so the caller controls how requests are made
// (the app sends them through the configured upstream proxy) and tests need
// no network.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Getter fetches a URL and returns its body, the HTTP status, and any
// transport error.
type Getter func(ctx context.Context, url string) (body []byte, status int, err error)

// Limits on what a download will pull, so a hostile repository can't fill
// the disk.
const (
	MaxFiles     = 200
	MaxFileBytes = 2 << 20
	MaxTotal     = 16 << 20
)

// Plugin is a plugin found in a repository.
type Plugin struct {
	// Repo is the repository URL it came from.
	Repo string
	Name string
	// Dir is the plugin's directory within the repository ("" for a repo
	// that is a single plugin at its root).
	Dir         string
	Version     string
	Description string
	Author      string
	// Installed is set by the caller when a plugin of this name is already
	// present in the plugins directory.
	Installed bool
	// InstalledVersion is the installed copy's version, and Update is set
	// when this version is newer than it and this is where that copy comes
	// from: the same repository and directory (see Origin), or no record at
	// all. The caller sets both, like Installed.
	InstalledVersion string
	Update           bool
	// InstalledByHand says the installed copy has no record of a repository
	// it came from, so updating it replaces files the user put there
	// themselves. The caller sets it, and the UI says so before updating.
	InstalledByHand bool
	// Files are the repository paths of the files to download.
	Files []string

	owner, repo, ref string
}

// OriginFile is where an installed plugin records the repository it came
// from, in the plugin's own directory. A plugin without one was put there by
// hand, and is never offered an update.
const OriginFile = ".reproxy-origin.json"

// Origin is where an installed plugin was downloaded from.
type Origin struct {
	Repo string `json:"repo"`
	Dir  string `json:"dir"`
}

// Origin is where this plugin is downloaded from.
func (p Plugin) Origin() Origin { return Origin{Repo: p.Repo, Dir: p.Dir} }

// Same reports whether two origins name the same plugin: the same GitHub
// repository (however its URL is written) and directory.
func (o Origin) Same(other Origin) bool {
	if o.Dir != other.Dir {
		return false
	}
	ao, ar, ok1 := ParseGitHub(o.Repo)
	bo, br, ok2 := ParseGitHub(other.Repo)
	return ok1 && ok2 && strings.EqualFold(ao, bo) && strings.EqualFold(ar, br)
}

// ParseGitHub reads an owner and repo from a GitHub URL
// (https://github.com/owner/repo[.git][/]). ok is false for anything else.
func ParseGitHub(repoURL string) (owner, repo string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || (u.Host != "github.com" && u.Host != "www.github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// List returns the plugins a repository offers. It reads the repo's file tree
// once, then each plugin's manifest.
func List(ctx context.Context, get Getter, repoURL string) ([]Plugin, error) {
	owner, repo, ok := ParseGitHub(repoURL)
	if !ok {
		return nil, fmt.Errorf("%q is not a GitHub repository URL (https://github.com/owner/repo)", repoURL)
	}
	tree, ref, err := fetchTree(ctx, get, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repoURL, err)
	}

	// A plugin is a directory with a plugin.json. They are looked for in the
	// places a repository keeps them: the repo root (the repo is then a
	// single plugin), a top-level directory, or a directory under the
	// repo's src/ or plugins/ folder. A plugin.json at the root claims the
	// whole repository, so sub-directories aren't also offered then.
	var blobs []string
	manifestDirs := map[string]bool{}
	for _, e := range tree {
		if e.Type != "blob" {
			continue
		}
		blobs = append(blobs, e.Path)
		if dir, ok := pluginDir(e.Path); ok {
			manifestDirs[dir] = true
		}
	}
	if manifestDirs[""] {
		manifestDirs = map[string]bool{"": true} // a root plugin is the only one
	}

	dirs := make([]string, 0, len(manifestDirs))
	for d := range manifestDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	byDir := assignFiles(blobs, dirs)

	var out []Plugin
	for _, dir := range dirs {
		man, err := fetchManifest(ctx, get, owner, repo, ref, path.Join(dir, "plugin.json"))
		if err != nil {
			continue // a directory whose manifest won't read isn't offered
		}
		p := Plugin{
			Repo: repoURL, Name: man.Name, Dir: dir, Version: man.Version,
			Description: man.Description, Author: man.Author.Name,
			Files: keepInstallable(byDir[dir]), owner: owner, repo: repo, ref: ref,
		}
		if p.Name == "" {
			p.Name = lastSegment(dir, repo)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no plugins (a plugin is a directory with a plugin.json)", repoURL)
	}
	return out, nil
}

// Download fetches each of the plugin's files and hands it to write,
// named by its path relative to the plugin's directory in the repository.
// Where the files land is write's business — the caller sandboxes that
// (internal/app writes through an os.Root). It refuses a file that escapes
// the plugin's directory, is too large, or isn't an installable kind.
func (p Plugin) Download(ctx context.Context, get Getter, write func(relPath string, data []byte) error) error {
	if len(p.Files) == 0 {
		return fmt.Errorf("%s has no files to download", p.Name)
	}
	if len(p.Files) > MaxFiles {
		return fmt.Errorf("%s has %d files, more than the %d allowed", p.Name, len(p.Files), MaxFiles)
	}
	var total int
	for _, repoPath := range p.Files {
		rel, ok := relativeTo(p.Dir, repoPath)
		if !ok || !safeRel(rel) {
			return fmt.Errorf("refusing a file outside the plugin: %q", repoPath)
		}
		data, status, err := get(ctx, rawURL(p.owner, p.repo, p.ref, repoPath))
		if err != nil {
			return fmt.Errorf("downloading %s: %w", repoPath, err)
		}
		if status != 200 {
			return fmt.Errorf("downloading %s: HTTP %d", repoPath, status)
		}
		if len(data) > MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte limit", repoPath, len(data), MaxFileBytes)
		}
		total += len(data)
		if total > MaxTotal {
			return fmt.Errorf("%s is larger than the %d-byte total limit", p.Name, MaxTotal)
		}
		if err := write(rel, data); err != nil {
			return err
		}
	}
	return nil
}

// ---- GitHub plumbing ----------------------------------------------------------

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

func fetchTree(ctx context.Context, get Getter, owner, repo string) ([]treeEntry, string, error) {
	// HEAD resolves to the default branch; the response echoes the commit
	// SHA back so the raw downloads pin to the same tree.
	const ref = "HEAD"
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, ref)
	body, status, err := get(ctx, apiURL)
	if err != nil {
		return nil, "", err
	}
	if status == 404 {
		return nil, "", fmt.Errorf("repository not found (is it private?)")
	}
	if status == 403 {
		return nil, "", fmt.Errorf("GitHub rate limit reached; try again later")
	}
	if status == 409 {
		return nil, "", fmt.Errorf("the repository is empty (no commits yet)")
	}
	if status != 200 {
		return nil, "", fmt.Errorf("GitHub returned HTTP %d", status)
	}
	var t struct {
		SHA       string      `json:"sha"`
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, "", fmt.Errorf("reading the repository's file list: %w", err)
	}
	if t.Truncated {
		return nil, "", fmt.Errorf("the repository is too large to list")
	}
	pin := t.SHA
	if pin == "" {
		pin = "HEAD"
	}
	return t.Tree, pin, nil
}

type manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
}

func fetchManifest(ctx context.Context, get Getter, owner, repo, ref, repoPath string) (manifest, error) {
	body, status, err := get(ctx, rawURL(owner, repo, ref, repoPath))
	if err != nil {
		return manifest{}, err
	}
	if status != 200 {
		return manifest{}, fmt.Errorf("HTTP %d", status)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return manifest{}, err
	}
	return m, nil
}

func rawURL(owner, repo, ref, repoPath string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, ref, repoPath)
}

// ---- Path helpers -------------------------------------------------------------

// pluginDir reports whether a repo path is a plugin's manifest, and the
// plugin's directory if so: a plugin.json at the root, in a top-level
// directory, or in a directory directly under the repo's src/ or plugins/.
func pluginDir(repoPath string) (dir string, ok bool) {
	d, file := path.Split(repoPath)
	if file != "plugin.json" {
		return "", false
	}
	d = strings.TrimSuffix(d, "/")
	segs := strings.Split(d, "/")
	switch {
	case d == "": // root
		return "", true
	case len(segs) == 1: // top-level directory
		return d, true
	case len(segs) == 2 && (segs[0] == "src" || segs[0] == "plugins"):
		return d, true
	}
	return "", false
}

// assignFiles groups blob paths under the plugin directory each belongs to —
// the longest matching one, so a plugin nested under another's reach isn't
// double-counted.
func assignFiles(blobs, dirs []string) map[string][]string {
	byDir := map[string][]string{}
	for _, f := range blobs {
		best, bestLen := "", -1
		for _, d := range dirs {
			if (d == "" || strings.HasPrefix(f, d+"/")) && len(d) > bestLen {
				best, bestLen = d, len(d)
			}
		}
		if bestLen >= 0 {
			byDir[best] = append(byDir[best], f)
		}
	}
	return byDir
}

// relativeTo returns repoPath relative to dir ("" dir means repo root).
func relativeTo(dir, repoPath string) (string, bool) {
	if dir == "" {
		return repoPath, true
	}
	rest, ok := strings.CutPrefix(repoPath, dir+"/")
	return rest, ok
}

// safeRel reports whether a relative path stays inside the plugin directory.
func safeRel(rel string) bool {
	if rel == "" || path.IsAbs(rel) {
		return false
	}
	clean := path.Clean(rel)
	return clean == rel && clean != ".." && !strings.HasPrefix(clean, "../")
}

// keepInstallable drops files a plugin doesn't run: only the manifest, the
// script files it is built from, and documentation are downloaded. Tests
// (anything under test/, or *.test.ts) are left out — an installed plugin
// runs without them.
func keepInstallable(files []string) []string {
	var out []string
	for _, f := range files {
		_, name := path.Split(f)
		if name == OriginFile || strings.HasSuffix(name, ".test.ts") || hasSegment(f, "test") || hasSegment(f, "node_modules") {
			continue
		}
		switch path.Ext(name) {
		case ".ts", ".js", ".mjs", ".cjs", ".json", ".md", ".txt":
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

func lastSegment(dir, fallback string) string {
	if dir == "" {
		return fallback
	}
	return path.Base(dir)
}
