// Package archive is the on-disk record of captured traffic.
//
// Every flow a capture.Store holds is written to a SQLite database inside the
// open engagement, so the traffic collected in a session survives the session
// and can be reopened, queried and exported (see package engagement). SQLite
// is the store because an archive has to be two things at once: fast to write
// while the proxy is running, and queryable afterwards — including by tools
// that aren't reproxy.
//
// Nothing here runs on the proxy's hot path. The archive subscribes to the
// store, and Record only drops the flow into a queue; a single background
// goroutine coalesces the queue (a flow that is added, held and completed in
// one window is written once) and applies each batch in one transaction.
//
// It is core and has no Qt dependency: the UI never touches it, the app wires
// it to the store.
package archive

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver: no cgo, so core still cross-compiles

	"reproxy/internal/capture"
)

// FileName is the archive inside an engagement directory.
const FileName = "capture.db"

// Version is the schema this build writes. An archive from a newer build is
// refused rather than half-read.
const Version = 15

// wsRow is a WebSocket message queued for writing, with its flow.
type wsRow struct {
	flowID uint64
	msg    capture.WSMessage
}

// messageKind identifies which of a flow's four messages a row holds.
type messageKind int

const (
	msgRequest messageKind = iota
	msgResponse
	msgOriginalRequest
	msgOriginalResponse
)

// Archive is one capture database.
type Archive struct {
	db   *sql.DB
	path string

	// mu guards the write queue and the id mapping.
	mu   sync.Mutex
	cond *sync.Cond
	// pending holds the latest state of each flow waiting to be written,
	// keyed by its store ID, with order remembering arrival.
	pending map[uint64]capture.Flow
	order   []uint64
	// wsPending holds WebSocket messages waiting to be written, in arrival
	// order. They are queued apart from flows: a message doesn't change its
	// flow's row, and re-queuing a flow must not re-write every message.
	wsPending []wsRow
	// wipe records that the store was cleared: the batch empties the
	// database before applying whatever was recorded after the clear.
	wipe bool
	// seq maps a flow's in-memory store ID to its row. A flow already in the
	// archive is updated in place; without this a second process, or a flow
	// loaded from an earlier session, would be inserted again.
	seq map[uint64]int64
	// flushing is set by Flush so the writer drains at once rather than
	// waiting for the next tick; writing marks a batch that has left the
	// queue but is not committed yet, which Flush must also wait for.
	flushing bool
	writing  bool
	lastErr  error

	wake chan struct{}
	quit chan struct{}
	done chan struct{}
}

// Tuning for the background writer. The interval is what bounds how much
// recent traffic a crash could cost; the threshold keeps a burst from
// sitting in memory until the tick.
const (
	flushInterval  = 250 * time.Millisecond
	flushThreshold = 256
)

