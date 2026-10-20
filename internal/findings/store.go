package findings

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// EventKind identifies what changed in a Store.
type EventKind int

const (
	// Added is emitted when a new finding is recorded.
	Added EventKind = iota
	// Updated is emitted when a finding changes: an edit, a new sighting
	// merged into it, or the user viewing it.
	Updated
	// Removed is emitted when a finding is deleted.
	Removed
	// Reset is emitted when the whole list is replaced, as when an
	// engagement is opened. Finding is zero; subscribers reload from List.
	Reset
)

// Event describes a change to a Store.
type Event struct {
	Kind    EventKind
	Finding Finding
}

// Store holds the findings of the open engagement and keeps them in its
// capture.db.
//
// Which database that is depends on the open engagement: Use points the
// store at another one, replacing the list with its contents, which is how
// opening an engagement brings its findings with it (see package
// engagement). With no database (Use(nil)) the store is empty and read-only:
// there is no engagement to record into.
//
// A write goes straight to SQLite, under the lock. That is affordable
// because findings arrive at human and agent pace — nothing here is on the
// proxy's path, which is why findings are not queued like captured traffic.
// A failed write leaves the change in memory, so the user's edit stands for
// the session, and is reported.
type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	list   []*Finding // in ID order, oldest first
	byID   map[uint64]*Finding
	nextID uint64

	// types is the catalog: the user's types from the settings file
	// (userTypes) followed by those plugins declare (declared, by plugin),
	// minus any whose ID the user's already has.
	types     []Type
	userTypes []Type
	declared  map[string][]Type

	subs map[int]func(Event)
	next int

	// hold says whether a new finding waits for the reviewer's first look
	// (see SetReviewHold); nil holds nothing.
	hold func(Draft) bool
}

// NewStore returns an empty store with no database behind it. Call Use to
// point it at an engagement's capture.db.
func NewStore() *Store {
	return &Store{byID: make(map[uint64]*Finding), nextID: 1, subs: make(map[int]func(Event))}
}

// Use points the store at db — the open engagement's capture.db, whose
// schema package archive owns — and replaces the list with what it holds.
// A nil db empties the store and stops it recording.
//
// The caller must not close the previous database before calling this: the
// store reads the new one while it still holds the old handle.
func (s *Store) Use(db *sql.DB) error {
	s.mu.Lock()
	s.db = db
	s.list, s.byID, s.nextID = nil, make(map[uint64]*Finding), 1
	var err error
	if db != nil {
		err = s.loadLocked()
	}
	s.mu.Unlock()

	s.emit(Event{Kind: Reset})
	return err
}

// Recording reports whether there is a database to record into: false with
// no engagement open, or when opening its archive failed.
func (s *Store) Recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db != nil
}

// SetTypes replaces the type catalog. It is called with the types from the
// settings file at startup and whenever settings are applied. Types a
// plugin declares will be merged in through this same catalog.
//
// Findings keep type IDs, not copies of the types, so removing a type from
// the catalog doesn't alter any finding: it shows as an unknown type until
// one with that ID exists again.
func (s *Store) SetTypes(types []Type) error {
	seen := make(map[string]bool, len(types))
	out := make([]Type, 0, len(types))
	for _, t := range types {
		t.ID, t.Name = strings.TrimSpace(t.ID), strings.TrimSpace(t.Name)
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("two finding types share the id %q", t.ID)
		}
		seen[t.ID] = true
		out = append(out, t)
	}

	s.mu.Lock()
	s.userTypes = out
	s.rebuildTypesLocked()
	s.mu.Unlock()
	s.emit(Event{Kind: Reset}) // the pills change, so every row is restated
	return nil
}

