package files

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"reproxy/internal/atomicfile"
	"reproxy/internal/randid"
)

// EventKind identifies what changed in a Store.
type EventKind int

const (
	// Added is emitted when a file is recovered for the first time.
	Added EventKind = iota
	// Updated is emitted when a file already recorded is recovered again
	// with different bytes, or its record is edited. Recovering the same
	// bytes again raises nothing.
	Updated
	// Removed is emitted when a file is deleted.
	Removed
	// Reset is emitted when the whole list is replaced, as when an
	// engagement is opened, or cleared. File is zero; subscribers reload
	// from List.
	Reset
	// BatchDone is emitted once after a recovery of many files
	// (Store.RecordAll), with the batch summarised in Event.Batch and a
	// zero File. It is what lets a plugin that cannot keep up with
	// thousands of per-file events pick the batch up at its own pace: the
	// files are already in the store, so Select(Query{Batch: …}) has them
	// all whether or not their Added events survived the queue.
	BatchDone
)

func (k EventKind) String() string {
	switch k {
	case Added:
		return "added"
	case Updated:
		return "updated"
	case Removed:
		return "removed"
	case Reset:
		return "reset"
	case BatchDone:
		return "batch_done"
	}
	return "unknown"
}

// Event describes a change to a Store.
type Event struct {
	Kind EventKind
	File File
	// Batch is set on BatchDone: the recovery that just finished.
	Batch Batch
}

// ErrNoRoot is returned by Record when the store has nowhere to write:
// there is no engagement open and no data directory was given.
var ErrNoRoot = errors.New("there is nowhere to write recovered files")

// ErrNotFound is returned for a file ID or path the store doesn't hold.
var ErrNotFound = errors.New("no such recovered file")

// Store holds the recovered files of the open engagement: their records in
// its capture.db, their bytes under a directory of its own.
//
// Which database and directory those are depends on the open engagement:
// Use points the store at another pair, replacing the list with what the
// database holds, which is how opening an engagement brings its recovered
// files with it. With no database the store still writes files and keeps
// them for the session (so the user's recovery is not refused because no
// engagement is open), exactly as the findings store keeps an unsaved
// finding.
//
// A write goes to the filesystem and then straight to SQLite, under the
// lock. That is affordable because nothing here is on the proxy's path:
// files are recovered at a human's, an agent's or a plugin's pace. The
// Dispatcher's subscriber turns each event into a plugin hook on its own
// queue, so no plugin's work happens under this lock either.
type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	root   string
	list   []*File // in ID order, oldest first
	byID   map[uint64]*File
	byPath map[string]*File
	nextID uint64

	subs map[int]func(Event)
	next int
}

// NewStore returns an empty store with no database and nowhere to write.
// Call Use to point it at an engagement.
func NewStore() *Store {
	return &Store{byID: map[uint64]*File{}, byPath: map[string]*File{}, nextID: 1, subs: map[int]func(Event){}}
}

// Use points the store at db — the open engagement's capture.db, whose
// schema package archive owns — and at root, the directory its recovered
// files live under. It replaces the list with what the database holds. A
// nil db empties the records; an empty root stops the store writing
// (Record then returns ErrNoRoot).
//
// The caller must not close the previous database before calling this: the
// store reads the new one while it still holds the old handle.
func (s *Store) Use(db *sql.DB, root string) error {
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	s.mu.Lock()
	s.db, s.root = db, root
	s.list, s.byID, s.byPath, s.nextID = nil, map[uint64]*File{}, map[string]*File{}, 1
	var err error
	if db != nil {
		err = s.loadLocked()
	}
	s.mu.Unlock()

	s.emit(Event{Kind: Reset})
	return err
}

// Recording reports whether there is a database to record into: false with
// no engagement open, or when opening its archive failed. Files are still
// written and kept for the session when it is false.
func (s *Store) Recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db != nil
}