// Open opens (creating it if needed) the archive at path and starts its
// writer. Close stops the writer and flushes what is queued.
func Open(path string) (*Archive, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// WAL lets the readers (the request list, a query, another reproxy
	// process) work while the writer is in a transaction.
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	a := &Archive{
		db:      db,
		path:    path,
		pending: make(map[uint64]capture.Flow),
		seq:     make(map[uint64]int64),
		wake:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	a.cond = sync.NewCond(&a.mu)
	go a.run()
	return a, nil
}

// dsn builds the connection string. The pragmas are per-connection, so they
// go here rather than in migrate: WAL for concurrent readers, NORMAL sync
// (a crash can cost the last transaction, never the file), a busy timeout so
// a second process waits rather than failing, and foreign keys so deleting a
// flow takes its messages with it.
func dsn(path string) string {
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(5000)",
		"foreign_keys(ON)",
	} {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// Path returns the database file this archive writes to.
func (a *Archive) Path() string { return a.path }

// DB returns the open database, so another package can keep its own tables
// in the engagement's capture.db — package findings does. The handle is
// closed by Close, so a holder must be pointed at the next archive before
// this one is closed (see app.followEngagement).
func (a *Archive) DB() *sql.DB { return a.db }

// Close stops the writer, flushing whatever is queued, and closes the
// database. It reports the last write error, if any.
func (a *Archive) Close() error {
	select {
	case <-a.quit: // already closed
	default:
		close(a.quit)
	}
	<-a.done

	a.mu.Lock()
	err := a.lastErr
	a.mu.Unlock()
	if cerr := a.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// schema is applied to a new database. Metadata and message bodies are kept
// apart on purpose: the request list only reads the flows table, so scanning
// it never has to walk past megabytes of bodies.
const schema = `
CREATE TABLE flows (
	seq         INTEGER PRIMARY KEY,
	state       INTEGER NOT NULL,
	client_addr TEXT    NOT NULL,
	dest        TEXT    NOT NULL,
	method      TEXT    NOT NULL,
	host        TEXT    NOT NULL,
	path        TEXT    NOT NULL,
	secure      INTEGER NOT NULL,
	target_id   TEXT    NOT NULL,
	status_code INTEGER NOT NULL,
	status_text TEXT    NOT NULL,
	started     INTEGER NOT NULL,
	duration    INTEGER NOT NULL,
	error       TEXT    NOT NULL,
	source      TEXT    NOT NULL DEFAULT '',
	origin      TEXT    NOT NULL DEFAULT '',
	protocol    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX flows_started ON flows (started);
CREATE INDEX flows_host    ON flows (host);
CREATE INDEX flows_target  ON flows (target_id);

CREATE TABLE messages (
	seq       INTEGER NOT NULL REFERENCES flows (seq) ON DELETE CASCADE,
	kind      INTEGER NOT NULL,
	size      INTEGER NOT NULL,
	truncated INTEGER NOT NULL,
	raw       BLOB    NOT NULL,
	PRIMARY KEY (seq, kind)
);
` + findingsSchema + entitiesSchema + pluginSchema + wsSchema + crawlSchema + filesSchema

// findingsSchemaV3 is what schema 3 added for the findings, before a
// finding could be hidden. The upgrade from 2 creates them as they were and
// the later steps bring them up to date, so one chain of steps serves every
// archive whatever version it starts from.
const findingsSchemaV3 = `
CREATE TABLE findings (
	seq       INTEGER PRIMARY KEY,
	value     TEXT    NOT NULL,
	value_key TEXT    NOT NULL,
	notes     TEXT    NOT NULL,
	fields    TEXT    NOT NULL,
	source    TEXT    NOT NULL,
	author    TEXT    NOT NULL,
	viewed    INTEGER NOT NULL,
	created   INTEGER NOT NULL,
	updated   INTEGER NOT NULL
);
CREATE INDEX findings_value_key ON findings (value_key);

CREATE TABLE finding_types (
	seq     INTEGER NOT NULL REFERENCES findings (seq) ON DELETE CASCADE,
	type_id TEXT    NOT NULL,
	pos     INTEGER NOT NULL,
	PRIMARY KEY (seq, type_id)
);
CREATE INDEX finding_types_type ON finding_types (type_id);

CREATE TABLE finding_flows (
	seq      INTEGER NOT NULL REFERENCES findings (seq) ON DELETE CASCADE,
	flow_seq INTEGER NOT NULL,
	method   TEXT    NOT NULL,
	url      TEXT    NOT NULL,
	location TEXT    NOT NULL,
	detail   TEXT    NOT NULL,
	added    INTEGER NOT NULL,
	pos      INTEGER NOT NULL,
	PRIMARY KEY (seq, flow_seq, location, detail)
);
CREATE INDEX finding_flows_flow ON finding_flows (flow_seq);
`

// findingsSchema holds what the engagement's findings are kept in (see
// package findings, which writes them). It lives here because one package
// has to own this database's schema and its version; nothing else in the
// archive reads or writes these tables.
//
// finding_flows.flow_seq is deliberately not a foreign key into flows: a
// finding outlives the traffic it came from, so Clear Requests must not take
// the findings with it. Each sighting carries its own method/URL snapshot
// and the flow reference is allowed to stop resolving.
const findingsSchema = `
CREATE TABLE findings (
	seq       INTEGER PRIMARY KEY,
	value     TEXT    NOT NULL,
	value_key TEXT    NOT NULL,
	notes     TEXT    NOT NULL,
	fields    TEXT    NOT NULL,
	source    TEXT    NOT NULL,
	author    TEXT    NOT NULL,
	viewed    INTEGER NOT NULL,
	hidden    INTEGER NOT NULL DEFAULT 0,
	confidence INTEGER,
	concern   INTEGER,
	created   INTEGER NOT NULL,
	updated   INTEGER NOT NULL
);
CREATE INDEX findings_value_key ON findings (value_key);

CREATE TABLE finding_types (
	seq     INTEGER NOT NULL REFERENCES findings (seq) ON DELETE CASCADE,
	type_id TEXT    NOT NULL,
	pos     INTEGER NOT NULL,
	PRIMARY KEY (seq, type_id)
);
CREATE INDEX finding_types_type ON finding_types (type_id);

CREATE TABLE finding_flows (
	seq      INTEGER NOT NULL REFERENCES findings (seq) ON DELETE CASCADE,
	flow_seq INTEGER NOT NULL,
	method   TEXT    NOT NULL,
	url      TEXT    NOT NULL,
	location TEXT    NOT NULL,
	detail   TEXT    NOT NULL,
	added    INTEGER NOT NULL,
	pos      INTEGER NOT NULL,
	PRIMARY KEY (seq, flow_seq, location, detail)
);
CREATE INDEX finding_flows_flow ON finding_flows (flow_seq);
`

// entitiesSchema holds the engagement's Entity Explorer (see package
// entities, which writes it): the domains, hosts and URLs requested or
// discovered, each one row keyed by its identity. The Explorer is drawn
// from these rows, never from the traffic, so — like findings — they
// outlive Clear Requests, and entity_flows.flow_seq is deliberately not a
// foreign key: each sighting carries its own method/status/URL snapshot.
//
// key is UNIQUE so that two writers, or a writer and a backfill, can only
// ever merge into one row per entity. type is domain, host or url, and
// parent the key of the entity it sits under. attrs is the attributes of
// its type as JSON (for a URL, its OpenAPI-style description: operations,
// parameters, responses, kinds), and kv its key/value store, a JSON array
// of {key, value}: both are read and rewritten whole, never queried into.
// scheme, host and path say where it is (only host, for a domain).
//
// entity_meta holds markers such as "backfill": set by the upgrade that
// first created these tables, so the traffic already archived is indexed
// once.
const entitiesSchema = `
CREATE TABLE entities (
	seq        INTEGER PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	type       TEXT    NOT NULL,
	parent     TEXT    NOT NULL,
	scheme     TEXT    NOT NULL,
	host       TEXT    NOT NULL,
	path       TEXT    NOT NULL,
	target_id  TEXT    NOT NULL,
	visited    INTEGER NOT NULL,
	origin     TEXT    NOT NULL,
	attrs      TEXT    NOT NULL,
	kv         TEXT    NOT NULL,
	hits       INTEGER NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);
CREATE INDEX entities_target ON entities (target_id);
CREATE INDEX entities_host   ON entities (host);
CREATE INDEX entities_parent ON entities (parent);

CREATE TABLE entity_flows (
	entity_seq INTEGER NOT NULL REFERENCES entities (seq) ON DELETE CASCADE,
	flow_seq   INTEGER NOT NULL,
	method     TEXT    NOT NULL,
	status     INTEGER NOT NULL,
	url        TEXT    NOT NULL,
	seen       INTEGER NOT NULL,
	PRIMARY KEY (entity_seq, flow_seq)
);
CREATE INDEX entity_flows_flow ON entity_flows (flow_seq);

CREATE TABLE entity_links (
	entity_seq    INTEGER NOT NULL REFERENCES entities (seq) ON DELETE CASCADE,
	source        TEXT    NOT NULL,
	from_url      TEXT    NOT NULL,
	from_flow_seq INTEGER NOT NULL,
	detail        TEXT    NOT NULL,
	added         INTEGER NOT NULL,
	PRIMARY KEY (entity_seq, source, from_url)
);

CREATE TABLE entity_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// endpointsSchemaV4 is what schema 4 added for the map, when it held only
// URLs ("endpoints"). It is kept for the upgrade from schema 3, whose
// tables the upgrade from schema 6 then turns into entitiesSchema's.
const endpointsSchemaV4 = `
CREATE TABLE endpoints (
	seq        INTEGER PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	scheme     TEXT    NOT NULL,
	host       TEXT    NOT NULL,
	path       TEXT    NOT NULL,
	target_id  TEXT    NOT NULL,
	visited    INTEGER NOT NULL,
	origin     TEXT    NOT NULL,
	summary    TEXT    NOT NULL,
	hits       INTEGER NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);
CREATE INDEX endpoints_target ON endpoints (target_id);
CREATE INDEX endpoints_host   ON endpoints (host);

CREATE TABLE endpoint_flows (
	endpoint_seq INTEGER NOT NULL REFERENCES endpoints (seq) ON DELETE CASCADE,
	flow_seq     INTEGER NOT NULL,
	method       TEXT    NOT NULL,
	status       INTEGER NOT NULL,
	url          TEXT    NOT NULL,
	seen         INTEGER NOT NULL,
	PRIMARY KEY (endpoint_seq, flow_seq)
);
CREATE INDEX endpoint_flows_flow ON endpoint_flows (flow_seq);

CREATE TABLE endpoint_links (
	endpoint_seq  INTEGER NOT NULL REFERENCES endpoints (seq) ON DELETE CASCADE,
	source        TEXT    NOT NULL,
	from_url      TEXT    NOT NULL,
	from_flow_seq INTEGER NOT NULL,
	detail        TEXT    NOT NULL,
	added         INTEGER NOT NULL,
	PRIMARY KEY (endpoint_seq, source, from_url)
);

CREATE TABLE endpoint_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// endpointsToEntities turns schema 6's endpoint tables into entitiesSchema's.
// Every row there is a URL; the hosts and domains they sit under are put
// in by package entities the first time the archive is opened.
const endpointsToEntities = `
ALTER TABLE endpoints RENAME TO entities;
ALTER TABLE entities RENAME COLUMN summary TO attrs;
ALTER TABLE entities ADD COLUMN type   TEXT NOT NULL DEFAULT 'url';
ALTER TABLE entities ADD COLUMN parent TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN kv     TEXT NOT NULL DEFAULT '[]';
DROP INDEX endpoints_target;
DROP INDEX endpoints_host;
CREATE INDEX entities_target ON entities (target_id);
CREATE INDEX entities_host   ON entities (host);
CREATE INDEX entities_parent ON entities (parent);
ALTER TABLE endpoint_flows RENAME TO entity_flows;
ALTER TABLE entity_flows RENAME COLUMN endpoint_seq TO entity_seq;
DROP INDEX endpoint_flows_flow;
CREATE INDEX entity_flows_flow ON entity_flows (flow_seq);
ALTER TABLE endpoint_links RENAME TO entity_links;
ALTER TABLE entity_links RENAME COLUMN endpoint_seq TO entity_seq;
ALTER TABLE endpoint_meta RENAME TO entity_meta;
`

// pluginSchema is the per-engagement key/value storage plugins get through
// the SDK's storage.engagement (see package plugin).
const pluginSchema = `
CREATE TABLE plugin_kv (
	plugin  TEXT    NOT NULL,
	key     TEXT    NOT NULL,
	value   TEXT    NOT NULL,
	updated INTEGER NOT NULL,
	PRIMARY KEY (plugin, key)
);
`

// wsSchema holds the WebSocket messages exchanged over a flow's connection
// after its 101 upgrade, in order. They cascade with their flow: unlike
// findings and entities, a WebSocket message is part of its flow and is
// gone when Clear Requests takes the flow.
const wsSchema = `
CREATE TABLE websocket_messages (
	flow_seq  INTEGER NOT NULL REFERENCES flows (seq) ON DELETE CASCADE,
	seq       INTEGER NOT NULL,
	dir       INTEGER NOT NULL,
	type      INTEGER NOT NULL,
	data      BLOB    NOT NULL,
	truncated INTEGER NOT NULL,
	edited    INTEGER NOT NULL,
	dropped   INTEGER NOT NULL,
	original  BLOB,
	at        INTEGER NOT NULL,
	PRIMARY KEY (flow_seq, seq)
);
`

// crawlSchema holds Autocrawl's state (see package crawl): every URL it has
// queued, crawled, failed or is keeping until a target covers it, and its
// settings. Like findings and entities it survives Clear Requests, so
// flow_seq is deliberately not a foreign key.
const crawlSchema = `
CREATE TABLE crawl_urls (
	url      TEXT    PRIMARY KEY,
	state    INTEGER NOT NULL,
	depth    INTEGER NOT NULL,
	found_on TEXT    NOT NULL DEFAULT '',
	status   INTEGER NOT NULL DEFAULT 0,
	flow_seq INTEGER NOT NULL DEFAULT 0,
	error    TEXT    NOT NULL DEFAULT '',
	added    INTEGER NOT NULL,
	crawled  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE crawl_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// filesSchema holds the engagement's recovered files (see package files,
// which writes them): the record of each one, never its bytes, which live
// under the engagement's recovered/ directory. Like findings, entities and
// the crawl it survives Clear Requests, so flow_seq is deliberately not a
// foreign key into flows — each file keeps its own method/URL snapshot and
// the flow reference is allowed to stop resolving.
//
// path is UNIQUE: one record per recovered file, which is the dedup rule
// package files relies on.
const filesSchema = `
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
CREATE INDEX files_batch ON files (batch);
CREATE INDEX files_flow ON files (flow_seq);
`

// migrate creates the schema in a new database and checks the version of an
// existing one. user_version holds it, so no table is needed.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	switch {
	case version == Version:
		return nil
	case version > Version:
		return fmt.Errorf("the archive was written by a newer version of reProxy (schema %d)", version)
	case version != 0:
		return upgrade(db, version)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("creating the schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, Version)); err != nil {
		return fmt.Errorf("setting the schema version: %w", err)
	}
	return tx.Commit()
}

// upgrades are the steps from each older schema to the next, keyed by the
// version they start from.
var upgrades = map[int]string{
	// 2: flows record where they came from (capture.Source); older rows all
	// came through the proxy, which is the empty source.
	1: `ALTER TABLE flows ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
	// 3: the engagement keeps its findings here too (see findingsSchema).
	// It is the schema as it was then: the columns added since have their
	// own steps below.
	2: findingsSchemaV3,
	// 4: flows record the plugin that sent them; the engagement keeps its
	// endpoint map and its plugins' storage. The traffic already archived
	// is indexed into the map once, on the first open (see package
	// endpoints).
	3: `ALTER TABLE flows ADD COLUMN origin TEXT NOT NULL DEFAULT '';` +
		endpointsSchemaV4 + pluginSchema +
		`INSERT INTO endpoint_meta (key, value) VALUES ('backfill', '1');`,
	// 5: a flow's WebSocket messages are kept.
	4: wsSchema,
	// 6: flows record their protocol (capture.Protocol), so DNS lookups are
	// told apart from HTTP; older rows are all HTTP, the empty protocol.
	5: `ALTER TABLE flows ADD COLUMN protocol TEXT NOT NULL DEFAULT ''`,
	// 7: the map of endpoints becomes the Entity Explorer: domains and
	// hosts as well as URLs, each with a key/value store.
	6: endpointsToEntities,
	// 8: a finding can be a secret in itself, kept masked on screen until
	// the user asks to see it. Findings already recorded are shown as they
	// always were.
	7: `ALTER TABLE findings ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`,
	// 9: every origin sits under a host for its name, keyed //name so it
	// can't clash with its domain's key (an apex: example.com and
	// //example.com). The address-only hosts schema 7 kept under the bare
	// address take the new key; the entities store moves the origins under
	// them, and puts in the name hosts other origins lack, on first open.
	8: `UPDATE entities SET key = '//' || key WHERE type = 'host' AND scheme = '' AND key NOT LIKE '//%'`,
	// 10: Autocrawl keeps what it has crawled, so a crawl can be stopped
	// and picked up again (see crawlSchema).
	9: crawlSchema,
	// 11: a finding can be flagged as a false positive — noise, triaged
	// away — so the list hides it by default. Findings already recorded are
	// not false positives.
	10: `ALTER TABLE findings ADD COLUMN false_positive INTEGER NOT NULL DEFAULT 0`,
	// 12: the false-positive flag becomes a 0–100 confidence score (50
	// unknown, below false positive, above true positive). Findings already
	// flagged as false positives take a low score; the rest start at 50.
	11: `ALTER TABLE findings ADD COLUMN confidence INTEGER NOT NULL DEFAULT 50;` +
		`UPDATE findings SET confidence = 20 WHERE false_positive = 1;` +
		`ALTER TABLE findings DROP COLUMN false_positive;`,
	// 13: confidence becomes nullable, so NULL ("not reviewed yet") is told
	// apart from a deliberate 50 ("reviewed, but genuinely unsure"); without
	// that the reviewer re-reviews the same findings forever. The default 50
	// from schema 12 is taken as unreviewed (→ NULL); any other score, such
	// as the 20 a flagged false positive became, is a real judgement and is
	// kept. A nullable column replaces the NOT NULL one by copying the kept
	// scores into a new column (SQLite can't drop a NOT NULL constraint in
	// place), which touches only the findings table, not its child rows.
	12: `ALTER TABLE findings ADD COLUMN confidence_new INTEGER;` +
		`UPDATE findings SET confidence_new = confidence WHERE confidence <> 50;` +
		`ALTER TABLE findings DROP COLUMN confidence;` +
		`ALTER TABLE findings RENAME COLUMN confidence_new TO confidence;`,
	// 14: a finding carries a concern score (0 none, 100 critical) beside its
	// confidence, nullable like it — NULL until assessed. Findings already
	// recorded have not been assessed.
	13: `ALTER TABLE findings ADD COLUMN concern INTEGER`,
	// 15: the engagement keeps a record of the files the work recovered —
	// the source a map carried, a bundle taken apart — beside the findings
	// and the Explorer (see filesSchema). The files an older engagement
	// recovered are in its recovered/ folder but were never recorded, so
	// there is nothing to backfill: the table starts empty.
	14: filesSchema,
}

// upgrade brings an archive at an older schema up to Version, one step at a
// time, in a single transaction.
func upgrade(db *sql.DB, version int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed
	for v := version; v < Version; v++ {
		step, ok := upgrades[v]
		if !ok {
			return fmt.Errorf("unknown archive schema %d", v)
		}
		if _, err := tx.Exec(step); err != nil {
			return fmt.Errorf("upgrading the schema from %d: %w", v, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, Version)); err != nil {
		return fmt.Errorf("setting the schema version: %w", err)
	}
	return tx.Commit()
}

// ExportTo writes a copy of the archive to path and returns its size. It is
// SQLite's own VACUUM INTO, so the copy is consistent even while traffic is
// being recorded into this one, and compact: it is the whole engagement's
// traffic and findings in a file anything that reads SQLite can open.
//
// The destination must not exist; SQLite refuses to overwrite, and so does
// this, because the obvious mistake is naming a file you meant to keep.
func (a *Archive) ExportTo(path string) (int64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(abs); err == nil {
		return 0, fmt.Errorf("%s already exists", abs)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	// The path is quoted into the statement because VACUUM INTO takes no
	// parameters; doubling any quote is what keeps it one string literal.
	quoted := "'" + strings.ReplaceAll(abs, "'", "''") + "'"
	if _, err := a.db.Exec("VACUUM INTO " + quoted); err != nil {
		return 0, fmt.Errorf("exporting to %s: %w", abs, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