// DeclareTypes adds the types a plugin declares to the catalog, replacing
// any it declared before. They last until WithdrawTypes and are never
// written to the settings file. A type with the ID of one the user has is
// left out: the user's wins. A type without an ID takes its name's slug, and
// one without a colour the next free one.
func (s *Store) DeclareTypes(owner string, types []Type) error {
	out := make([]Type, 0, len(types))
	s.mu.Lock()
	used := make([]string, 0, len(s.types))
	for _, t := range s.types {
		used = append(used, t.Color)
	}
	s.mu.Unlock()
	for _, t := range types {
		t.Name = strings.TrimSpace(t.Name)
		if t.ID = strings.TrimSpace(t.ID); t.ID == "" {
			t.ID = Slug(t.Name)
		}
		if t.Color == "" {
			t.Color = PickColor(used)
			used = append(used, t.Color)
		}
		if err := t.Validate(); err != nil {
			return err
		}
		out = append(out, t)
	}

	s.mu.Lock()
	if s.declared == nil {
		s.declared = make(map[string][]Type)
	}
	s.declared[owner] = out
	s.rebuildTypesLocked()
	s.mu.Unlock()
	s.emit(Event{Kind: Reset})
	return nil
}

// WithdrawTypes removes the types owner declared. Findings keep their type
// IDs, as when the user deletes a type.
func (s *Store) WithdrawTypes(owner string) {
	s.mu.Lock()
	_, had := s.declared[owner]
	delete(s.declared, owner)
	s.rebuildTypesLocked()
	s.mu.Unlock()
	if had {
		s.emit(Event{Kind: Reset})
	}
}

func (s *Store) rebuildTypesLocked() {
	all := append([]Type(nil), s.userTypes...)
	seen := make(map[string]bool, len(all))
	for _, t := range all {
		seen[t.ID] = true
	}
	owners := make([]string, 0, len(s.declared))
	for o := range s.declared {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, o := range owners {
		for _, t := range s.declared[o] {
			if !seen[t.ID] {
				seen[t.ID] = true
				all = append(all, t)
			}
		}
	}
	s.types = all
}

// Types returns the catalog, in the order it is shown.
func (s *Store) Types() []Type {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Type(nil), s.types...)
}

// Type returns one type by ID.
func (s *Store) Type(id string) (Type, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.typeLocked(id)
}

func (s *Store) typeLocked(id string) (Type, bool) {
	for _, t := range s.types {
		if t.ID == id {
			return t, true
		}
	}
	return Type{}, false
}

// ResolveType finds a type by its ID or its name, ignoring case, so an agent
// can say "credential" or "Credential" and a tool doesn't have to make the
// model guess at slugs.
func (s *Store) ResolveType(nameOrID string) (Type, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveTypeLocked(nameOrID)
}

func (s *Store) resolveTypeLocked(nameOrID string) (Type, bool) {
	want := strings.TrimSpace(nameOrID)
	for _, t := range s.types {
		if strings.EqualFold(t.ID, want) {
			return t, true
		}
	}
	for _, t := range s.types {
		if strings.EqualFold(t.Name, want) {
			return t, true
		}
	}
	// A name typed as prose ("Sensitive Data") against a slug id.
	for _, t := range s.types {
		if t.ID == Slug(want) {
			return t, true
		}
	}
	return Type{}, false
}

// TypeIDs returns the catalog's IDs, for an error message that tells a
// caller what it could have said.
func (s *Store) TypeIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.types))
	for _, t := range s.types {
		out = append(out, t.ID)
	}
	return out
}

// NextColor returns the colour a new type would be given: the first palette
// colour no type in the catalog holds.
func (s *Store) NextColor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := make([]string, 0, len(s.types))
	for _, t := range s.types {
		used = append(used, t.Color)
	}
	return PickColor(used)
}

// List returns every finding, oldest first. The findings are copies.
func (s *Store) List() []Finding {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Finding, len(s.list))
	for i, f := range s.list {
		out[i] = f.clone()
	}
	return out
}

// Get returns one finding by ID.
func (s *Store) Get(id uint64) (Finding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byID[id]
	if !ok {
		return Finding{}, false
	}
	return f.clone(), true
}