// Root is the directory recovered files are written under, or "" when there
// is nowhere to write.
func (s *Store) Root() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// List returns every recovered file, oldest first.
func (s *Store) List() []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]File, len(s.list))
	for i, f := range s.list {
		out[i] = *f
	}
	return out
}

// Len is how many files are recorded.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// Get returns one file by ID.
func (s *Store) Get(id uint64) (File, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byID[id]
	if !ok {
		return File{}, false
	}
	return *f, true
}

// ByPath returns one file by its path relative to the root.
func (s *Store) ByPath(rel string) (File, bool) {
	clean, ok := SafeRel(rel)
	if !ok {
		return File{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byPath[clean]
	if !ok {
		return File{}, false
	}
	return *f, true
}

// Select returns the files matching a query, newest first.
func (s *Store) Select(q Query) []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]File, 0, 16)
	for _, p := range s.list {
		if f := *p; q.match(f) {
			out = append(out, f)
		}
	}
	// Newest first, as the Findings list does, and the limit takes the
	// newest rather than whatever the scan reached first.
	sort.SliceStable(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// Batches summarises the recoveries the store holds, newest first. A file
// recovered on its own has no batch and is in none of them.
func (s *Store) Batches() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[string]*Batch{}
	var order []string
	for _, f := range s.list {
		if f.Batch == "" {
			continue
		}
		b, ok := byID[f.Batch]
		if !ok {
			b = &Batch{
				ID: f.Batch, Label: f.BatchLabel, Kind: f.Kind, Source: f.Source,
				Author: f.Author, Host: f.Host, FlowID: f.FlowID, Created: f.Created,
			}
			byID[f.Batch] = b
			order = append(order, f.Batch)
		}
		b.Files++
		b.Bytes += f.Size
		if f.Created.Before(b.Created) {
			b.Created = f.Created
		}
	}
	out := make([]Batch, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sortBatches(out)
	return out
}

// Read returns a file's bytes, at most max of them (0 or less: the whole
// file). It reads from disk, so it reports a file the user has deleted or
// moved since.
func (s *Store) Read(id uint64, max int64) ([]byte, error) {
	s.mu.Lock()
	f, ok := s.byID[id]
	root := s.root
	var rel string
	if ok {
		rel = f.Path
	}
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w %d", ErrNotFound, id)
	}
	return readFile(root, rel, max)
}

func readFile(root, rel string, max int64) ([]byte, error) {
	if root == "" {
		return nil, ErrNoRoot
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if !within(root, target) {
		return nil, fmt.Errorf("reading %s: the path escapes the recovered files directory", rel)
	}
	fh, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("reading the recovered file %s: %w", rel, err)
	}
	defer fh.Close()
	var r io.Reader = fh
	if max > 0 {
		r = io.LimitReader(fh, max)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading the recovered file %s: %w", rel, err)
	}
	return data, nil
}

// Record writes one recovered file and records it. created is true for a
// file the store had not seen at that path.
//
// Recovering a path the store already holds, with the same bytes, changes
// nothing and raises no event: created is false and the file comes back as
// it stands. That is what keeps a second run of the same recovery from
// setting every plugin that scans recovered files off again. Different
// bytes replace the file and raise Updated.
func (s *Store) Record(d Draft) (File, bool, error) {
	d = d.normalized()
	if err := d.validate(); err != nil {
		return File{}, false, err
	}
	rel, ok := SafeRel(d.Path)
	if !ok {
		return File{}, false, fmt.Errorf("%q is not a path a file can be recovered to", d.Path)
	}

	s.mu.Lock()
	f, created, ev, err := s.recordLocked(rel, d, time.Now())
	s.mu.Unlock()

	if ev != nil {
		s.emit(*ev)
	}
	return f, created, err
}

