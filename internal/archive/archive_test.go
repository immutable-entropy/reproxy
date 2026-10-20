package archive

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"reproxy/internal/capture"
)

// open returns an archive in a temporary directory, closed by the test.
func open(t *testing.T) *Archive {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// sample is a complete flow with every attribute set, including the
// originals an edit leaves behind.
func sample(id uint64) capture.Flow {
	orig := capture.Message{Raw: []byte("GET /a HTTP/1.1\r\n\r\n"), Size: 19}
	origResp := capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\nbefore"), Size: 25}
	return capture.Flow{
		ID:               id,
		State:            capture.StateComplete,
		ClientAddr:       "127.0.0.1:51000",
		Target:           "app.example.test:443",
		Method:           "POST",
		Host:             "app.example.test",
		Path:             "/orders/12?full=1",
		Secure:           true,
		TargetID:         "t-1",
		Source:           capture.SourceAgent,
		StatusCode:       201,
		StatusText:       "201 Created",
		Request:          capture.Message{Raw: []byte("POST /orders/12 HTTP/1.1\r\n\r\nbody"), Size: 2048, Truncated: true},
		Response:         capture.Message{Raw: []byte("HTTP/1.1 201 Created\r\n\r\nok"), Size: 26},
		OriginalRequest:  &orig,
		OriginalResponse: &origResp,
		Started:          time.Unix(1760000000, 123456789),
		Duration:         42 * time.Millisecond,
		Error:            "",
	}
}

// Every attribute a flow carries in memory has to survive the round trip:
// that is the whole point of the archive.
func TestFlowRoundTripsWithEveryAttribute(t *testing.T) {
	a := open(t)
	want := sample(1)
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: want})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	// A flow's row is the archive's own key: the ID it was recorded under
	// belongs to the session's store, not to the file.
	flows, err := a.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	got, ok, err := a.Flow(flows[0].ID)
	if err != nil || !ok {
		t.Fatalf("Flow = %v, %v", ok, err)
	}
	want.ID = got.ID
	if got.Started.UnixNano() != want.Started.UnixNano() {
		t.Errorf("Started = %v, want %v", got.Started, want.Started)
	}
	// Compare the rest field by field; time.Time doesn't survive ==.
	got.Started, want.Started = time.Time{}, time.Time{}
	if fmt.Sprintf("%+v", *got.OriginalRequest) != fmt.Sprintf("%+v", *want.OriginalRequest) {
		t.Errorf("OriginalRequest = %+v", *got.OriginalRequest)
	}
	if fmt.Sprintf("%+v", *got.OriginalResponse) != fmt.Sprintf("%+v", *want.OriginalResponse) {
		t.Errorf("OriginalResponse = %+v", *got.OriginalResponse)
	}
	got.OriginalRequest, got.OriginalResponse = nil, nil
	want.OriginalRequest, want.OriginalResponse = nil, nil
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// A flow recorded several times before the batch is written costs one row,
// and the row holds its final state.
func TestRepeatedRecordsCoalesceIntoOneRow(t *testing.T) {
	a := open(t)
	f := sample(7)
	f.State, f.StatusCode, f.StatusText, f.Response = capture.StatePending, 0, "", capture.Message{}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	f.State = capture.StateHeldResponse
	a.Record(capture.Event{Kind: capture.FlowUpdated, Flow: f})
	final := sample(7)
	a.Record(capture.Event{Kind: capture.FlowUpdated, Flow: final})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	n, err := a.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
	flows, err := a.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := flows[0]
	if got.State != capture.StateComplete || got.StatusCode != 201 {
		t.Errorf("stored state = %v, status = %d", got.State, got.StatusCode)
	}
	if string(got.Response.Raw) != string(final.Response.Raw) {
		t.Errorf("Response = %q", got.Response.Raw)
	}
}

// An update that arrives in a later batch finds the row written by the
// earlier one rather than inserting a second.
func TestUpdateInALaterBatchKeepsOneRow(t *testing.T) {
	a := open(t)
	f := sample(3)
	f.State, f.Response = capture.StatePending, capture.Message{}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowUpdated, Flow: sample(3)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	if n, _ := a.Count(); n != 1 {
		t.Fatalf("Count = %d, want 1", n)
	}
	flows, err := a.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := flows[0]
	if got.State != capture.StateComplete || len(got.Response.Raw) == 0 {
		t.Errorf("the second batch didn't update the row: %+v", got.State)
	}
}