// Len returns how many findings there are.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// Unviewed returns how many findings the user hasn't opened. It is what the
// navigation bar's Findings badge shows, so the count and the highlighted
// rows can never disagree.
func (s *Store) Unviewed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.list {
		// An unreviewed finding (no score) needs attention; a reviewed one
		// only nags while its score is at or above the floor.
		if f.Unread() && (f.Confidence == nil || *f.Confidence >= ConfidenceFloor) {
			n++
		}
	}
	return n
}

// Record records d, or merges it into the finding that already holds its
// value, and returns the finding and whether a new one was made.
//
// Merging is the whole point: the same credential found on another endpoint
// is one finding with two sightings, not two findings. A merge unions in the
// draft's types, appends its sighting (unless that exact spot is already
// recorded), appends its note as a new paragraph, adds key/value entries the
// finding doesn't have, and — when the draft came from an agent or a plugin
// — marks the finding unviewed again, because a fresh sighting is news.
//
// A draft with an empty value never merges: there is nothing to match on.
func (s *Store) Record(d Draft) (Finding, bool, error) {
	d = d.normalized()
	if err := d.validate(); err != nil {
		return Finding{}, false, err
	}

	s.mu.Lock()
	types, err := s.resolveTypesLocked(d.Types)
	if err != nil {
		s.mu.Unlock()
		return Finding{}, false, err
	}
	now := time.Now()

	if existing := s.matchLocked(d.Key()); existing != nil {
		merge(existing, d, types, now, s.hold != nil)
		out := existing.clone()
		err := s.saveLocked(existing)
		s.mu.Unlock()
		s.emit(Event{Kind: Updated, Finding: out})
		return out, false, err
	}

	f := &Finding{
		ID:         s.nextID,
		Value:      d.Value,
		Types:      types,
		Notes:      d.Notes,
		Fields:     d.Fields,
		Source:     d.Source,
		Author:     d.Author,
		Hidden:     d.Hidden,
		Confidence: scored(d.Confidence), // nil unless the draft scored it: a new finding is unreviewed
		Concern:    scored(d.Concern),
		Viewed:     !d.Source.Raised(), // a user's own finding needs no notification
		Created:    now,
		Updated:    now,
	}
	// A finding the reviewer will score (it reviews those with no
	// confidence) waits for it rather than being announced now.
	f.AwaitingReview = d.Source.Raised() && d.Confidence == nil && s.hold != nil && s.hold(d)
	maybeAutoRead(f) // a draft pre-scored both-low needs no notification
	addSightings(f, d, now)
	s.nextID++
	s.list = append(s.list, f)
	s.byID[f.ID] = f
	out := f.clone()
	err = s.saveLocked(f)
	s.mu.Unlock()

	s.emit(Event{Kind: Added, Finding: out})
	return out, true, err
}

// matchLocked finds the finding holding this value, or nil.
func (s *Store) matchLocked(key string) *Finding {
	if key == "" {
		return nil
	}
	for _, f := range s.list {
		if strings.TrimSpace(f.Value) == key {
			return f
		}
	}
	return nil
}

// addSightings adds a draft's links — the single Link and any in Links — to
// a finding, each skipped if it says nothing or the finding already has it.
func addSightings(f *Finding, d Draft, now time.Time) {
	add := func(link Link) {
		if link.empty() {
			return
		}
		link.Added = now
		for _, l := range f.Links {
			if l.same(link) {
				return
			}
		}
		f.Links = append(f.Links, link)
	}
	add(d.Link)
	for _, l := range d.Links {
		add(l)
	}
}