// RecordAll writes one recovery's worth of files: a source map unpacked, a
// bundle taken apart. The drafts are written in order, under lim, and share
// one batch ID — their own if they set one, else a new one — so a plugin
// that misses the per-file events can still find the whole recovery with
// Select(Query{Batch: …}).
//
// Every file raises its own event, and one BatchDone follows for the
// recovery as a whole. A draft with an unsafe path, or one past a limit, is
// reported in Result.Skipped rather than failing the recovery; an error is
// returned only when something was genuinely wrong (nowhere to write, a
// failed write).
func (s *Store) RecordAll(drafts []Draft, lim Limits) (Result, error) {
	lim = lim.withDefaults()
	batch, label := "", ""
	for _, d := range drafts {
		if batch == "" {
			batch = strings.TrimSpace(d.Batch)
		}
		if label == "" {
			label = strings.TrimSpace(d.BatchLabel)
		}
	}
	if batch == "" {
		batch = randid.Hex(8)
	}

	res := Result{Batch: batch, Dir: s.Root()}
	now := time.Now()
	var events []Event
	var first Draft
	if len(drafts) > 0 {
		first = drafts[0].normalized()
	}

	s.mu.Lock()
	if s.root == "" {
		s.mu.Unlock()
		return res, ErrNoRoot
	}
	res.Dir = s.root
	var err error
	for _, d := range drafts {
		d = d.normalized()
		d.Batch, d.BatchLabel = batch, label
		if err = d.validate(); err != nil {
			break
		}
		rel, ok := SafeRel(d.Path)
		if !ok {
			res.Skipped = append(res.Skipped, d.Path+" (not a path a file can be recovered to)")
			continue
		}
		size := int64(len(d.Content))
		switch {
		case res.Files >= lim.MaxFiles:
			res.Skipped = append(res.Skipped, rel+" (file limit reached)")
			continue
		case res.Bytes+size > lim.MaxBytes:
			res.Skipped = append(res.Skipped, rel+" (size limit reached)")
			continue
		}
		f, _, ev, werr := s.recordLocked(rel, d, now)
		if werr != nil {
			err = werr
			break
		}
		res.Recorded = append(res.Recorded, f)
		if ev == nil { // already recorded, byte for byte
			res.Unchanged++
			continue
		}
		events = append(events, *ev)
		res.Files++
		res.Bytes += size
	}
	s.mu.Unlock()

	for _, ev := range events {
		s.emit(ev)
	}
	if len(res.Recorded) > 0 {
		s.emit(Event{Kind: BatchDone, Batch: Batch{
			ID: batch, Label: label, Files: len(res.Recorded), Bytes: res.Bytes,
			Kind: first.Kind, Source: first.Source, Author: first.Author,
			Host: first.Host, FlowID: first.FlowID, Created: now,
		}})
	}
	return res, err
}

// recordLocked writes the bytes and upserts the record. The event to emit
// is nil when nothing changed. The caller holds the lock.
func (s *Store) recordLocked(rel string, d Draft, now time.Time) (File, bool, *Event, error) {
	if s.root == "" {
		return File{}, false, nil, ErrNoRoot
	}
	sum := hash(d.Content)
	if was, ok := s.byPath[rel]; ok && was.SHA256 == sum {
		return *was, false, nil, nil // recovered before, byte for byte
	}
	if err := s.write(rel, d.Content); err != nil {
		return File{}, false, nil, err
	}

	f, created := s.byPath[rel], false
	if f == nil {
		created = true
		f = &File{ID: s.nextID, Path: rel, Created: now}
		s.nextID++
		s.list = append(s.list, f)
		s.byID[f.ID] = f
		s.byPath[rel] = f
	}
	f.Size = int64(len(d.Content))
	f.SHA256 = sum
	f.MediaType = MediaType(rel, d.Content)
	f.Kind, f.Notes = d.Kind, d.Notes
	f.Source, f.Author = d.Source, d.Author
	f.Batch, f.BatchLabel = d.Batch, d.BatchLabel
	f.FlowID, f.Method, f.URL, f.Host = d.FlowID, d.Method, d.URL, d.Host
	f.Entity = d.Entity
	f.Updated = now

	out := *f
	kind := Updated
	if created {
		kind = Added
	}
	// A failed save leaves the file on disk and the record in memory, so
	// the recovery stands for the session; the error is reported.
	err := s.saveLocked(f)
	return out, created, &Event{Kind: kind, File: out}, err
}

