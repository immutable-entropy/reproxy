package engagement

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"reproxy/internal/target"
)

// newManager returns a manager and the target store it drives, both rooted
// in a temporary data directory.
func newManager(t *testing.T) (*Manager, *target.Store, string) {
	t.Helper()
	data := t.TempDir()
	store, err := target.NewStore(filepath.Join(data, "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(filepath.Join(data, StateFile), store.Path(), store), store, data
}

func TestCreateAndOpenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	in := []target.Target{target.New(target.KindDomain, "acme.test", target.Palette[0])}

	e, err := Create(dir, "Acme Q4", "Authorised by the platform team", in)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Acme Q4" || e.Version != Version || e.Created.IsZero() {
		t.Fatalf("Create = %+v", e.Meta)
	}
	for _, name := range []string{MetaFile, TargetsFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != e.Name || again.Description != e.Description {
		t.Errorf("Open = %+v", again.Meta)
	}
	// Pointing Open at the metadata file itself works too.
	if _, err := Open(filepath.Join(dir, MetaFile)); err != nil {
		t.Errorf("Open(%s) = %v", MetaFile, err)
	}
	// The targets came with it.
	list, err := target.NewStore(again.TargetsPath())
	if err != nil {
		t.Fatal(err)
	}
	if l := list.List(); len(l) != 1 || l[0].Value != "acme.test" {
		t.Errorf("targets = %+v", l)
	}
}

func TestCreateRefusesBadDestinations(t *testing.T) {
	root := t.TempDir()
	if _, err := Create(filepath.Join(root, "x"), "  ", "", nil); err == nil {
		t.Error("an engagement with no name was created")
	}
	if _, err := Create("", "Named", "", nil); err == nil {
		t.Error("an engagement with no directory was created")
	}

	// An existing engagement is never written over.
	dir := filepath.Join(root, "acme")
	if _, err := Create(dir, "Acme", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(dir, "Acme again", "", nil); err == nil {
		t.Error("an existing engagement was overwritten")
	}
	// Nor is somebody else's directory.
	busy := filepath.Join(root, "busy")
	os.MkdirAll(busy, 0o755)
	os.WriteFile(filepath.Join(busy, "notes.txt"), []byte("mine"), 0o600)
	if _, err := Create(busy, "Busy", "", nil); err == nil {
		t.Error("a non-empty directory was used for an engagement")
	}
}

func TestOpenRejectsADirectoryWithNoEngagement(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("a plain directory was opened as an engagement")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, MetaFile), []byte("{not json"), 0o600)
	if _, err := Open(dir); err == nil {
		t.Error("a corrupt engagement.json was accepted")
	}
	os.WriteFile(filepath.Join(dir, MetaFile), []byte(`{"version":99,"name":"Future"}`), 0o600)
	if _, err := Open(dir); err == nil {
		t.Error("an engagement from a newer format was accepted")
	}
}