func TestRecentReturnsNewestInArrivalOrder(t *testing.T) {
	a := open(t)
	for i := 1; i <= 10; i++ {
		f := sample(uint64(i))
		f.Path = fmt.Sprintf("/%d", i)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	flows, err := a.Recent(LoadOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 3 {
		t.Fatalf("got %d flows, want 3", len(flows))
	}
	for i, want := range []string{"/8", "/9", "/10"} {
		if flows[i].Path != want {
			t.Errorf("flows[%d].Path = %q, want %q", i, flows[i].Path, want)
		}
	}
	// The IDs are the rows, so an edit to a loaded flow finds its row again.
	if flows[2].ID != 10 {
		t.Errorf("newest ID = %d, want its row", flows[2].ID)
	}
	if len(flows[0].Request.Raw) == 0 {
		t.Error("bodies were not loaded")
	}

	all, err := a.Recent(LoadOptions{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Errorf("unlimited load returned %d flows", len(all))
	}
}

// The byte budget stops bodies being loaded, but every flow still comes back
// with its metadata and its real sizes.
func TestRecentStopsLoadingBodiesAtTheBudget(t *testing.T) {
	a := open(t)
	for i := 1; i <= 5; i++ {
		f := sample(uint64(i))
		f.Request.Raw = make([]byte, 1000)
		f.Response.Raw = make([]byte, 1000)
		a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	// Enough for the newest flow's four messages and no more.
	flows, err := a.Recent(LoadOptions{MaxBytes: 2100})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 5 {
		t.Fatalf("got %d flows, want all 5", len(flows))
	}
	newest := flows[len(flows)-1]
	if len(newest.Request.Raw) != 1000 {
		t.Errorf("the newest flow's body was skipped (%d bytes)", len(newest.Request.Raw))
	}
	oldest := flows[0]
	if len(oldest.Request.Raw) != 0 {
		t.Error("the budget didn't stop the oldest flow's body being loaded")
	}
	if oldest.Request.Size != 2048 || oldest.Method != "POST" || !oldest.Request.Truncated {
		t.Errorf("a flow past the budget lost its metadata: %+v", oldest)
	}
	// Its body is still one call away.
	full, ok, err := a.Flow(oldest.ID)
	if err != nil || !ok {
		t.Fatalf("Flow = %v, %v", ok, err)
	}
	if len(full.Request.Raw) != 1000 {
		t.Errorf("fetching the skipped body gave %d bytes", len(full.Request.Raw))
	}
}

// Clearing the store empties the archive, and flows recorded afterwards are
// new rows rather than updates to the deleted ones.
func TestClearEmptiesTheArchive(t *testing.T) {
	a := open(t)
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(1)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowsCleared})
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(2)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	n, err := a.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Count = %d, want only the flow recorded after the clear", n)
	}
	flows, err := a.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || len(flows[0].Request.Raw) == 0 {
		t.Errorf("after a clear: %+v", flows)
	}
}

// Flows loaded from the archive and put back into a store are recognised as
// the rows they came from: editing one updates it instead of duplicating it.
func TestResetAdoptsLoadedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		first.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(uint64(i))})
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	loaded, err := second.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	store := capture.NewStore()
	defer store.Subscribe(second.Record)()
	store.Reset(loaded)
	store.Update(loaded[1].ID, func(f *capture.Flow) { f.Error = "edited" })
	if err := second.Flush(); err != nil {
		t.Fatal(err)
	}

	if n, _ := second.Count(); n != 3 {
		t.Fatalf("Count = %d, want 3: the edit added a row", n)
	}
	got, _, err := second.Flow(loaded[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "edited" { //
		t.Errorf("the edit didn't reach the stored flow: %+v", got.Error)
	}

	// A flow captured after the reset gets a row of its own.
	store.Add(sample(0))
	if err := second.Flush(); err != nil {
		t.Fatal(err)
	}
	if n, _ := second.Count(); n != 4 {
		t.Errorf("Count = %d, want 4 after a new capture", n)
	}
}

// A store wired to an archive records everything that happens to it, through
// the proxy's own path: Add, then Complete.
func TestStoreSubscriptionRecordsTheLifecycle(t *testing.T) {
	a := open(t)
	store := capture.NewStore()
	defer store.Subscribe(a.Record)()

	id := store.Add(capture.Flow{
		Method: "GET", Host: "example.test", Path: "/", TargetID: "t-9",
		Request: capture.Message{Raw: []byte("GET / HTTP/1.1\r\n\r\n"), Size: 18},
		Started: time.Now(),
	})
	store.Complete(id, capture.Message{Raw: []byte("HTTP/1.1 200 OK\r\n\r\n"), Size: 19},
		200, "200 OK", time.Now())
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}

	flows, err := a.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	f := flows[0]
	if f.State != capture.StateComplete || f.StatusCode != 200 || f.TargetID != "t-9" {
		t.Errorf("stored %+v", f)
	}
	if string(f.Response.Raw) != "HTTP/1.1 200 OK\r\n\r\n" {
		t.Errorf("Response.Raw = %q", f.Response.Raw)
	}
	if f.Duration <= 0 {
		t.Errorf("Duration = %v", f.Duration)
	}
}

