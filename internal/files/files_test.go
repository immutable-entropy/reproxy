package files

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// schema is the files table as package archive creates it. The test owns a
// copy rather than importing archive, which would make this package's tests
// depend on the one that depends on it.
const schema = `
CREATE TABLE files (
	seq         INTEGER PRIMARY KEY,
	path        TEXT    NOT NULL,
	bytes       INTEGER NOT NULL,
	sha256      TEXT    NOT NULL,
	media_type  TEXT    NOT NULL,
	kind        TEXT    NOT NULL DEFAULT '',
	source      TEXT    NOT NULL,
	author      TEXT    NOT NULL DEFAULT '',
	batch       TEXT    NOT NULL DEFAULT '',
	batch_label TEXT    NOT NULL DEFAULT '',
	notes       TEXT    NOT NULL DEFAULT '',
	flow_seq    INTEGER NOT NULL DEFAULT 0,
	method      TEXT    NOT NULL DEFAULT '',
	url         TEXT    NOT NULL DEFAULT '',
	host        TEXT    NOT NULL DEFAULT '',
	entity      TEXT    NOT NULL DEFAULT '',
	created     INTEGER NOT NULL,
	updated     INTEGER NOT NULL
);
CREATE UNIQUE INDEX files_path ON files (path);
`

// newStore returns a store writing into a temporary directory, with a
// database when db is true.
func newStore(t *testing.T, withDB bool) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "recovered")
	s := NewStore()
	var db *sql.DB
	if withDB {
		var err error
		db, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "capture.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
	}
	if err := s.Use(db, root); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func draft(path, content string) Draft {
	return Draft{Path: path, Content: []byte(content), Source: SourcePlugin, Author: "spa-recon", Kind: "source-map"}
}