// merge folds a draft into the finding that already holds its value. held
// says a reviewer is judging what the user sees, so a fresh sighting of a
// finding it has already scored is only news if that score was notable.
func merge(f *Finding, d Draft, types []string, now time.Time, held bool) {
	for _, id := range types {
		if !f.HasType(id) {
			f.Types = append(f.Types, id)
		}
	}
	if note := strings.TrimSpace(d.Notes); note != "" && !strings.Contains(f.Notes, note) {
		if f.Notes == "" {
			f.Notes = d.Notes
		} else {
			f.Notes += "\n\n" + d.Notes
		}
	}
	for _, kv := range d.Fields {
		if !hasField(f.Fields, kv.Key) {
			f.Fields = append(f.Fields, kv)
		}
	}
	addSightings(f, d, now)
	if d.Source.Raised() && !(held && f.Confidence != nil && f.Concern != nil && !f.Notable()) {
		// A fresh sighting is news; whether it nags the badge then depends on
		// the finding's confidence (see Unviewed), not on this.
		f.Viewed = false
	}
	if d.Hidden {
		// One sighting of a value as a secret is enough: a finding is never
		// un-hidden by a merge, only by the user (SetHidden).
		f.Hidden = true
	}
	if d.Confidence != nil {
		// A draft that carries an explicit judgement rescores the finding; a
		// draft without one leaves the score as it stands.
		c := clampScore(*d.Confidence)
		f.Confidence = &c
	}
	if d.Concern != nil {
		c := clampScore(*d.Concern)
		f.Concern = &c
	}
	maybeAutoRead(f) // a re-sighting with both scores low shouldn't nag
	settleReview(f)
	f.Updated = now
}

// settleReview ends a finding's wait for the reviewer once it has both
// scores: it stays unread if the review found it notable, and is marked
// read if not. A finding the user opened meanwhile stays read either way.
func settleReview(f *Finding) {
	if !f.AwaitingReview || f.Confidence == nil || f.Concern == nil {
		return
	}
	f.AwaitingReview = false
	if !f.Notable() {
		f.Viewed = true
	}
}

// SetReviewHold sets which new findings wait for the findings reviewer's
// first look instead of arriving unread (see Finding.AwaitingReview): the
// app sets it while a reviewer is enabled, and clears it (nil) when none is.
// It is consulted, under the store's lock, for each finding raised without
// a confidence score, so it must not call back into the store.
func (s *Store) SetReviewHold(hold func(Draft) bool) {
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
}