// Reopening the file gives back what was written, which is what makes an
// engagement's traffic survive the session.
func TestArchiveSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(1)})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || flows[0].Path != "/orders/12?full=1" {
		t.Fatalf("reopened %+v", flows)
	}
	if string(flows[0].Request.Raw) != "POST /orders/12 HTTP/1.1\r\n\r\nbody" {
		t.Errorf("Request.Raw = %q", flows[0].Request.Raw)
	}
}

func TestOpenRejectsANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, Version+1)); err != nil {
		t.Fatal(err)
	}
	a.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("an archive from a newer build was opened")
	}
}

// An archive written before flows had a source opens, its rows read as
// proxy traffic, and new rows keep their source.
func TestOpenUpgradesAVersion1Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sample(1)
	old.Source = capture.SourceProxy
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: old})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	// Take the file back to schema 1: no source column and no findings.
	for _, q := range append(v4Tables(),
		`ALTER TABLE flows DROP COLUMN source`,
		`DROP TABLE finding_flows`, `DROP TABLE finding_types`, `DROP TABLE findings`,
		`DROP TABLE files`, `PRAGMA user_version = 1`,
	) {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(2)})
	if err := again.Flush(); err != nil {
		t.Fatal(err)
	}
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 2 || flows[0].Source != capture.SourceProxy || flows[1].Source != capture.SourceAgent {
		t.Fatalf("sources after upgrade: %+v", flows)
	}
}

// An archive written before findings were kept in it opens, its traffic
// intact, with the findings tables created.
func TestOpenUpgradesAVersion2Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(1)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	// Take the file back to schema 2: no findings tables.
	for _, q := range append(v4Tables(),
		`DROP TABLE finding_flows`, `DROP TABLE finding_types`, `DROP TABLE findings`,
		`DROP TABLE files`, `PRAGMA user_version = 2`,
	) {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.db.Exec(`
		INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, created, updated)
		VALUES (1, 'x', 'x', '', '[]', 'user', '', 1, 0, 0)`); err != nil {
		t.Fatalf("findings table after upgrade: %v", err)
	}
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("flows after upgrade: %d, want 1", len(flows))
	}
}

// v7Tables puts schema 6's endpoint tables back in place of the
// entities, to take a file back to schema 6.
func v7Tables() []string {
	return []string{
		`DROP TABLE crawl_urls`, `DROP TABLE crawl_meta`,
		`DROP TABLE entity_links`, `DROP TABLE entity_flows`, `DROP TABLE entities`, `DROP TABLE entity_meta`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`,
		`ALTER TABLE findings DROP COLUMN concern`,
		`ALTER TABLE findings DROP COLUMN confidence`,
		`ALTER TABLE findings DROP COLUMN hidden`,
		endpointsSchemaV4,
	}
}

// v4Tables undoes what schema 4 added, to take a file back to schema 3.
func v4Tables() []string {
	return []string{
		`DROP TABLE crawl_urls`, `DROP TABLE crawl_meta`,
		`ALTER TABLE flows DROP COLUMN origin`,
		`ALTER TABLE flows DROP COLUMN protocol`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`,
		`ALTER TABLE findings DROP COLUMN concern`,
		`ALTER TABLE findings DROP COLUMN confidence`,
		`ALTER TABLE findings DROP COLUMN hidden`,
		`DROP TABLE entity_links`, `DROP TABLE entity_flows`, `DROP TABLE entities`,
		`DROP TABLE entity_meta`, `DROP TABLE plugin_kv`, `DROP TABLE websocket_messages`,
	}
}