func TestRecordWritesTheFileAndRecordsIt(t *testing.T) {
	s, root := newStore(t, true)
	f, created, err := s.Record(draft("app.test/main/src/api.ts", "export const api = 1;\n"))
	if err != nil || !created {
		t.Fatalf("Record = %+v, created %v, err %v", f, created, err)
	}
	if f.Path != "app.test/main/src/api.ts" || f.Size != 22 || f.MediaType != "text/typescript" {
		t.Errorf("recorded %+v", f)
	}
	if f.Name() != "api.ts" || f.Dir() != "app.test/main/src" || !f.Text() {
		t.Errorf("name %q dir %q text %v", f.Name(), f.Dir(), f.Text())
	}
	data, err := os.ReadFile(filepath.Join(root, "app.test/main/src/api.ts"))
	if err != nil || string(data) != "export const api = 1;\n" {
		t.Fatalf("on disk = %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(root, "app.test/main/src/api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600: recovered source is engagement data", mode)
	}
	if got, err := s.Read(f.ID, 0); err != nil || string(got) != "export const api = 1;\n" {
		t.Errorf("Read = %q, %v", got, err)
	}
}

func TestRecordTheSameBytesAgainChangesNothing(t *testing.T) {
	s, _ := newStore(t, true)
	var events []Event
	s.Subscribe(func(e Event) { events = append(events, e) })

	first, created, err := s.Record(draft("a/b.js", "x=1"))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	again, created, err := s.Record(draft("a/b.js", "x=1"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the same file recovered twice should not be created twice")
	}
	if again.ID != first.ID || s.Len() != 1 {
		t.Errorf("ids %d/%d, %d files", first.ID, again.ID, s.Len())
	}
	if len(events) != 1 || events[0].Kind != Added {
		t.Fatalf("events = %v", events)
	}

	// Different bytes at the same path replace the file and say so.
	changed, created, err := s.Record(draft("a/b.js", "x=2"))
	if err != nil || created {
		t.Fatal(created, err)
	}
	if changed.ID != first.ID || changed.SHA256 == first.SHA256 {
		t.Errorf("changed = %+v", changed)
	}
	if len(events) != 2 || events[1].Kind != Updated {
		t.Fatalf("events = %v", events)
	}
}

func TestRecordConfinesAHostilePath(t *testing.T) {
	s, root := newStore(t, true)
	outside := filepath.Join(filepath.Dir(root), "escaped.txt")
	for _, path := range []string{
		"../../escaped.txt",
		"/etc/cron.d/x",
		"webpack://../../escaped.txt",
		"C:\\Windows\\escaped.txt",
		"\x00virtual:/../escaped.txt",
	} {
		f, _, err := s.Record(draft(path, "pwned"))
		if err != nil {
			continue // refused outright is fine too
		}
		if strings.Contains(f.Path, "..") || filepath.IsAbs(f.Path) {
			t.Errorf("%q was recorded as %q", path, f.Path)
		}
		target := filepath.Join(root, filepath.FromSlash(f.Path))
		if !within(root, target) {
			t.Errorf("%q was written to %q, outside %q", path, target, root)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file escaped to %s", outside)
	}
}

func TestRecordRefusesAPathWithNothingSafeLeft(t *testing.T) {
	s, _ := newStore(t, true)
	if _, _, err := s.Record(draft("../..", "x")); err == nil {
		t.Error("a path that cleans away to nothing should be refused")
	}
}

func TestRecordWithNowhereToWrite(t *testing.T) {
	s := NewStore()
	if _, _, err := s.Record(draft("a.js", "x")); !errors.Is(err, ErrNoRoot) {
		t.Errorf("err = %v, want ErrNoRoot", err)
	}
}

func TestRecordKeepsAFileWithNoEngagementOpen(t *testing.T) {
	s, root := newStore(t, false) // a root, but no database
	f, created, err := s.Record(draft("a/b.js", "x=1"))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if s.Recording() {
		t.Error("Recording should be false with no database")
	}
	if _, err := os.Stat(filepath.Join(root, "a/b.js")); err != nil {
		t.Errorf("the file should still be written: %v", err)
	}
	if got, ok := s.Get(f.ID); !ok || got.Path != "a/b.js" {
		t.Errorf("the record should be kept for the session: %+v, %v", got, ok)
	}
}

func TestRecordValidatesTheDraft(t *testing.T) {
	s, _ := newStore(t, true)
	if _, _, err := s.Record(Draft{Path: "a.js", Source: "nobody"}); err == nil {
		t.Error("an unknown source should be refused")
	}
	if _, _, err := s.Record(Draft{Path: "a.js", Source: SourcePlugin}); err == nil {
		t.Error("a plugin's file with no author should be refused")
	}
	if _, _, err := s.Record(Draft{Path: "a.js", Content: []byte("x")}); err != nil {
		t.Errorf("an empty source is the user's: %v", err)
	}
}

func TestRecordAllSharesOneBatchAndEndsWithBatchDone(t *testing.T) {
	s, _ := newStore(t, true)
	var kinds []EventKind
	var batch Batch
	s.Subscribe(func(e Event) {
		kinds = append(kinds, e.Kind)
		if e.Kind == BatchDone {
			batch = e.Batch
		}
	})

	drafts := []Draft{
		draft("app.test/main/src/a.ts", "a"),
		draft("app.test/main/src/b.ts", "bb"),
		draft("app.test/main/node_modules/vue/dist/vue.js", "ccc"),
	}
	for i := range drafts {
		drafts[i].BatchLabel = "https://app.test/assets/main.js.map"
		drafts[i].FlowID = 42
		drafts[i].Host = "app.test"
	}
	res, err := s.RecordAll(drafts, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes != 6 || res.Unchanged != 0 || len(res.Recorded) != 3 {
		t.Fatalf("result = %+v", res)
	}
	if res.Batch == "" {
		t.Fatal("a recovery should have a batch")
	}
	if len(kinds) != 4 || kinds[3] != BatchDone {
		t.Fatalf("events = %v, want three Added then BatchDone", kinds)
	}
	if batch.ID != res.Batch || batch.Files != 3 || batch.Label != "https://app.test/assets/main.js.map" {
		t.Errorf("batch = %+v", batch)
	}
	// The whole recovery is findable by its batch, however many per-file
	// events a plugin missed.
	if got := s.Select(Query{Batch: res.Batch}); len(got) != 3 {
		t.Errorf("Select by batch = %d files", len(got))
	}
	if b := s.Batches(); len(b) != 1 || b[0].Files != 3 || b[0].FlowID != 42 {
		t.Errorf("Batches = %+v", b)
	}
}

func TestRecordAllCountsWhatWasAlreadyThere(t *testing.T) {
	s, _ := newStore(t, true)
	drafts := []Draft{draft("a.js", "x"), draft("b.js", "y")}
	if _, err := s.RecordAll(drafts, Limits{}); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordAll(drafts, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 || res.Unchanged != 2 {
		t.Errorf("a second identical recovery: %+v", res)
	}
}

func TestRecordAllObeysItsLimits(t *testing.T) {
	s, _ := newStore(t, true)
	res, err := s.RecordAll([]Draft{draft("a.js", "aaa"), draft("b.js", "bbb"), draft("c.js", "ccc")},
		Limits{MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "file limit") {
		t.Errorf("file limit: %+v", res)
	}

	s2, _ := newStore(t, true)
	res, err = s2.RecordAll([]Draft{draft("a.js", "aaaa"), draft("b.js", "bbbb")}, Limits{MaxBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Bytes != 4 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "size limit") {
		t.Errorf("size limit: %+v", res)
	}
}

func TestRecordAllReportsAnUnsafePathRatherThanFailing(t *testing.T) {
	s, _ := newStore(t, true)
	res, err := s.RecordAll([]Draft{draft("../..", "x"), draft("ok.js", "y")}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || len(res.Skipped) != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestRemoveDeletesTheBytesAndPrunesEmptyDirectories(t *testing.T) {
	s, root := newStore(t, true)
	f, _, err := s.Record(draft("app.test/main/src/a.ts", "a"))
	if err != nil {
		t.Fatal(err)
	}
	keep, _, err := s.Record(draft("app.test/other.js", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "app.test/main/src/a.ts")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file survived Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "app.test/main")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory it emptied survived: %v", err)
	}
	// The root and a sibling's directory stay.
	if _, err := os.Stat(filepath.Join(root, "app.test/other.js")); err != nil {
		t.Errorf("a sibling was taken with it: %v", err)
	}
	if _, ok := s.Get(f.ID); ok {
		t.Error("the record survived Remove")
	}
	if _, ok := s.Get(keep.ID); !ok {
		t.Error("the wrong record was removed")
	}
}

func TestClearEmptiesTheStoreAndTheFilesItWrote(t *testing.T) {
	s, root := newStore(t, true)
	if _, _, err := s.Record(draft("a/b.js", "x")); err != nil {
		t.Fatal(err)
	}
	// Something the store never recorded is left alone.
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Errorf("%d files after Clear", s.Len())
	}
	if _, err := os.Stat(filepath.Join(root, "a/b.js")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a recovered file survived Clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Errorf("Clear took a file the store never recorded: %v", err)
	}
}

func TestFilesSurviveReopeningTheEngagement(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "recovered")
	db, err := sql.Open("sqlite", filepath.Join(dir, "capture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	s := NewStore()
	if err := s.Use(db, root); err != nil {
		t.Fatal(err)
	}
	d := draft("app.test/main/src/api.ts", "export const api = 1;\n")
	d.Notes = "recovered from the build's source map"
	d.FlowID, d.Method, d.URL, d.Host = 7, "GET", "https://app.test/assets/main.js.map", "app.test"
	d.Entity = "https://app.test/assets/main.js.map"
	d.Batch, d.BatchLabel = "b1", "main.js.map"
	first, _, err := s.Record(d)
	if err != nil {
		t.Fatal(err)
	}

	// Another store over the same database: what an engagement reopened
	// gives the user.
	again := NewStore()
	if err := again.Use(db, root); err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get(first.ID)
	if !ok {
		t.Fatal("the file was not reloaded")
	}
	if got.Path != first.Path || got.SHA256 != first.SHA256 || got.Size != first.Size ||
		got.Kind != "source-map" || got.Source != SourcePlugin || got.Author != "spa-recon" ||
		got.Notes != d.Notes || got.FlowID != 7 || got.URL != d.URL || got.Host != "app.test" ||
		got.Entity != d.Entity || got.Batch != "b1" || got.BatchLabel != "main.js.map" ||
		got.MediaType != "text/typescript" {
		t.Errorf("reloaded = %+v", got)
	}
	if !got.Created.Equal(first.Created) || !got.Updated.Equal(first.Updated) {
		t.Errorf("times = %v/%v, want %v/%v", got.Created, got.Updated, first.Created, first.Updated)
	}
	// And the bytes are still readable through it.
	if data, err := again.Read(got.ID, 0); err != nil || string(data) != "export const api = 1;\n" {
		t.Errorf("Read = %q, %v", data, err)
	}
	// Recovering the same path again, through the reopened store, is still
	// recognised as unchanged.
	if _, created, err := again.Record(d); err != nil || created {
		t.Errorf("created %v, err %v: the path should be known from the database", created, err)
	}
}

func TestReadIsBounded(t *testing.T) {
	s, _ := newStore(t, true)
	f, _, err := s.Record(draft("a.js", "0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(f.ID, 4)
	if err != nil || string(got) != "0123" {
		t.Errorf("Read = %q, %v", got, err)
	}
	if _, err := s.Read(999, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestReadReportsAFileTheUserDeleted(t *testing.T) {
	s, root := newStore(t, true)
	f, _, err := s.Record(draft("a.js", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.js")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(f.ID, 0); err == nil {
		t.Error("reading a file that is gone should fail")
	}
}

func TestSelect(t *testing.T) {
	s, _ := newStore(t, true)
	js := draft("app.test/main/a.js", "x")
	js.Host = "app.test"
	png := Draft{Path: "app.test/main/logo.png", Content: []byte("\x89PNG\r\n\x1a\n\x00rest"),
		Source: SourceUser, Kind: "download", Host: "app.test"}
	other := draft("other.test/b.js", "y")
	other.Host = "other.test"
	for _, d := range []Draft{js, png, other} {
		if _, _, err := s.Record(d); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		q    Query
		want int
	}{
		{"everything", Query{}, 3},
		{"by host", Query{Host: "app.test"}, 2},
		{"by source", Query{Source: SourceUser}, 1},
		{"by author", Query{Author: "spa-recon"}, 2},
		{"by kind", Query{Kind: "download"}, 1},
		{"text only", Query{TextOnly: true}, 2},
		{"by dir", Query{Dir: "app.test/main"}, 2},
		{"by text", Query{Text: "logo"}, 1},
		{"limited", Query{Limit: 1}, 1},
	}
	for _, c := range cases {
		if got := s.Select(c.q); len(got) != c.want {
			t.Errorf("%s: %d files, want %d", c.name, len(got), c.want)
		}
	}
	// Newest first.
	if got := s.Select(Query{}); got[0].Path != "other.test/b.js" {
		t.Errorf("first = %q, want the newest", got[0].Path)
	}
}

func TestMediaType(t *testing.T) {
	cases := []struct {
		name, want string
		content    string
	}{
		// .ts must not be video/mp2t, which is what the system mime table
		// says and what would hide recovered TypeScript from a text/* filter.
		{"src/api.ts", "text/typescript", "export {}"},
		{"main.js", "text/javascript", "x=1"},
		{"main.js.map", "application/json", "{}"},
		{"app.vue", "text/x-vue", "<template/>"},
		{"logo.png", "image/png", "\x89PNG"},
		{"LICENSE", "text/plain", "MIT"},
		{"blob", "application/octet-stream", "\x00\x01\x02binary"},
		{"empty", "text/plain", ""},
	}
	for _, c := range cases {
		if got := MediaType(c.name, []byte(c.content)); got != c.want {
			t.Errorf("MediaType(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSafeRel(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"src/api.ts", "src/api.ts", true},
		{"webpack://app/src/api.ts", "app/src/api.ts", true},
		{"../../../../etc/passwd", "etc/passwd", true},
		{"/etc/cron.d/x", "etc/cron.d/x", true},
		{"C:\\Windows\\x", "Windows/x", true},
		{"\x00virtual:/module", "virtual_/module", true},
		{"a/b?query=1#hash", "a/b", true},
		{"a//b/./c", "a/b/c", true},
		{"..", "", false},
		{"", "", false},
		{"/", "", false},
	}
	for _, c := range cases {
		got, ok := SafeRel(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("SafeRel(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSetNotes(t *testing.T) {
	s, _ := newStore(t, true)
	f, _, err := s.Record(draft("a.js", "x"))
	if err != nil {
		t.Fatal(err)
	}
	var updates int
	s.Subscribe(func(e Event) {
		if e.Kind == Updated {
			updates++
		}
	})
	if err := s.SetNotes(f.ID, "looked at this one\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(f.ID)
	if got.Notes != "looked at this one" || updates != 1 {
		t.Errorf("got %+v, %d updates", got, updates)
	}
	if err := s.SetNotes(999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUseResetsTheStore(t *testing.T) {
	s, _ := newStore(t, true)
	if _, _, err := s.Record(draft("a.js", "x")); err != nil {
		t.Fatal(err)
	}
	var reset int
	s.Subscribe(func(e Event) {
		if e.Kind == Reset {
			reset++
		}
	})
	if err := s.Use(nil, ""); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 || s.Recording() || s.Root() != "" || reset != 1 {
		t.Errorf("%d files, recording %v, root %q, %d resets", s.Len(), s.Recording(), s.Root(), reset)
	}
}
