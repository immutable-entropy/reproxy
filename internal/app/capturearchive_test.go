package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
	"reproxy/internal/target"
)

// record puts one flow through the store the way the proxy does.
func record(a *App, path string) uint64 {
	id := a.Store().Add(capture.Flow{
		Method: "GET", Host: "example.test", Path: path, TargetID: "t-1",
		Request: capture.Message{Raw: []byte("GET " + path + " HTTP/1.1\r\n\r\n"), Size: 20},
		Started: time.Now(),
	})
	a.Store().Complete(id, capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n"), Size: 19},
		200, "200 OK", time.Now())
	return id
}

// flush waits for the archive writer, which the UI never has to do.
func flush(t *testing.T, a *App) {
	t.Helper()
	if cur := a.recorder.Current(); cur != nil {
		if err := cur.Flush(); err != nil {
			t.Fatal(err)
		}
	}
}

// newAppAt builds an app over an existing data directory, as a second launch
// would.
func newAppAt(t *testing.T, configPath string) *App {
	t.Helper()
	a, err := New(Options{ConfigPath: configPath, PortOverride: freePort(t),
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// With no engagement open, traffic still goes somewhere: the data
// directory's own archive, and it comes back on the next launch.
func TestTrafficIsRecordedWithoutAnEngagement(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	record(a, "/one")
	flush(t, a)

	if want := filepath.Join(filepath.Dir(configPath), archive.FileName); a.recorder.Path() != want {
		t.Fatalf("recording to %q, want %q", a.recorder.Path(), want)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	next := newAppAt(t, configPath)
	flows := next.Store().List()
	if len(flows) != 1 || flows[0].Path != "/one" {
		t.Fatalf("after relaunching: %+v", flows)
	}
	if string(flows[0].Response.Raw) != "HTTP/1.1 200 OK\r\n\r\n" {
		t.Errorf("the response wasn't restored: %q", flows[0].Response.Raw)
	}
}

// Traffic follows the engagement: what was captured before one was created
// stays where it was, and what comes after goes into the engagement.
func TestOpeningAnEngagementSwapsTheArchive(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	dataDir := filepath.Dir(configPath)
	record(a, "/before")
	flush(t, a)

	e, err := a.Engagements().Create(filepath.Join(dataDir, "engagements", "acme"),
		"Acme", "", []target.Target{target.New(target.KindDomain, "example.test", target.Palette[0])})
	if err != nil {
		t.Fatal(err)
	}
	if a.recorder.Path() != e.CapturePath() {
		t.Fatalf("recording to %q, want the engagement's archive", a.recorder.Path())
	}
	// The window follows: the flow from before isn't this engagement's.
	if n := a.Store().Len(); n != 0 {
		t.Fatalf("the store still holds %d flows from before the engagement", n)
	}

	record(a, "/inside")
	flush(t, a)
	if _, err := os.Stat(e.CapturePath()); err != nil {
		t.Fatalf("the engagement has no archive: %v", err)
	}

	// Closing it puts the earlier traffic back.
	if err := a.Engagements().Close(); err != nil {
		t.Fatal(err)
	}
	flows := a.Store().List()
	if len(flows) != 1 || flows[0].Path != "/before" {
		t.Fatalf("after closing the engagement: %+v", flows)
	}

	// And reopening it brings its own back.
	if _, err := a.Engagements().Open(e.Dir); err != nil {
		t.Fatal(err)
	}
	flows = a.Store().List()
	if len(flows) != 1 || flows[0].Path != "/inside" {
		t.Fatalf("after reopening the engagement: %+v", flows)
	}
}

// The engagement reopened on the next launch comes back with its traffic.
func TestRestoredEngagementBringsItsTraffic(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	dir := filepath.Join(filepath.Dir(configPath), "engagements", "acme")
	if _, err := a.Engagements().Create(dir, "Acme", "", nil); err != nil {
		t.Fatal(err)
	}
	record(a, "/kept")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	next := newAppAt(t, configPath)
	if e, ok := next.Engagements().Current(); !ok || e.Name != "Acme" {
		t.Fatalf("Current = %+v, %v", e, ok)
	}
	flows := next.Store().List()
	if len(flows) != 1 || flows[0].Path != "/kept" {
		t.Fatalf("relaunched with %+v", flows)
	}
	// Editing a restored flow updates its row rather than adding one.
	next.Store().Update(flows[0].ID, func(f *capture.Flow) { f.Error = "edited" })
	flush(t, next)
	if n, err := next.recorder.Current().Count(); err != nil || n != 1 {
		t.Fatalf("Count = %d, %v; want 1", n, err)
	}
}

// Clearing the requests clears the engagement's archive too: the list and
// what is kept on disk are the same thing now.
func TestClearEmptiesTheEngagementArchive(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	record(a, "/one")
	record(a, "/two")
	flush(t, a)

	a.Store().Clear()
	flush(t, a)
	if n, err := a.recorder.Current().Count(); err != nil || n != 0 {
		t.Fatalf("Count = %d, %v; want 0", n, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if n := newAppAt(t, configPath).Store().Len(); n != 0 {
		t.Errorf("relaunched with %d flows after a clear", n)
	}
}

// The Explorer is drawn from the engagement's entities, which outlive
// Clear Requests and come back on the next launch.
func TestEntitiesOutliveClearRequests(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	record(a, "/users/1")
	record(a, "/users/2")
	record(a, "/about")
	a.indexer.Flush()
	if n := a.Entities().Len(); n != 5 {
		t.Fatalf("entities = %d, want 5 (/users/{id}, /about, their origin, name and domain)", n)
	}

	a.Store().Clear()
	flush(t, a)
	a.indexer.Flush()
	if n := a.Entities().Len(); n != 5 {
		t.Fatalf("entities after Clear Requests = %d, want 5", n)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	next := newAppAt(t, configPath)
	e, ok := next.Entities().Get("http://example.test/users/{id}")
	if !ok || e.Hits != 2 || len(e.URLAttrs.Sightings) != 2 {
		t.Fatalf("URL after relaunch = %+v, %v", e, ok)
	}
	if d, _ := next.Entities().Get("example.test"); d.Hits != 3 {
		t.Errorf("domain hits after relaunch = %d, want 3", d.Hits)
	}
}

// Traffic archived before the Explorer existed is indexed once, on the
// first launch that opens it.
func TestArchivedTrafficIsMappedOnce(t *testing.T) {
	a, configPath := newApp(t, freePort(t))
	record(a, "/one")
	record(a, "/two")
	flush(t, a)
	path := a.recorder.Path()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// Make the file look freshly upgraded: no entities, the marker set.
	ar, err := archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM entities`,
		`INSERT INTO entity_meta (key, value) VALUES ('backfill', '1')`} {
		if _, err := ar.DB().Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	ar.Close()

	next := newAppAt(t, configPath)
	deadline := time.Now().Add(5 * time.Second)
	for next.Entities().NeedsBackfill() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if next.Entities().NeedsBackfill() {
		t.Fatal("backfill never finished")
	}
	if n := next.Entities().Len(); n != 5 {
		t.Fatalf("entities after backfill = %d, want 5", n)
	}
	if e, _ := next.Entities().Get("http://example.test/one"); e.Hits != 1 {
		t.Errorf("/one hits = %d, want 1", e.Hits)
	}
}