// An archive written before the endpoint map was kept in it opens with its
// traffic intact, the new tables created, and the backfill marker set so the
// traffic already there is indexed once.
func TestOpenUpgradesAVersion3Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(1)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(v4Tables(), `DROP TABLE files`, `PRAGMA user_version = 3`) {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var backfill string
	if err := again.db.QueryRow(`SELECT value FROM entity_meta WHERE key = 'backfill'`).Scan(&backfill); err != nil || backfill != "1" {
		t.Fatalf("backfill marker after upgrade: %q, %v", backfill, err)
	}
	f := sample(2)
	f.Source, f.Origin = capture.SourcePlugin, "mapper"
	again.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	if err := again.Flush(); err != nil {
		t.Fatal(err)
	}
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 2 || flows[0].Origin != "" || flows[1].Origin != "mapper" {
		t.Fatalf("flows after upgrade: %+v", flows)
	}
}

// A version 5 archive gains the protocol column; its rows are all HTTP.
func TestOpenUpgradesAVersion5Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: sample(1)})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(v7Tables(), `ALTER TABLE flows DROP COLUMN protocol`, `DROP TABLE files`, `PRAGMA user_version = 5`) {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.Record(capture.Event{Kind: capture.FlowAdded, Flow: dnsSample(2)})
	if err := again.Flush(); err != nil {
		t.Fatal(err)
	}
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 2 || flows[0].IsDNS() || !flows[1].IsDNS() {
		t.Fatalf("flows after upgrade: %+v", flows)
	}
}

// A version 6 archive's endpoints become URL entities, their sightings
// and links kept, with an empty key/value store each.
func TestOpenUpgradesAVersion6Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(v7Tables(),
		`INSERT INTO endpoints (key, scheme, host, path, target_id, visited, origin, summary, hits, first_seen, last_seen)
			VALUES ('https://app.test/users/{id}', 'https', 'app.test', '/users/{id}', 't1', 1, 'traffic', '{"examples":["/users/7"]}', 3, 1, 2)`,
		`INSERT INTO endpoint_flows (endpoint_seq, flow_seq, method, status, url, seen) VALUES (1, 9, 'GET', 200, 'https://app.test/users/7', 2)`,
		`INSERT INTO endpoint_links (endpoint_seq, source, from_url, from_flow_seq, detail, added) VALUES (1, 'link', 'https://app.test/', 8, 'html:href', 1)`,
		`DROP TABLE files`, `PRAGMA user_version = 6`,
	) {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var typ, parent, attrs, kv string
	if err := again.db.QueryRow(`SELECT type, parent, attrs, kv FROM entities WHERE key = 'https://app.test/users/{id}'`).
		Scan(&typ, &parent, &attrs, &kv); err != nil {
		t.Fatal(err)
	}
	if typ != "url" || parent != "" || attrs != `{"examples":["/users/7"]}` || kv != "[]" {
		t.Errorf("upgraded row: type %q, parent %q, attrs %s, kv %s", typ, parent, attrs, kv)
	}
	var sightings, links int
	again.db.QueryRow(`SELECT count(*) FROM entity_flows WHERE entity_seq = 1`).Scan(&sightings)
	again.db.QueryRow(`SELECT count(*) FROM entity_links WHERE entity_seq = 1`).Scan(&links)
	if sightings != 1 || links != 1 {
		t.Errorf("sightings %d, links %d after upgrade; want 1 and 1", sightings, links)
	}
	// The cascade still follows the renamed table.
	if _, err := again.db.Exec(`DELETE FROM entities`); err != nil {
		t.Fatal(err)
	}
	again.db.QueryRow(`SELECT count(*) FROM entity_flows`).Scan(&sightings)
	if sightings != 0 {
		t.Errorf("%d sightings outlived their entity", sightings)
	}
}

