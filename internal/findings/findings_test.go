package findings

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
)

// types stands in for the user's catalog from Settings → Findings.
var types = []Type{
	{ID: "credential", Name: "Credential", Color: "#d6453d"},
	{ID: "sensitive-data", Name: "Sensitive Data", Color: "#d08700"},
	{ID: "error", Name: "Error", Color: "#3b6ef5"},
}

// open returns a store over a real engagement database — package archive
// owns that schema, so testing against it is testing the schema findings
// actually runs on — plus the archive itself, for the traffic a finding
// points at.
func open(t *testing.T) (*Store, *archive.Archive) {
	t.Helper()
	a, err := archive.Open(filepath.Join(t.TempDir(), archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })

	s := NewStore()
	if err := s.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	return s, a
}

func TestRecordMergesOnValueAndUnionsTypes(t *testing.T) {
	s, _ := open(t)

	first, created, err := s.Record(Draft{
		Value: "AKIAQ7",
		Types: []string{"credential"},
		Link:  Link{FlowID: 1, Method: "GET", URL: "https://a.test/me", Location: InRequestHeader, Detail: "Authorization"},
	})
	if err != nil || !created {
		t.Fatalf("first Record: created=%v, err=%v", created, err)
	}
	if !first.Viewed {
		t.Error("a finding the user made should start viewed: they just made it")
	}

	// The same value somewhere else, raised by an agent under another type.
	second, created, err := s.Record(Draft{
		Value:  "AKIAQ7",
		Types:  []string{"Sensitive Data"}, // by name, and a type the finding lacks
		Notes:  "Also returned to unauthenticated callers.",
		Source: SourceAgent,
		Author: "session-1",
		Link:   Link{FlowID: 2, Method: "POST", URL: "https://a.test/auth", Location: InResponseBody},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the same value must merge into the finding that holds it, not make a second")
	}
	if second.ID != first.ID {
		t.Fatalf("merged into finding %d, want %d", second.ID, first.ID)
	}
	if got := strings.Join(second.Types, ","); got != "credential,sensitive-data" {
		t.Errorf("types after merge = %q", got)
	}
	if len(second.Links) != 2 {
		t.Errorf("links after merge = %d, want 2", len(second.Links))
	}
	if second.Viewed {
		t.Error("a sighting raised by an agent is news: the finding should be unviewed again")
	}
	if s.Len() != 1 {
		t.Errorf("store holds %d findings, want 1", s.Len())
	}
	if n := s.Unviewed(); n != 1 {
		t.Errorf("unviewed = %d, want 1", n)
	}

	// The same spot again adds nothing.
	if _, _, err := s.Record(Draft{
		Value: "AKIAQ7", Notes: "Seen again.", Source: SourceAgent,
		Link: Link{FlowID: 2, Method: "POST", URL: "https://a.test/auth", Location: InResponseBody},
	}); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Get(first.ID)
	if len(again.Links) != 2 {
		t.Errorf("links after re-recording the same spot = %d, want 2", len(again.Links))
	}
}

func TestRecordKeepsBothNotesOnMerge(t *testing.T) {
	s, _ := open(t)
	if _, _, err := s.Record(Draft{Value: "token", Notes: "First."}); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.Record(Draft{Value: "token", Notes: "Second.", Source: SourceAgent})
	if err != nil {
		t.Fatal(err)
	}
	if f.Notes != "First.\n\nSecond." {
		t.Errorf("notes after merge = %q", f.Notes)
	}
	// A note already there isn't repeated.
	f, _, err = s.Record(Draft{Value: "token", Notes: "Second.", Source: SourceAgent})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(f.Notes, "Second.") != 1 {
		t.Errorf("note repeated: %q", f.Notes)
	}
}

func TestRecordWithoutAValueNeverMerges(t *testing.T) {
	s, _ := open(t)
	for _, note := range []string{"The login page allows unlimited attempts.", "The logout link is a GET."} {
		if _, created, err := s.Record(Draft{Notes: note}); err != nil || !created {
			t.Fatalf("Record(%q): created=%v, err=%v", note, created, err)
		}
	}
	if s.Len() != 2 {
		t.Errorf("store holds %d findings, want 2: findings with no value have nothing to match on", s.Len())
	}
}

func TestRecordRefusesWhatItCannotRecord(t *testing.T) {
	s, _ := open(t)
	cases := map[string]Draft{
		"nothing at all":          {},
		"an agent with no note":   {Value: "x", Source: SourceAgent},
		"a plugin with no note":   {Value: "x", Source: SourcePlugin},
		"a type that isn't known": {Value: "x", Types: []string{"nonsense"}},
		"an unknown source":       {Value: "x", Source: Source("somewhere")},
	}
	for name, d := range cases {
		if _, _, err := s.Record(d); err == nil {
			t.Errorf("Record with %s was accepted", name)
		}
	}
	if s.Len() != 0 {
		t.Errorf("a refused draft left %d findings behind", s.Len())
	}
}

func TestEditsAndViewing(t *testing.T) {
	s, _ := open(t)
	f, _, err := s.Record(Draft{Value: "secret", Types: []string{"credential"}, Source: SourceAgent,
		Notes: "In the response body.", Author: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Viewed || s.Unviewed() != 1 {
		t.Fatal("an agent's finding should arrive unviewed")
	}
	if err := s.MarkViewed(f.ID); err != nil {
		t.Fatal(err)
	}
	if s.Unviewed() != 0 {
		t.Error("the badge should clear once the finding is opened")
	}

	if err := s.SetFields(f.ID, []Field{{Key: "user", Value: "admin"}, {Key: "", Value: "dropped"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Classify(f.ID, []string{"error"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotes(f.ID, "Rewritten.\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(f.ID)
	if len(got.Fields) != 1 || got.Fields[0].Key != "user" {
		t.Errorf("fields = %+v, want just user (a keyless row is dropped)", got.Fields)
	}
	if strings.Join(got.Types, ",") != "error" {
		t.Errorf("types = %v", got.Types)
	}
	if got.Notes != "Rewritten." {
		t.Errorf("notes = %q", got.Notes)
	}

	// A value another finding holds would be the duplicate Record avoids.
	other, _, err := s.Record(Draft{Value: "another"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetValue(other.ID, "secret"); err == nil {
		t.Error("SetValue onto a value another finding holds was accepted")
	}
	if kept, _ := s.Get(other.ID); kept.Value != "another" {
		t.Errorf("a refused SetValue changed the finding: %q", kept.Value)
	}
}

// Everything a finding carries has to survive being written and read back:
// that is what makes findings the engagement's record rather than a session's.
func TestFindingsRoundTripThroughTheEngagement(t *testing.T) {
	a, err := archive.Open(filepath.Join(t.TempDir(), archive.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	s := NewStore()
	if err := s.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	want, _, err := s.Record(Draft{
		Value:  "AKIAQ7",
		Types:  []string{"credential", "sensitive-data"},
		Notes:  "Static AWS key in the bundle.",
		Fields: []Field{{Key: "account", Value: "4711"}, {Key: "rotated", Value: "no"}},
		Source: SourceAgent,
		Author: "session-1",
		Link:   Link{FlowID: 9, Method: "GET", URL: "https://a.test/app.js", Location: InResponseBody, Detail: "line 42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Record(Draft{Notes: "No rate limiting on /login."}); err != nil {
		t.Fatal(err)
	}

	// A second store over the same database is what reopening looks like.
	reopened := NewStore()
	if err := reopened.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("reopened with %d findings, want 2", reopened.Len())
	}
	got, ok := reopened.Get(want.ID)
	if !ok {
		t.Fatalf("finding %d is gone", want.ID)
	}
	switch {
	case got.Value != want.Value, got.Notes != want.Notes:
		t.Errorf("value/notes = %q / %q", got.Value, got.Notes)
	case strings.Join(got.Types, ",") != "credential,sensitive-data":
		t.Errorf("types = %v (order must survive too)", got.Types)
	case len(got.Fields) != 2 || got.Fields[0].Key != "account" || got.Fields[1].Value != "no":
		t.Errorf("fields = %+v", got.Fields)
	case got.Source != SourceAgent || got.Author != "session-1" || got.Viewed:
		t.Errorf("source/author/viewed = %q / %q / %v", got.Source, got.Author, got.Viewed)
	case len(got.Links) != 1 || got.Links[0].FlowID != 9 || got.Links[0].Detail != "line 42":
		t.Errorf("links = %+v", got.Links)
	case !got.Created.Equal(want.Created) || !got.Updated.Equal(want.Updated):
		t.Errorf("times = %v / %v, want %v / %v", got.Created, got.Updated, want.Created, want.Updated)
	}

	// A new finding in the reopened store must not land on an existing row.
	third, _, err := s.Record(Draft{Value: "third"})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID <= want.ID {
		t.Errorf("new finding took ID %d, at or below %d", third.ID, want.ID)
	}
}

// Clear Requests empties the traffic. The findings are the point of the
// engagement, so they stay, and each sighting still says what it saw.
func TestFindingsSurviveClearingTheTraffic(t *testing.T) {
	s, a := open(t)
	raw := []byte("GET /me HTTP/1.1\r\nHost: a.test\r\n\r\n")
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: capture.Flow{
		ID: 1, Method: "GET", Host: "a.test", Path: "/me",
		Request: capture.Message{Raw: raw, Size: int64(len(raw))},
	}})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	f, _, err := s.Record(Draft{Value: "AKIAQ7", Types: []string{"credential"},
		Link: Link{FlowID: 1, Method: "GET", URL: "https://a.test/me", Location: InRequestHeader, Detail: "Authorization"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Clear(); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore()
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(f.ID)
	if !ok {
		t.Fatal("clearing the traffic took the findings with it")
	}
	if len(got.Links) != 1 {
		t.Fatalf("links = %+v", got.Links)
	}
	if want := "GET https://a.test/me — request header (Authorization)"; got.Links[0].Label() != want {
		t.Errorf("sighting reads %q, want %q", got.Links[0].Label(), want)
	}
}

func TestSelectFiltersAndOrders(t *testing.T) {
	s, _ := open(t)
	one, _, _ := s.Record(Draft{Value: "AKIAQ7", Types: []string{"credential"},
		Link: Link{FlowID: 1, Method: "GET", URL: "https://a.test/me"}})
	two, _, _ := s.Record(Draft{Value: "bob@a.test", Types: []string{"sensitive-data"},
		Notes: "PII in the list response.", Source: SourceAgent,
		Link: Link{FlowID: 2, Method: "GET", URL: "https://a.test/users"}})

	all := s.Select(Query{})
	if len(all) != 2 || all[0].ID != two.ID {
		t.Fatalf("Select returned %d findings, newest first? %+v", len(all), all)
	}
	if got := s.Select(Query{Types: []string{"credential"}}); len(got) != 1 || got[0].ID != one.ID {
		t.Errorf("type filter = %+v", got)
	}
	if got := s.Select(Query{Unviewed: true}); len(got) != 1 || got[0].ID != two.ID {
		t.Errorf("unviewed filter = %+v", got)
	}
	if got := s.Select(Query{FlowID: 2}); len(got) != 1 || got[0].ID != two.ID {
		t.Errorf("flow filter = %+v", got)
	}
	match := func(text string) bool { return strings.Contains(text, "PII") }
	if got := s.Select(Query{Match: match}); len(got) != 1 || got[0].ID != two.ID {
		t.Errorf("text filter = %+v", got)
	}
}

func TestSubscribersSeeEveryChange(t *testing.T) {
	s, _ := open(t)
	var kinds []EventKind
	unsubscribe := s.Subscribe(func(e Event) { kinds = append(kinds, e.Kind) })

	f, _, err := s.Record(Draft{Value: "x", Notes: "note"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkViewed(f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(f.ID); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	if _, _, err := s.Record(Draft{Value: "y", Notes: "note"}); err != nil {
		t.Fatal(err)
	}

	want := []EventKind{Added, Updated, Removed}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
}

func TestRemoveTakesTheRowWithIt(t *testing.T) {
	s, a := open(t)
	f, _, err := s.Record(Draft{Value: "x", Types: []string{"credential"},
		Link: Link{FlowID: 1, Method: "GET", URL: "https://a.test/"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(f.ID); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore()
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 0 {
		t.Errorf("the removed finding came back: %+v", reopened.List())
	}
	// Its types and sightings went with it (ON DELETE CASCADE).
	for _, table := range []string{"finding_types", "finding_flows"} {
		var n int
		if err := a.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows", table, n)
		}
	}
}

func TestTypeCatalog(t *testing.T) {
	s := NewStore()
	if err := s.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"credential", "Credential", "CREDENTIAL", "Sensitive Data", "sensitive-data"} {
		if _, ok := s.ResolveType(spelling); !ok {
			t.Errorf("ResolveType(%q) found nothing", spelling)
		}
	}
	if _, ok := s.ResolveType("nonsense"); ok {
		t.Error("ResolveType invented a type")
	}
	if err := s.SetTypes([]Type{{ID: "a", Name: "A", Color: "#fff"}}); err == nil {
		t.Error("a three-digit colour was accepted")
	}
	if err := s.SetTypes([]Type{
		{ID: "a", Name: "A", Color: "#112233"},
		{ID: "a", Name: "Also A", Color: "#445566"},
	}); err == nil {
		t.Error("two types sharing an id were accepted")
	}
	// A rejected catalog leaves the one in use alone.
	if len(s.Types()) != len(types) {
		t.Errorf("catalog = %+v", s.Types())
	}
}

func TestWithNoEngagementFindingsAreNotRecorded(t *testing.T) {
	s := NewStore()
	if s.Recording() {
		t.Error("a store with no database claims to be recording")
	}
	// It still works for the session: the finding is just not persisted.
	if _, created, err := s.Record(Draft{Value: "x", Notes: "n"}); err != nil || !created {
		t.Fatalf("Record with no database: created=%v, err=%v", created, err)
	}
	if s.Len() != 1 {
		t.Error("the finding should be in memory")
	}
}

func TestPickColorAvoidsWhatIsTaken(t *testing.T) {
	if got := PickColor(nil); got != Palette[0] {
		t.Errorf("PickColor(nil) = %q", got)
	}
	if got := PickColor([]string{Palette[0], strings.ToUpper(Palette[1])}); got != Palette[2] {
		t.Errorf("PickColor = %q, want %q (case must not hide a taken colour)", got, Palette[2])
	}
	// With every colour taken, PickColor starts again at the first.
	if got := PickColor(Palette); got != Palette[0] {
		t.Errorf("PickColor with every colour taken = %q, want %q", got, Palette[0])
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Sensitive Data":            "sensitive-data",
		"  Unexpected   Response  ": "unexpected-response",
		"CSRF/XSS":                  "csrf-xss",
		"":                          "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A plugin's types join the catalog while it is enabled, without overriding
// the user's, and leave with it.
func TestDeclaredTypes(t *testing.T) {
	s := NewStore()
	if err := s.SetTypes([]Type{{ID: "credential", Name: "Credential", Color: "#112233"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeclareTypes("scanner", []Type{{Name: "Credential"}, {Name: "Sensitive Data"}}); err != nil {
		t.Fatal(err)
	}
	types := s.Types()
	if len(types) != 2 || types[0].Color != "#112233" || types[1].ID != "sensitive-data" || types[1].Color == "" {
		t.Fatalf("types = %+v", types)
	}
	if _, ok := s.ResolveType("Sensitive Data"); !ok {
		t.Error("declared type not resolvable")
	}
	// The user's settings being applied again keeps the declared types.
	s.SetTypes([]Type{{ID: "credential", Name: "Credential", Color: "#112233"}})
	if len(s.Types()) != 2 {
		t.Error("SetTypes dropped the declared types")
	}
	s.WithdrawTypes("scanner")
	if len(s.Types()) != 1 {
		t.Errorf("types after withdrawal = %+v", s.Types())
	}
}

func TestMarkAllViewed(t *testing.T) {
	s, _ := open(t)
	for i := 0; i < 3; i++ {
		if _, _, err := s.Record(Draft{Value: fmt.Sprintf("v%d", i), Notes: "n", Source: SourceAgent}); err != nil {
			t.Fatal(err)
		}
	}
	s.MarkViewed(s.List()[0].ID)
	n, err := s.MarkAllViewed()
	if err != nil || n != 2 {
		t.Fatalf("MarkAllViewed = %d, %v", n, err)
	}
	if s.Unviewed() != 0 {
		t.Errorf("unviewed = %d", s.Unviewed())
	}
	if n, _ := s.MarkAllViewed(); n != 0 {
		t.Errorf("second call changed %d", n)
	}
}

// A value that is a secret in itself is recorded hidden, and stays hidden
// across a merge: one sighting of it as a secret is enough. Only the user
// (SetHidden) takes the mask off for good.
func TestHiddenSurvivesAMergeAndAReopen(t *testing.T) {
	s, a := open(t)

	f, _, err := s.Record(Draft{
		Value: "hunter2", Types: []string{"credential"}, Notes: "the password of the test account",
		Source: SourcePlugin, Author: "sensitive-info", Hidden: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Hidden {
		t.Fatal("the finding was recorded in the clear")
	}

	// A second sighting that says nothing about hiding leaves it hidden.
	merged, created, err := s.Record(Draft{
		Value: "hunter2", Notes: "seen again on another endpoint", Source: SourcePlugin, Author: "sensitive-info",
	})
	if err != nil || created {
		t.Fatalf("second sighting: created=%v, err=%v", created, err)
	}
	if !merged.Hidden {
		t.Error("a merge took the mask off")
	}

	// It is still hidden when the engagement is opened again.
	reopened := NewStore()
	if err := reopened.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	again, ok := reopened.Get(f.ID)
	if !ok {
		t.Fatalf("finding %d is gone", f.ID)
	}
	if !again.Hidden {
		t.Error("the mask was lost when the archive was read back")
	}

	// The user can take it off.
	if err := reopened.SetHidden(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if shown, _ := reopened.Get(f.ID); shown.Hidden {
		t.Error("SetHidden(false) left it hidden")
	}
}

func TestRecoveredFileLinksSurviveAReopen(t *testing.T) {
	s, a := open(t)

	f, _, err := s.Record(Draft{
		Value:  "https://app.test/assets/main.js.map",
		Types:  []string{"credential"},
		Notes:  "exposed source map",
		Source: SourcePlugin, Author: "spa-recon",
		Link: Link{FlowID: 7, Method: "GET", URL: "https://app.test/assets/main.js.map", Location: InResponseBody},
		Links: []Link{
			RecoveredFileLink("app.test/main/src/api.ts"),
			RecoveredFileLink("app.test/main/src/routes.ts"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One flow sighting plus two file links.
	files := 0
	for _, l := range f.Links {
		if l.Location == InRecoveredFile {
			files++
		}
	}
	if len(f.Links) != 3 || files != 2 {
		t.Fatalf("got %d links (%d files), want 3 (2)", len(f.Links), files)
	}

	// Recording one of the files again adds nothing: a file link dedupes on
	// its path.
	again, created, err := s.Record(Draft{
		Value: "https://app.test/assets/main.js.map", Notes: "seen again",
		Source: SourcePlugin, Author: "spa-recon",
		Links: []Link{RecoveredFileLink("app.test/main/src/api.ts")},
	})
	if err != nil || created {
		t.Fatalf("merge: created=%v err=%v", created, err)
	}
	if len(again.Links) != 3 {
		t.Fatalf("a duplicate file link was added: %d links", len(again.Links))
	}

	// They survive a reopen — finding_flows holds two file rows that share a
	// seq and location and differ only by their path (the detail).
	reopened := NewStore()
	if err := reopened.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(f.ID)
	if !ok {
		t.Fatalf("finding %d is gone", f.ID)
	}
	paths := map[string]bool{}
	for _, l := range got.Links {
		if l.Location == InRecoveredFile {
			paths[l.Detail] = true
		}
	}
	if !paths["app.test/main/src/api.ts"] || !paths["app.test/main/src/routes.ts"] {
		t.Fatalf("recovered-file links lost on reopen: %v", paths)
	}
}

// What a hidden finding reads as on screen: never the value, and something
// more useful than dots when there is a note to show instead.
func TestMaskedSummary(t *testing.T) {
	f := Finding{Value: "hunter2", Notes: "Password in the request body of POST https://app.test/login"}
	if got := f.SummaryMasked(); got != f.Summary() {
		t.Errorf("a finding that isn't hidden reads as %q, want its summary", got)
	}

	f.Hidden = true
	masked := f.SummaryMasked()
	if strings.Contains(masked, "hunter2") {
		t.Errorf("the masked summary %q holds the value", masked)
	}
	if !strings.Contains(masked, "Password in the request body") {
		t.Errorf("the masked summary %q doesn't say what the finding is", masked)
	}
	if m := f.Masked(); strings.Contains(m, "hunter2") || !strings.Contains(m, "7 characters") {
		t.Errorf("Masked() = %q, want dots and the length", m)
	}

	// With no note, there is nothing to say but the shape of the value.
	bare := Finding{Value: "hunter2", Hidden: true}
	if got := bare.SummaryMasked(); got != bare.Masked() {
		t.Errorf("a hidden finding with no notes reads as %q, want %q", got, bare.Masked())
	}
	if (Finding{Hidden: true}).Masked() != "" {
		t.Error("a finding with no value has nothing to mask")
	}
}

// TestSummaryKeepsRunesWhole covers the corruption the shared elide
// package was introduced for: collapse used to cut Value and Notes by byte
// count, so any non-ASCII finding — the normal case, since a finding's
// value comes straight out of captured traffic — reached the Findings list
// as invalid UTF-8.
func TestSummaryKeepsRunesWhole(t *testing.T) {
	f := Finding{
		Value: strings.Repeat("日", 200), // 3 bytes per rune, over collapse's 160
		Notes: strings.Repeat("é", 200),
	}
	for _, s := range []string{f.Summary(), f.SummaryMasked(), collapse(f.Notes)} {
		if !utf8.ValidString(s) {
			t.Errorf("not valid UTF-8: % x", s)
		}
		if strings.ContainsRune(s, '�') {
			t.Errorf("contains U+FFFD: %q", s)
		}
	}
}

// A new finding is unreviewed (no score) and counts as unviewed. Scoring it
// below the floor drops it from the badge and the default list but not from
// the record; an unreviewed finding is never hidden by the floor. The score
// survives a merge and a reopen, can be cleared back to unreviewed, and is
// clamped to range.
func TestConfidenceTriage(t *testing.T) {
	s, a := open(t)

	f, _, err := s.Record(Draft{
		Value: "X-Powered-By: PHP/8.1", Types: []string{"sensitive-data"},
		Notes: "a stack banner", Source: SourcePlugin, Author: "sensitive-info",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.Confidence != nil || s.Unviewed() != 1 {
		t.Fatalf("new finding: confidence=%v unviewed=%d", f.Confidence, s.Unviewed())
	}

	// The user (or an agent) scores it down as likely noise.
	if err := s.SetConfidence(f.ID, ptrInt(15)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Confidence == nil || *got.Confidence != 15 {
		t.Fatalf("SetConfidence didn't take: %v", got.Confidence)
	}
	if s.Unviewed() != 0 {
		t.Errorf("a low-confidence finding still counts as unviewed: %d", s.Unviewed())
	}

	// It's gone from a list filtered at the floor but still there unfiltered.
	if n := len(s.Select(Query{MinConfidence: ConfidenceFloor})); n != 0 {
		t.Errorf("a low-confidence finding showed in the filtered list: %d", n)
	}
	if n := len(s.Select(Query{})); n != 1 {
		t.Errorf("a low-confidence finding was dropped from the full list: %d", n)
	}

	// An unreviewed finding has no score, so the floor never hides it, and it
	// can be isolated with UnreviewedOnly.
	g, _, err := s.Record(Draft{Value: "Server: nginx", Notes: "a banner", Source: SourcePlugin, Author: "sensitive-info"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.Select(Query{MinConfidence: ConfidenceFloor})); n != 1 {
		t.Errorf("an unreviewed finding was hidden by the floor: %d", n)
	}
	unrev := s.Select(Query{UnreviewedOnly: true})
	if len(unrev) != 1 || unrev[0].ID != g.ID {
		t.Errorf("UnreviewedOnly didn't isolate the unscored finding: %+v", unrev)
	}

	// A fresh sighting is news but doesn't rescore the first one, so it stays
	// out of the badge while its score is below the floor.
	if _, _, err := s.Record(Draft{Value: "X-Powered-By: PHP/8.1", Notes: "again", Source: SourcePlugin, Author: "sensitive-info"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Confidence == nil || *got.Confidence != 15 {
		t.Errorf("a re-sighting changed the score: %v", got.Confidence)
	}

	// A draft carrying an explicit score rescores on merge.
	if _, _, err := s.Record(Draft{Value: "X-Powered-By: PHP/8.1", Notes: "real after all", Source: SourceAgent, Author: "agent", Confidence: ptrInt(90)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Confidence == nil || *got.Confidence != 90 {
		t.Errorf("an explicit merge score didn't take: %v", got.Confidence)
	}

	// Clamping: an out-of-range score is held to 0–100.
	if err := s.SetConfidence(f.ID, ptrInt(250)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Confidence == nil || *got.Confidence != ConfidenceMax {
		t.Errorf("confidence wasn't clamped: %v", got.Confidence)
	}

	// Clearing the score marks it unreviewed again.
	if err := s.SetConfidence(f.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Confidence != nil {
		t.Errorf("clearing the score didn't unreview it: %v", got.Confidence)
	}

	// The score survives reopening the engagement.
	if err := s.SetConfidence(f.ID, ptrInt(15)); err != nil {
		t.Fatal(err)
	}
	reopened := NewStore()
	if err := reopened.SetTypes(types); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Use(a.DB()); err != nil {
		t.Fatal(err)
	}
	if again, _ := reopened.Get(f.ID); again.Confidence == nil || *again.Confidence != 15 {
		t.Errorf("the confidence score was lost on reopen: %v", again.Confidence)
	}
}

func ptrInt(n int) *int { return &n }

// Concern is a second nullable 0–100 score. A finding is marked read
// automatically only once both its confidence and its concern are assessed
// and both below the auto-read line, so low-value findings drop out of the
// badge without hiding a high-concern one.
func TestConcernAndAutoRead(t *testing.T) {
	s, _ := open(t)

	f, _, err := s.Record(Draft{
		Value: "Server: nginx/1.18", Notes: "a banner", Source: SourcePlugin, Author: "sensitive-info",
	})
	if err != nil {
		t.Fatal(err)
	}
	// New: both scores unset, and (raised) it is unviewed.
	if f.Concern != nil || f.Confidence != nil || s.Unviewed() != 1 {
		t.Fatalf("new finding: confidence=%v concern=%v unviewed=%d", f.Confidence, f.Concern, s.Unviewed())
	}

	// Scoring confidence low alone does not auto-read (concern still unknown).
	if err := s.SetConfidence(f.ID, ptrInt(30)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Viewed {
		t.Error("a finding was auto-read on low confidence alone")
	}

	// A high concern keeps it in view even though confidence is low.
	if err := s.SetConcern(f.ID, ptrInt(80)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Viewed || got.Concern == nil || *got.Concern != 80 {
		t.Errorf("high concern didn't keep it unread: viewed=%v concern=%v", got.Viewed, got.Concern)
	}

	// Dropping the concern below the line too (both low) auto-reads it.
	if err := s.SetConcern(f.ID, ptrInt(20)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); !got.Viewed {
		t.Error("a both-low finding wasn't auto-read")
	}
	if s.Unviewed() != 0 {
		t.Errorf("a both-low finding still counts as unviewed: %d", s.Unviewed())
	}

	// Clamping, and clearing concern back to unassessed.
	if err := s.SetConcern(f.ID, ptrInt(250)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Concern == nil || *got.Concern != ConcernMax {
		t.Errorf("concern wasn't clamped: %v", got.Concern)
	}
	if err := s.SetConcern(f.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(f.ID); got.Concern != nil {
		t.Errorf("clearing concern didn't unassess it: %v", got.Concern)
	}

	// A draft recorded already-scored both-low arrives read, so it never nags.
	g, _, err := s.Record(Draft{
		Value: "X-Powered-By: PHP", Notes: "stack banner", Source: SourceAgent, Author: "agent",
		Confidence: ptrInt(20), Concern: ptrInt(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !g.Viewed {
		t.Error("a both-low draft wasn't auto-read on record")
	}
}