// write puts the bytes at rel, inside the root. Files are 0600 and
// directories 0700: recovered client source is engagement data.
func (s *Store) write(rel string, content []byte) error {
	target := filepath.Join(s.root, filepath.FromSlash(rel))
	if !within(s.root, target) {
		// SafeRel should already have prevented this; refuse regardless.
		return fmt.Errorf("writing %s: the path escapes %s", rel, s.root)
	}
	if err := atomicfile.Write(target, content, atomicfile.Private, atomicfile.PrivateDir); err != nil {
		return fmt.Errorf("writing the recovered file %s: %w", rel, err)
	}
	return nil
}

// SetNotes replaces a file's notes.
func (s *Store) SetNotes(id uint64, notes string) error {
	return s.change(id, func(f *File) { f.Notes = strings.TrimRight(notes, " \t\r\n") })
}

func (s *Store) change(id uint64, mutate func(*File)) error {
	s.mu.Lock()
	f, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w %d", ErrNotFound, id)
	}
	mutate(f)
	f.Updated = time.Now()
	out := *f
	err := s.saveLocked(f)
	s.mu.Unlock()

	s.emit(Event{Kind: Updated, File: out})
	return err
}

// Remove deletes a file: its record and the bytes on disk, with the
// directories left empty by it.
func (s *Store) Remove(id uint64) error {
	s.mu.Lock()
	f, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	out := *f
	for i := range s.list {
		if s.list[i].ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			break
		}
	}
	delete(s.byID, id)
	delete(s.byPath, f.Path)
	err := s.deleteLocked(id)
	if rmErr := s.erase(out.Path); err == nil {
		err = rmErr
	}
	s.mu.Unlock()

	s.emit(Event{Kind: Removed, File: out})
	return err
}

// Clear removes every recovered file: the records, and the bytes of each
// file the store knows about. Nothing it never recorded is touched, so a
// note the user left in the folder survives.
func (s *Store) Clear() error {
	s.mu.Lock()
	var err error
	for _, f := range s.list {
		if rmErr := s.erase(f.Path); err == nil {
			err = rmErr
		}
	}
	s.list, s.byID, s.byPath = nil, map[uint64]*File{}, map[string]*File{}
	if s.db != nil {
		if _, dbErr := s.db.Exec(`DELETE FROM files`); dbErr != nil && err == nil {
			err = fmt.Errorf("clearing the recovered files: %w", dbErr)
		}
	}
	s.mu.Unlock()

	s.emit(Event{Kind: Reset})
	return err
}

// erase deletes one file's bytes and prunes the directories it leaves
// empty, up to but never including the root. The caller holds the lock.
func (s *Store) erase(rel string) error {
	if s.root == "" {
		return nil
	}
	target := filepath.Join(s.root, filepath.FromSlash(rel))
	if !within(s.root, target) || target == s.root {
		return nil
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting the recovered file %s: %w", rel, err)
	}
	for dir := filepath.Dir(target); within(s.root, dir) && dir != s.root; dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil { // fails while anything is left in it
			return nil
		}
	}
	return nil
}

// within reports whether target is root itself or sits inside it, comparing
// cleaned absolute paths so a "../" cannot slip through.
func within(root, target string) bool {
	abs, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+string(os.PathSeparator))
}

// Subscribe registers fn to be called after any change. The returned
// function removes it; fn runs on the goroutine that made the change and
// must not block (GUI code hands the event to its own thread, and the
// plugin Dispatcher only queues).
func (s *Store) Subscribe(fn func(Event)) (unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.next
	s.next++
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Store) emit(e Event) {
	s.mu.Lock()
	fns := make([]func(Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(e)
	}
}