// A version 11 archive's false-positive flag becomes a nullable confidence:
// a flagged false positive takes a low score, everything else is left
// unreviewed (NULL), and the false_positive column is gone.
func TestOpenUpgradesAVersion11Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Take findings back to schema 11: a false_positive flag, no confidence.
	for _, q := range []string{
		`ALTER TABLE findings DROP COLUMN concern`,
		`ALTER TABLE findings DROP COLUMN confidence`,
		`ALTER TABLE findings ADD COLUMN false_positive INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, false_positive, created, updated)
			VALUES (1, 'a', 'a', '', '[]', 'user', '', 1, 0, 0, 0, 0)`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, false_positive, created, updated)
			VALUES (2, 'b', 'b', '', '[]', 'plugin', '', 0, 0, 1, 0, 0)`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`, `DROP TABLE files`, `PRAGMA user_version = 11`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var c1, c2 sql.NullInt64
	again.db.QueryRow(`SELECT confidence FROM findings WHERE seq = 1`).Scan(&c1)
	again.db.QueryRow(`SELECT confidence FROM findings WHERE seq = 2`).Scan(&c2)
	if c1.Valid {
		t.Errorf("a non-false-positive should be unreviewed (NULL), got %d", c1.Int64)
	}
	if !c2.Valid || c2.Int64 != 20 {
		t.Errorf("a false positive should map to a low score of 20, got %v", c2)
	}
	if _, err := again.db.Exec(`SELECT false_positive FROM findings`); err == nil {
		t.Error("the false_positive column survived the upgrade")
	}
}

// A version 12 archive's NOT NULL confidence becomes nullable: the default 50
// is taken as unreviewed (NULL), a real score is kept, and a NULL can then be
// stored.
func TestOpenUpgradesAVersion12Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Take findings back to schema 12: confidence NOT NULL DEFAULT 50.
	for _, q := range []string{
		`ALTER TABLE findings DROP COLUMN concern`,
		`ALTER TABLE findings DROP COLUMN confidence`,
		`ALTER TABLE findings ADD COLUMN confidence INTEGER NOT NULL DEFAULT 50`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, confidence, created, updated)
			VALUES (1, 'a', 'a', '', '[]', 'user', '', 1, 0, 50, 0, 0)`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, confidence, created, updated)
			VALUES (2, 'b', 'b', '', '[]', 'plugin', '', 0, 0, 20, 0, 0)`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`, `DROP TABLE files`, `PRAGMA user_version = 12`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var c1, c2 sql.NullInt64
	again.db.QueryRow(`SELECT confidence FROM findings WHERE seq = 1`).Scan(&c1)
	again.db.QueryRow(`SELECT confidence FROM findings WHERE seq = 2`).Scan(&c2)
	if c1.Valid {
		t.Errorf("the default-50 finding should be unreviewed (NULL), got %d", c1.Int64)
	}
	if !c2.Valid || c2.Int64 != 20 {
		t.Errorf("the scored finding lost its score: %v", c2)
	}
	// The column is nullable now: an unreviewed finding can be stored.
	if _, err := again.db.Exec(`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, confidence, created, updated)
		VALUES (3, 'c', 'c', '', '[]', 'agent', '', 0, 0, NULL, 0, 0)`); err != nil {
		t.Errorf("couldn't store a NULL (unreviewed) confidence after upgrade: %v", err)
	}
}

// A version 13 archive gains a nullable concern column; findings already there
// have none (NULL), and a concern score can then be stored.
func TestOpenUpgradesAVersion13Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Take findings back to schema 13: confidence but no concern.
	for _, q := range []string{
		`ALTER TABLE findings DROP COLUMN concern`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, confidence, created, updated)
			VALUES (1, 'a', 'a', '', '[]', 'user', '', 1, 0, 80, 0, 0)`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`, `DROP TABLE files`,
		`PRAGMA user_version = 13`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var concern sql.NullInt64
	if err := again.db.QueryRow(`SELECT concern FROM findings WHERE seq = 1`).Scan(&concern); err != nil {
		t.Fatalf("no concern column after upgrade: %v", err)
	}
	if concern.Valid {
		t.Errorf("a finding from before concern should be unassessed (NULL), got %d", concern.Int64)
	}
	if _, err := again.db.Exec(`UPDATE findings SET concern = 90 WHERE seq = 1`); err != nil {
		t.Errorf("couldn't store a concern score after upgrade: %v", err)
	}
}

func TestOpenUpgradesAVersion14Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Take the archive back to schema 14: findings and the Explorer, but no
	// record of the files the work recovered.
	for _, q := range []string{`ALTER TABLE findings DROP COLUMN awaiting_review`, `DROP TABLE files`, `PRAGMA user_version = 14`} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.db.Exec(`
		INSERT INTO files (seq, path, bytes, sha256, media_type, source, created, updated)
		VALUES (1, 'app.test/main/src/api.ts', 12, 'ab', 'text/typescript', 'plugin', 0, 0)`); err != nil {
		t.Fatalf("no files table after upgrade: %v", err)
	}
	var n int
	if err := again.db.QueryRow(`SELECT count(*) FROM files`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("files after the upgrade: %d rows, err %v", n, err)
	}
}

// A version 15 archive's findings have all been announced already, so none
// comes out of the upgrade waiting for the reviewer.
func TestOpenUpgradesAVersion15Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE findings DROP COLUMN awaiting_review`,
		`INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, created, updated)
			VALUES (1, 'a', 'a', '', '[]', 'plugin', '', 0, 0, 0, 0)`,
		`PRAGMA user_version = 15`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var viewed, awaiting int
	if err := again.db.QueryRow(`SELECT viewed, awaiting_review FROM findings WHERE seq = 1`).Scan(&viewed, &awaiting); err != nil {
		t.Fatal(err)
	}
	if viewed != 0 || awaiting != 0 {
		t.Fatalf("after the upgrade: viewed %d, awaiting %d", viewed, awaiting)
	}
}