// Opening an engagement swaps the target store onto its targets; closing it
// puts the data directory's own targets back.
func TestOpeningAnEngagementSwapsItsTargets(t *testing.T) {
	m, store, data := newManager(t)
	if _, err := store.Create(target.KindDomain, "scratch.test"); err != nil {
		t.Fatal(err)
	}

	changes := 0
	defer m.Subscribe(func() { changes++ })()

	dir := filepath.Join(data, "engagements", "acme")
	e, err := m.Create(dir, "Acme", "", []target.Target{
		target.New(target.KindCIDR, "10.0.0.0/8", target.Palette[1]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cur, ok := m.Current(); !ok || cur.Dir != e.Dir {
		t.Fatalf("Current = %+v, %v", cur, ok)
	}
	if store.Path() != e.TargetsPath() {
		t.Errorf("targets file = %q, want the engagement's", store.Path())
	}
	if _, ok := store.Set().Match("host.test", netip.MustParseAddr("10.1.2.3"), "/"); !ok {
		t.Error("the engagement's targets aren't matching")
	}
	if _, ok := store.Set().Match("scratch.test", netip.Addr{}, "/"); ok {
		t.Error("targets from outside the engagement still match")
	}

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Current(); ok {
		t.Error("an engagement is still open after Close")
	}
	if _, ok := store.Set().Match("scratch.test", netip.Addr{}, "/"); !ok {
		t.Error("closing didn't bring back the data directory's targets")
	}
	if changes != 2 {
		t.Errorf("subscribers called %d times, want 2 (create, close)", changes)
	}
}

// The active engagement is remembered, so the next launch reopens it.
func TestRestoreReopensTheActiveEngagement(t *testing.T) {
	m, _, data := newManager(t)
	dir := filepath.Join(data, "engagements", "acme")
	if _, err := m.Create(dir, "Acme", "", nil); err != nil {
		t.Fatal(err)
	}

	// A fresh manager over the same data directory, as on the next launch.
	next, store, _ := func() (*Manager, *target.Store, string) {
		store, err := target.NewStore(filepath.Join(data, "targets.json"))
		if err != nil {
			t.Fatal(err)
		}
		return NewManager(filepath.Join(data, StateFile), store.Path(), store), store, data
	}()
	if _, ok := next.Current(); ok {
		t.Error("an engagement was open before Restore")
	}
	if err := next.Restore(); err != nil {
		t.Fatal(err)
	}
	cur, ok := next.Current()
	if !ok || cur.Name != "Acme" {
		t.Fatalf("Current = %+v, %v", cur, ok)
	}
	if store.Path() != cur.TargetsPath() {
		t.Errorf("targets file = %q", store.Path())
	}
	if r := next.Recent(); len(r) != 1 || r[0] != cur.Dir {
		t.Errorf("Recent = %v", r)
	}

	// Closing clears it: the launch after that opens nothing.
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	third, _, _ := func() (*Manager, *target.Store, string) {
		store, err := target.NewStore(filepath.Join(data, "targets.json"))
		if err != nil {
			t.Fatal(err)
		}
		return NewManager(filepath.Join(data, StateFile), store.Path(), store), store, data
	}()
	if err := third.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, ok := third.Current(); ok {
		t.Error("Restore opened an engagement that had been closed")
	}
	// The recent list survives closing.
	if r := third.Recent(); len(r) != 1 {
		t.Errorf("Recent = %v", r)
	}
}

// An engagement deleted between sessions is forgotten rather than fatal.
func TestRestoreForgetsAMissingEngagement(t *testing.T) {
	m, _, data := newManager(t)
	dir := filepath.Join(data, "engagements", "gone")
	if _, err := m.Create(dir, "Gone", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	store, err := target.NewStore(filepath.Join(data, "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	next := NewManager(filepath.Join(data, StateFile), store.Path(), store)
	if err := next.Restore(); err == nil {
		t.Error("Restore didn't report the missing engagement")
	}
	if _, ok := next.Current(); ok {
		t.Error("a missing engagement was reported as open")
	}
	if r := next.Recent(); len(r) != 0 {
		t.Errorf("Recent = %v, want the missing engagement forgotten", r)
	}
}

func TestRenameWritesTheMetadata(t *testing.T) {
	m, _, data := newManager(t)
	dir := filepath.Join(data, "engagements", "acme")
	if _, err := m.Create(dir, "Acme", "first", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename("Acme Q4", "second"); err != nil {
		t.Fatal(err)
	}
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Acme Q4" || e.Description != "second" {
		t.Errorf("reopened %+v", e.Meta)
	}
	if err := m.Rename("  ", ""); err == nil {
		t.Error("an engagement was renamed to nothing")
	}
}

func TestSlugAndDefaultDir(t *testing.T) {
	cases := map[string]string{
		"Acme Q4":           "acme-q4",
		"  spaces  here  ":  "spaces-here",
		"Client/Project #2": "client-project-2",
		"!!!":               "",
	}
	for name, want := range cases {
		if got := Slug(name); got != want {
			t.Errorf("Slug(%q) = %q, want %q", name, got, want)
		}
	}
	if got := DefaultDir("/data", "Acme Q4"); got != filepath.Join("/data", "engagements", "acme-q4") {
		t.Errorf("DefaultDir = %q", got)
	}
	// A name with nothing usable in it still gets a folder.
	if got := DefaultDir("/data", "!!!"); got != filepath.Join("/data", "engagements", "engagement") {
		t.Errorf("DefaultDir = %q", got)
	}
}

func TestLabelFallsBackToTheFolderName(t *testing.T) {
	e := Engagement{Dir: filepath.Join("/data", "engagements", "acme")}
	if e.Label() != "acme" {
		t.Errorf("Label = %q", e.Label())
	}
}
