// Package engagement is one piece of authorised work and everything
// collected under it.
//
// An engagement is what other tools call a project: a name, a description
// and a directory on the local system that holds all of the state belonging
// to the work — its targets today, and the captured traffic, replay history
// and findings as those become persistent. Keeping it in one directory is
// what makes an engagement saveable, exportable (copy or archive the
// directory) and openable again later, including automatically on the next
// launch (see Manager).
//
// The layout inside an engagement directory is:
//
//	engagement.json   the metadata in Meta
//	targets.json      the hosts in scope (see package target)
//	capture.db        the traffic collected (see package archive)
//	settings.json     the settings the engagement overrides (see
//	                  config.EngagementSettings); absent until one is
//	                  overridden
//
// It is core (it decides where captured state lives) and has no Qt
// dependency: the UI drives a Manager, which repoints the target store at
// the open engagement.
package engagement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"reproxy/internal/target"
)

// Files inside an engagement directory.
const (
	// MetaFile holds Meta and marks a directory as an engagement.
	MetaFile = "engagement.json"
	// TargetsFile holds the engagement's targets.
	TargetsFile = "targets.json"
	// CaptureFile holds the traffic collected under the engagement. It is
	// written by package archive, which this package doesn't import: the
	// engagement only says where it goes.
	CaptureFile = "capture.db"
	// SettingsFile holds the settings the engagement overrides. Package
	// config reads and writes it; this package only says where it is.
	SettingsFile = "settings.json"
)

// Version is the engagement format this build writes. It is recorded in
// every engagement.json so a later build can migrate an older one rather
// than guess.
const Version = 1

// Meta is an engagement's engagement.json: everything about the engagement
// that isn't traffic.
type Meta struct {
	Version int `json:"version"`
	// Name is what the user calls the engagement. It is required.
	Name string `json:"name"`
	// Description is free-form text kept with the engagement.
	Description string    `json:"description,omitempty"`
	Created     time.Time `json:"created"`
	// Opened is when the engagement was last opened, so a future "recent
	// engagements" view can order by it without opening each one.
	Opened time.Time `json:"opened,omitempty"`
}

// Engagement is one engagement on disk: its metadata and the directory that
// holds it.
type Engagement struct {
	Meta
	// Dir is the engagement directory. Everything belonging to the
	// engagement lives under it.
	Dir string
}

// MetaPath is the engagement's engagement.json.
func (e Engagement) MetaPath() string { return filepath.Join(e.Dir, MetaFile) }

// TargetsPath is the engagement's targets file.
func (e Engagement) TargetsPath() string { return filepath.Join(e.Dir, TargetsFile) }

// CapturePath is the engagement's traffic archive.
func (e Engagement) CapturePath() string { return filepath.Join(e.Dir, CaptureFile) }

// SettingsPath is the engagement's settings file.
func (e Engagement) SettingsPath() string { return filepath.Join(e.Dir, SettingsFile) }

// Label is what to show for this engagement: its name, or the directory's
// own name if a hand-edited file left it blank.
func (e Engagement) Label() string {
	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}
	return filepath.Base(e.Dir)
}

// Create lays down a new engagement in dir with the given targets and
// returns it. The directory is created if it doesn't exist; an existing
// engagement, or any other non-empty directory, is refused rather than
// written into — an engagement owns its directory.
func Create(dir, name, description string, targets []target.Target) (Engagement, error) {
	dir = strings.TrimSpace(dir)
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return Engagement{}, errors.New("the engagement needs a name")
	case dir == "":
		return Engagement{}, errors.New("choose where to store the engagement")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Engagement{}, fmt.Errorf("resolving %s: %w", dir, err)
	}
	if err := checkEmpty(abs); err != nil {
		return Engagement{}, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return Engagement{}, fmt.Errorf("creating %s: %w", abs, err)
	}

	now := time.Now()
	e := Engagement{
		Meta: Meta{
			Version:     Version,
			Name:        name,
			Description: strings.TrimSpace(description),
			Created:     now,
			Opened:      now,
		},
		Dir: abs,
	}
	if err := target.WriteFile(e.TargetsPath(), targets); err != nil {
		return Engagement{}, err
	}
	if err := e.save(); err != nil {
		return Engagement{}, err
	}
	return e, nil
}

// checkEmpty reports an error unless dir is missing or holds nothing, so
// Create never scatters an engagement through a directory the user is
// already using for something else.
func checkEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Name() == MetaFile {
			return fmt.Errorf("%s already holds an engagement; open it instead", dir)
		}
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s isn't empty; choose a new folder for the engagement", dir)
	}
	return nil
}

// Open reads the engagement at dir. Being pointed at the engagement.json
// file itself works too, so a file dialog can offer either.
func Open(dir string) (Engagement, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Engagement{}, errors.New("no engagement to open")
	}
	if filepath.Base(dir) == MetaFile {
		dir = filepath.Dir(dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Engagement{}, fmt.Errorf("resolving %s: %w", dir, err)
	}

	e := Engagement{Dir: abs}
	data, err := os.ReadFile(e.MetaPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Engagement{}, fmt.Errorf("%s holds no engagement (no %s)", abs, MetaFile)
	}
	if err != nil {
		return Engagement{}, fmt.Errorf("reading %s: %w", e.MetaPath(), err)
	}
	if err := json.Unmarshal(data, &e.Meta); err != nil {
		return Engagement{}, fmt.Errorf("parsing %s: %w", e.MetaPath(), err)
	}
	if e.Version > Version {
		return Engagement{}, fmt.Errorf("%s was saved by a newer version of reProxy (format %d)",
			e.Label(), e.Version)
	}
	return e, nil
}

// Save writes the engagement's metadata back, for a rename or an edited
// description.
func (e Engagement) Save() error {
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" {
		return errors.New("the engagement needs a name")
	}
	return e.save()
}

func (e Engagement) save() error {
	if e.Version == 0 {
		e.Version = Version
	}
	return target.WriteJSON(e.MetaPath(), e.Meta)
}

// DefaultDir suggests where a new engagement called name should live: a
// folder named after it under engagements/ in the data directory.
func DefaultDir(dataDir, name string) string {
	slug := Slug(name)
	if slug == "" {
		slug = "engagement"
	}
	return filepath.Join(dataDir, "engagements", slug)
}

// Slug turns an engagement name into a folder name: lower case, with runs of
// anything but letters and digits collapsed to a single dash.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}