// dnsSample is a recorded DNS lookup.
func dnsSample(id uint64) capture.Flow {
	return capture.Flow{
		ID: id, State: capture.StateComplete, Protocol: capture.ProtocolDNS,
		Method: "DNS", Host: "app.example.test", Target: "app.example.test", Path: "10.0.0.12",
		StatusText: "NOERROR",
		Request:    capture.Message{Raw: []byte("QUERY app.example.test DNS\r\n\r\n"), Size: 29},
		Response:   capture.Message{Raw: []byte("DNS NOERROR\r\nA: 10.0.0.12\r\n\r\n"), Size: 31},
		Started:    time.Unix(1760000000, 0),
	}
}

// WebSocket messages are written and come back with their flow.
func TestWebSocketMessagesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := sample(1)
	a.Record(capture.Event{Kind: capture.FlowAdded, Flow: f})
	f.Messages = []capture.WSMessage{{Seq: 1, Dir: capture.Sent, Type: capture.WSText, Data: []byte("hi"), At: time.Unix(1, 0)}}
	a.Record(capture.Event{Kind: capture.WSMessageAdded, Flow: f, Message: f.Messages[0]})
	m2 := capture.WSMessage{Seq: 2, Dir: capture.Received, Type: capture.WSBinary, Data: []byte{9, 8}, Edited: true, Original: []byte{1}, At: time.Unix(2, 0)}
	a.Record(capture.Event{Kind: capture.WSMessageAdded, Flow: f, Message: m2})
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	flows, err := again.Recent(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 || len(flows[0].Messages) != 2 {
		t.Fatalf("messages = %+v", flows)
	}
	got := flows[0].Messages
	if got[0].Dir != capture.Sent || string(got[0].Data) != "hi" || got[1].Type != capture.WSBinary || !got[1].Edited {
		t.Fatalf("round trip = %+v", got)
	}
	// Clearing takes the WebSocket messages with the flows (cascade).
	if err := again.Clear(); err != nil {
		t.Fatal(err)
	}
	var n int
	again.db.QueryRow(`SELECT count(*) FROM websocket_messages`).Scan(&n)
	if n != 0 {
		t.Fatalf("websocket rows after clear = %d", n)
	}
}

// An archive from before Autocrawl opens with its crawl tables created
// empty.
func TestOpenUpgradesAVersion9Archive(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE crawl_urls`, `DROP TABLE crawl_meta`,
		`DROP TABLE files`,
		`ALTER TABLE findings DROP COLUMN awaiting_review`,
		`ALTER TABLE findings DROP COLUMN concern`,
		`ALTER TABLE findings DROP COLUMN confidence`, `PRAGMA user_version = 9`} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	a.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var n int
	if err := again.db.QueryRow(`SELECT count(*) FROM crawl_urls`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("crawl_urls after the upgrade: %d rows, err %v", n, err)
	}
	if _, err := again.db.Exec(`INSERT INTO crawl_meta (key, value) VALUES ('threads', '1')`); err != nil {
		t.Fatal(err)
	}
}