// AwaitingReview returns the IDs of the findings waiting for the reviewer,
// oldest first.
func (s *Store) AwaitingReview() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uint64
	for _, f := range s.list {
		if f.AwaitingReview {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// ReleaseReview stops the given findings waiting for the reviewer, so they
// arrive unread as if no reviewer were set: what the reviewer does with a
// finding it couldn't score (its turn failed, was stopped, or ended without
// getting to it). IDs no longer waiting are skipped, and nil releases every
// finding that is. It returns how many were released, with one Reset for
// the lot.
func (s *Store) ReleaseReview(ids []uint64) (int, error) {
	s.mu.Lock()
	var targets []*Finding
	if ids == nil {
		targets = s.list
	} else {
		for _, id := range ids {
			if f, ok := s.byID[id]; ok {
				targets = append(targets, f)
			}
		}
	}
	n := 0
	var firstErr error
	for _, f := range targets {
		if !f.AwaitingReview {
			continue
		}
		f.AwaitingReview = false
		if err := s.saveLocked(f); err != nil && firstErr == nil {
			firstErr = err
		}
		n++
	}
	s.mu.Unlock()
	if n > 0 {
		s.emit(Event{Kind: Reset})
	}
	return n, firstErr
}

// scored returns a clamped copy of a draft's score, or nil to leave the
// finding's axis unset.
func scored(v *int) *int {
	if v == nil {
		return nil
	}
	c := clampScore(*v)
	return &c
}

// maybeAutoRead marks a finding read once it has been judged both unlikely to
// be real and not a concern (both scores assessed and below autoReadBelow), so
// low-value findings don't sit in the unviewed badge waiting to be reviewed.
// It only ever marks read — it never reopens a finding.
func maybeAutoRead(f *Finding) {
	if f.Confidence != nil && *f.Confidence < autoReadBelow &&
		f.Concern != nil && *f.Concern < autoReadBelow {
		f.Viewed = true
	}
}

func hasField(fields []Field, key string) bool {
	for _, kv := range fields {
		if strings.EqualFold(kv.Key, key) {
			return true
		}
	}
	return false
}

// resolveTypesLocked turns a draft's type names or IDs into IDs, refusing
// one the catalog doesn't know — a typo must not create a finding nobody can
// filter for.
func (s *Store) resolveTypesLocked(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, want := range in {
		t, ok := s.resolveTypeLocked(want)
		if !ok {
			known := make([]string, 0, len(s.types))
			for _, t := range s.types {
				known = append(known, t.ID)
			}
			if len(known) == 0 {
				return nil, fmt.Errorf("no finding type %q is configured, and there are none to choose from; "+
					"add some in Settings → Findings", want)
			}
			return nil, fmt.Errorf("no finding type %q is configured; the types are: %s",
				want, strings.Join(known, ", "))
		}
		if !containsString(out, t.ID) {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

// MarkViewed records that the user opened a finding, which is what clears it
// from the Findings badge. Viewing is not an edit, so it leaves Updated
// alone.
func (s *Store) MarkViewed(id uint64) error {
	return s.change(id, false, func(f *Finding) error {
		f.Viewed = true
		return nil
	})
}

// MarkAllViewed marks every finding viewed and returns how many changed. It
// emits one Reset rather than an event per finding, so the list refreshes
// once.
func (s *Store) MarkAllViewed() (int, error) {
	s.mu.Lock()
	n := 0
	var firstErr error
	for _, f := range s.list {
		if f.Viewed {
			continue
		}
		f.Viewed = true
		if err := s.saveLocked(f); err != nil && firstErr == nil {
			firstErr = err
		}
		n++
	}
	s.mu.Unlock()
	if n > 0 {
		s.emit(Event{Kind: Reset})
	}
	return n, firstErr
}

// SetNotes replaces a finding's notes.
func (s *Store) SetNotes(id uint64, notes string) error {
	return s.change(id, true, func(f *Finding) error {
		f.Notes = strings.TrimRight(notes, " \t\r\n")
		return nil
	})
}

// SetValue replaces a finding's value. A value another finding already holds
// is refused rather than quietly creating the duplicate Record avoids.
func (s *Store) SetValue(id uint64, value string) error {
	return s.change(id, true, func(f *Finding) error {
		if key := strings.TrimSpace(value); key != "" {
			if other := s.matchLocked(key); other != nil && other.ID != id {
				return fmt.Errorf("finding %d already holds that value", other.ID)
			}
		}
		f.Value = value
		return nil
	})
}

// SetHidden masks or reveals a finding's value on screen (see
// Finding.Hidden). It is the user's decision, from the finding's dialog;
// the eye beside the value only reveals it for as long as they look.
func (s *Store) SetHidden(id uint64, hidden bool) error {
	return s.change(id, true, func(f *Finding) error {
		f.Hidden = hidden
		return nil
	})
}

// SetConfidence scores a finding 0–100 (see Finding.Confidence), clamping to
// range, or marks it unreviewed again with a nil score. It is a judgement:
// the user's from the finding dialog, or an agent's from
// set_finding_confidence as it reviews the traffic. A score below
// ConfidenceFloor drops the finding out of the list and the unviewed badge
// without deleting it, which is how the noise is triaged away.
func (s *Store) SetConfidence(id uint64, confidence *int) error {
	return s.change(id, true, func(f *Finding) error {
		f.Confidence = scored(confidence)
		maybeAutoRead(f)
		settleReview(f)
		return nil
	})
}

// SetConcern scores how big an issue a finding is 0–100 (see Finding.Concern),
// clamping to range, or marks it unassessed again with a nil score. Scoring it
// both unlikely to be real and not a concern marks it read (see maybeAutoRead).
func (s *Store) SetConcern(id uint64, concern *int) error {
	return s.change(id, true, func(f *Finding) error {
		f.Concern = scored(concern)
		maybeAutoRead(f)
		settleReview(f)
		return nil
	})
}

// SetFields replaces a finding's key/value store.
func (s *Store) SetFields(id uint64, fields []Field) error {
	return s.change(id, true, func(f *Finding) error {
		f.Fields = trimFields(fields)
		return nil
	})
}

// Classify replaces a finding's types, resolving names or IDs against the
// catalog.
func (s *Store) Classify(id uint64, types []string) error {
	return s.change(id, true, func(f *Finding) error {
		resolved, err := s.resolveTypesLocked(dedupeStrings(types))
		if err != nil {
			return err
		}
		f.Types = resolved
		return nil
	})
}

// change applies mutate to one finding, persists it and notifies. touch
// bumps the finding's Updated time; viewing doesn't.
func (s *Store) change(id uint64, touch bool, mutate func(*Finding) error) error {
	s.mu.Lock()
	f, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no finding %d", id)
	}
	before := f.clone()
	if err := mutate(f); err != nil {
		*f = before // leave the finding exactly as it was
		s.mu.Unlock()
		return err
	}
	if touch {
		f.Updated = time.Now()
	}
	out := f.clone()
	err := s.saveLocked(f)
	s.mu.Unlock()

	s.emit(Event{Kind: Updated, Finding: out})
	return err
}

// Remove deletes a finding.
func (s *Store) Remove(id uint64) error {
	s.mu.Lock()
	f, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	out := f.clone()
	for i := range s.list {
		if s.list[i].ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			break
		}
	}
	delete(s.byID, id)
	err := s.deleteLocked(id)
	s.mu.Unlock()

	s.emit(Event{Kind: Removed, Finding: out})
	return err
}

// Subscribe registers fn to be called after any change. The returned
// function removes it; fn runs on the goroutine that made the change and
// must not block (GUI code hands the event to its own thread).
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

// Query selects findings for a list or for tool output. The zero value
// matches everything.
type Query struct {
	// Types keeps only findings carrying at least one of these type IDs.
	Types []string
	// Unviewed keeps only the findings the user hasn't opened.
	Unviewed bool
	// MinConfidence hides reviewed findings scored below this (see
	// Finding.Confidence). Unreviewed findings (no score) are always kept,
	// since they haven't been judged. Zero keeps everything, so the cutoff is
	// simply off by default; the findings page sets it to ConfidenceFloor to
	// hide the noise an agent has scored down.
	MinConfidence int
	// MinConcern hides findings whose concern is scored below this (see
	// Finding.Concern), in the same way: a finding with no concern score is
	// kept, and zero keeps everything.
	MinConcern int
	// UnreviewedOnly keeps only findings that have not been scored yet.
	UnreviewedOnly bool
	// FlowID keeps only findings seen in one request.
	FlowID uint64
	// Match, when set, is applied to each finding's SearchText.
	Match func(string) bool
}

// Select returns the findings matching q, newest first — the order the list
// shows them in, since the newest finding is the one worth seeing.
func (s *Store) Select(q Query) []Finding {
	all := s.List()
	out := make([]Finding, 0, len(all))
	for _, f := range all {
		if q.Unviewed && !f.Unread() {
			continue
		}
		if q.UnreviewedOnly && f.Confidence != nil {
			continue
		}
		// The cutoff hides only reviewed-and-scored-low findings; an
		// unreviewed finding has no score and is kept.
		if f.Confidence != nil && *f.Confidence < q.MinConfidence {
			continue
		}
		if f.Concern != nil && *f.Concern < q.MinConcern {
			continue
		}
		if len(q.Types) > 0 && !anyType(f, q.Types) {
			continue
		}
		if q.FlowID != 0 && !seenIn(f, q.FlowID) {
			continue
		}
		if q.Match != nil && !q.Match(f.SearchText()) {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out
}

func anyType(f Finding, types []string) bool {
	for _, id := range types {
		if f.HasType(id) {
			return true
		}
	}
	return false
}

func seenIn(f Finding, flowID uint64) bool {
	for _, l := range f.Links {
		if l.FlowID == flowID {
			return true
		}
	}
	return false
}
