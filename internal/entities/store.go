package entities

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// EventKind identifies what changed in a Store.
type EventKind int

const (
	// Created is emitted when an entity is put in the store.
	Created EventKind = iota
	// Updated is emitted when an entity changes: a URL gains a request or
	// a link, a request reaches something under a host or domain, its
	// key/value store is edited.
	Updated
	// Deleted is emitted for each entity removed, children before their
	// parent, and for each entity Clear takes, before the Cleared event.
	Deleted
	// Cleared is emitted when the store is emptied. Entity is zero.
	Cleared
	// Reset is emitted when the whole store is replaced, as when an
	// engagement is opened. Entity is zero; subscribers reload from List.
	Reset
	// RelatedAdded is emitted when a host becomes related for the first
	// time (see Related). Host is the host; Entity is zero.
	RelatedAdded
	// RelatedChanged is emitted when which hosts are related was worked
	// out again (the targets or the suppressions changed), whether or not
	// anything is new. Entity is zero.
	RelatedChanged
)

// Event describes a change to a Store.
type Event struct {
	Kind   EventKind
	Entity Entity
	// Host is the related host, for RelatedAdded.
	Host string
	// InTarget is set on Created, Updated and Deleted when the entity
	// belongs to a target: a target covers its host, or it is what a
	// target stands for (SyncTargets). A related host's entities, and
	// those of traffic collected with no target covering it, are not.
	// Every entity is while the store has no scope (SetScope).
	InTarget bool
}

// ErrNotRecording is returned when an entity is added with no engagement
// database to keep it in.
var ErrNotRecording = errors.New("no engagement is open to record the entity in")

// ErrNotFound is returned for a key the store doesn't hold.
var ErrNotFound = errors.New("no such entity")

// Batch is a set of changes applied together: in one transaction, with one
// event per entity touched.
type Batch struct {
	Visits      []Visit
	Discoveries []Discovery
}

// Seed is an entity a target stands for: the domain or address the user
// put in scope (see SyncTargets).
type Seed struct {
	Type     Type // TypeDomain, or TypeHost for an address-only host
	Name     string
	TargetID string
}

// Store holds the open engagement's entities and keeps them in its
// capture.db.
//
// Like findings.Store, Use points it at another engagement's database and
// replaces the entities with what that holds; with no database it still
// works, in memory only, so that tests and a session with no engagement
// still have an Explorer. Unlike findings, changes arrive in batches from
// the Indexer (off the proxy's goroutine) and from plugins; each batch is
// one transaction.
type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	list   []*Entity // in ID order
	byKey  map[string]*Entity
	nextID uint64 // for a store with no database

	// scope says which target covers a host; nil until SetScope. related
	// is the hosts related now, known every host announced as related
	// since the store was loaded (so one is announced once).
	scope      ScopeFunc
	suppressed SuppressFunc
	related    map[string]bool
	known      map[string]bool
	// seeds are the entities the targets stand for, by key (see
	// SyncTargets).
	seeds map[string]Seed

	subMu   sync.Mutex
	subs    map[int]func(Event)
	nextSub int
}

// NewStore returns an empty store with no database behind it.
func NewStore() *Store {
	return &Store{
		byKey: make(map[string]*Entity), nextID: 1, subs: make(map[int]func(Event)),
		related: map[string]bool{}, known: map[string]bool{}, seeds: map[string]Seed{},
	}
}

// Use points the store at db — the open engagement's capture.db, whose
// schema package archive owns — and replaces the entities with what it
// holds. A nil db empties the store and keeps it in memory. What the
// targets stand for (SyncTargets) is put in again.
//
// The caller must not close the previous database before calling this.
func (s *Store) Use(db *sql.DB) error {
	s.mu.Lock()
	s.db = db
	s.list, s.byKey, s.nextID = nil, make(map[string]*Entity), 1
	var err error
	if db != nil {
		err = s.loadLocked()
	}
	// What the engagement already relates to was announced when it was
	// found; only what turns up from here on is news.
	s.related = s.relatedLocked()
	s.known = make(map[string]bool, len(s.related))
	for h := range s.related {
		s.known[h] = true
	}
	c := s.change()
	if err == nil {
		s.completeLocked(c)
	}
	s.seedLocked(c)
	werr := s.commitLocked(c)
	s.mu.Unlock()

	s.emit(Event{Kind: Reset})
	if err == nil {
		err = werr
	}
	return err
}

// Recording reports whether there is a database behind the store.
func (s *Store) Recording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db != nil
}

// Subscribe registers fn to be called after every change, on the goroutine
// that made it. fn must not block or call back into the store. The returned
// function removes the subscription.
func (s *Store) Subscribe(fn func(Event)) (unsubscribe func()) {
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.subMu.Unlock()
	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Store) emit(events ...Event) {
	if len(events) == 0 {
		return
	}
	s.subMu.Lock()
	fns := make([]func(Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.subMu.Unlock()
	for _, e := range events {
		for _, fn := range fns {
			fn(e)
		}
	}
}

// List returns a copy of every entity, in the order they were first seen.
func (s *Store) List() []Entity {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entity, len(s.list))
	for i, e := range s.list {
		out[i] = e.Clone()
	}
	return out
}

// Len returns the number of entities.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// Get returns the entity with the given key.
func (s *Store) Get(key string) (Entity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byKey[key]
	if !ok {
		return Entity{}, false
	}
	return e.Clone(), true
}

// Lookup returns the URL a concrete URL falls under.
func (s *Store) Lookup(scheme, host, path string) (Entity, bool) {
	return s.Get(AddressOf(scheme, host, path).Key())
}

// Children returns the entities directly under key, in the order they were
// first seen.
func (s *Store) Children(key string) []Entity {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entity
	for _, e := range s.list {
		if e.Parent == key && key != "" {
			out = append(out, e.Clone())
		}
	}
	return out
}

// ResolvingTo returns the names of the hosts known to resolve to an
// address (HostAttrs.Addresses), in the order they were first seen.
func (s *Store) ResolvingTo(addr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.list {
		if e.IsName() && slices.Contains(e.HostAttrs.Addresses, addr) {
			out = append(out, e.Host)
		}
	}
	return out
}

// ByTarget returns the entities first seen under the target, or every
// entity when id is "".
func (s *Store) ByTarget(id string) []Entity {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entity
	for _, e := range s.list {
		if id == "" || e.TargetID == id {
			out = append(out, e.Clone())
		}
	}
	return out
}

// Discover adds one discovered URL, as a plugin does: it is put in the
// store if it isn't there (with its host and domain), and the link recorded
// either way. created is true when the URL is new.
func (s *Store) Discover(d Discovery) (Entity, bool, error) {
	if d.Link.Added.IsZero() {
		d.Link.Added = time.Now()
	}
	changed, err := s.Apply(Batch{Discoveries: []Discovery{d}})
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byKey[d.Address.Key()]
	if !ok {
		return Entity{}, false, err
	}
	created := false
	for _, c := range changed {
		if c.Kind == Created && c.Entity.Key == e.Key {
			created = true
		}
	}
	return e.Clone(), created, err
}

// ErrBadHost is returned for a host that is neither a DNS name nor an IP
// address.
var ErrBadHost = errors.New("not a host name or IP address")

// DiscoverHost puts a name host in the store (with its domain), and a host
// for each address d says it resolves to, each discovered by d.Link. A
// host already there gains the link and any new addresses. It returns the
// name's host, and whether it is new.
func (s *Store) DiscoverHost(d HostDiscovery) (Entity, bool, error) {
	name, ok := hostName(d.Name)
	if !ok {
		return Entity{}, false, fmt.Errorf("%w: %q", ErrBadHost, d.Name)
	}
	var addrs []string
	for _, a := range d.Addresses {
		ip, err := netip.ParseAddr(strings.TrimSpace(a))
		if err != nil || ip.Zone() != "" {
			return Entity{}, false, fmt.Errorf("%w: %q is not an IP address", ErrBadHost, a)
		}
		addrs = append(addrs, ip.Unmap().String())
	}
	if d.Link.Added.IsZero() {
		d.Link.Added = time.Now()
	}
	s.mu.Lock()
	targetID := d.TargetID
	if s.scope != nil {
		targetID, _ = s.scope(name)
	}
	c := s.change()
	add := func(name, targetID string) *Entity {
		e := s.ensureLocked(c, TypeHost, "", name, "", targetID, d.Link.Source, d.Link.Added)
		if e.addLink(d.Link) {
			c.touch(e).links = append(c.touch(e).links, d.Link)
		}
		return e
	}
	e := add(name, targetID)
	for _, a := range addrs {
		if a == name {
			continue
		}
		tid := ""
		if s.scope != nil {
			tid, _ = s.scope(a)
		}
		add(a, tid)
		if len(e.HostAttrs.Addresses) < maxAddresses && !slices.Contains(e.HostAttrs.Addresses, a) {
			e.HostAttrs.Addresses = append(e.HostAttrs.Addresses, a)
			c.touch(e)
		}
	}
	err := s.commitLocked(c)
	events := s.eventsLocked(c, Updated)
	for _, h := range c.order {
		if host := BareHost(h.Host); s.relatesLocked(host, c.touched[h].links) {
			events = append(events, s.markRelatedLocked(host)...)
		}
	}
	created := c.touched[e] != nil && c.touched[e].created
	out := e.Clone()
	s.mu.Unlock()

	s.emit(events...)
	return out, created, err
}

// hostName is a host as a name host keys it: a DNS name (lower case, no
// trailing dot) or an IP address, with no scheme, port or path.
func hostName(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "/?#@ \t") {
		return "", false
	}
	if ip, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil && ip.Zone() == "" {
		return ip.Unmap().String(), true
	}
	if strings.Contains(raw, ":") {
		return "", false // a port, which a name host doesn't have
	}
	name := strings.TrimSuffix(strings.ToLower(raw), ".")
	if len(name) == 0 || len(name) > 253 {
		return "", false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return "", false
			}
		}
	}
	return name, true
}

// Apply merges a batch into the store and writes every entity it touched
// in one transaction. It returns the events it emitted. A failed write
// leaves the change in memory for the session, and is reported.
func (s *Store) Apply(b Batch) ([]Event, error) {
	if len(b.Visits) == 0 && len(b.Discoveries) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	c := s.change()
	for _, v := range b.Visits {
		a := v.Address
		if s.byKey[keyOf(TypeURL, a.Scheme, a.Host, a.Path)] == nil && !v.found() {
			// A request for a path that isn't there (a probe for /.env that
			// came back 404) doesn't put it in the Explorer. The origin
			// that answered is still mapped, and the hit counted on it; a
			// request nothing answered maps nothing. A URL already known is
			// visited whatever the answer, so its record stays truthful.
			if v.Status != 0 {
				for at := s.ensureLocked(c, TypeHost, a.Scheme, a.Host, "", v.TargetID, OriginTraffic, v.Seen); at != nil; at = s.byKey[at.Parent] {
					at.hit(v)
					c.touch(at)
				}
			}
			continue
		}
		e := s.ensureLocked(c, TypeURL, a.Scheme, a.Host, a.Path, v.TargetID, OriginTraffic, v.Seen)
		e.visit(v)
		p := c.touch(e)
		p.sightings = append(p.sightings, e.URLAttrs.Sightings[len(e.URLAttrs.Sightings)-1])
		for at := e; at != nil; at = s.byKey[at.Parent] {
			at.hit(v)
			c.touch(at)
		}
	}
	for _, d := range b.Discoveries {
		targetID := d.TargetID
		if s.scope != nil {
			// A link can point anywhere: the URL belongs to the target
			// covering its own host, not the one of the page that linked.
			targetID, _ = s.scope(BareHost(d.Address.Host))
		}
		a := d.Address
		e := s.ensureLocked(c, TypeURL, a.Scheme, a.Host, a.Path, targetID, d.Link.Source, d.Link.Added)
		if e.discover(d) {
			p := c.touch(e)
			p.links = append(p.links, d.Link)
		}
	}
	err := s.commitLocked(c)
	events := s.eventsLocked(c, Updated)
	for _, e := range c.order {
		if e.Type != TypeURL {
			continue
		}
		if h := BareHost(e.Host); s.relatesLocked(h, c.touched[e].links) {
			events = append(events, s.markRelatedLocked(h)...)
		}
	}
	s.mu.Unlock()

	s.emit(events...)
	return events, err
}

// change is what one operation did to the store, for the write and the
// events: each entity touched, in the order first touched (so a new parent
// comes before its new children).
type change struct {
	touched map[*Entity]*pending
	order   []*Entity
}

// pending is what an operation changed on one entity.
type pending struct {
	created   bool
	sightings []Sighting
	links     []Link
}

func (s *Store) change() *change { return &change{touched: map[*Entity]*pending{}} }

func (c *change) touch(e *Entity) *pending {
	p := c.touched[e]
	if p == nil {
		p = &pending{}
		c.touched[e] = p
		c.order = append(c.order, e)
	}
	return p
}

// eventsLocked is an event per entity c touched: Created for the new
// ones, kind for the rest.
func (s *Store) eventsLocked(c *change, kind EventKind) []Event {
	events := make([]Event, 0, len(c.order))
	for _, e := range c.order {
		k := kind
		if c.touched[e].created {
			k = Created
		}
		events = append(events, Event{Kind: k, Entity: e.Clone(), InTarget: s.inTargetLocked(e)})
	}
	return events
}

// inTargetLocked reports whether e belongs to a target (see
// Event.InTarget).
func (s *Store) inTargetLocked(e *Entity) bool {
	if s.scope == nil {
		return true
	}
	if _, ok := s.seeds[e.Key]; ok {
		return true
	}
	_, ok := s.scope(BareHost(e.Host))
	return ok
}

// ensureLocked returns the entity of type t at an address, creating it —
// and, first, whatever it sits under — when it isn't there.
func (s *Store) ensureLocked(c *change, t Type, scheme, host, path, targetID, origin string, at time.Time) *Entity {
	if e := s.byKey[keyOf(t, scheme, host, path)]; e != nil {
		return e
	}
	e := newEntity(t, scheme, host, path, targetID, origin)
	e.FirstSeen, e.LastSeen = at, at
	if pt, ps, ph, ok := parentOf(e); ok {
		s.ensureLocked(c, pt, ps, ph, "", targetID, origin, at)
	}
	s.byKey[e.Key] = e
	s.list = append(s.list, e)
	c.touch(e).created = true
	return e
}

// commitLocked writes what c touched (or, with no database, numbers what
// it created).
func (s *Store) commitLocked(c *change) error {
	if len(c.order) == 0 {
		return nil
	}
	if s.db != nil {
		return s.writeLocked(c)
	}
	for _, e := range c.order {
		if c.touched[e].created {
			e.ID = s.nextID
			s.nextID++
		}
	}
	return nil
}

// completeLocked puts in what entities loaded from an older archive sit
// under — an archive kept only URLs once, and origins sat directly under
// their domain before name hosts — counting the hits below each parent it
// creates. An entity whose parent has changed is moved, and rewritten.
func (s *Store) completeLocked(c *change) {
	for _, e := range append([]*Entity(nil), s.list...) {
		pt, ps, ph, ok := parentOf(e)
		want := ""
		if ok {
			want = keyOf(pt, ps, ph, "")
		}
		if e.Parent != want {
			e.Parent = want
			c.touch(e)
		}
		if !ok || s.byKey[want] != nil {
			continue
		}
		origin := e.Origin
		if e.Visited() {
			origin = OriginTraffic
		}
		s.ensureLocked(c, pt, ps, ph, "", e.TargetID, origin, e.FirstSeen)
	}
	for _, e := range c.order {
		if c.touched[e].created {
			e.Hits, e.LastSeen = s.sumLocked(e)
		}
	}
	// What was created is new to this database, but not news.
	for _, e := range c.order {
		c.touched[e].created = false
	}
}

// sumLocked totals the hits of the entities directly under e, and the
// latest they were seen.
func (s *Store) sumLocked(e *Entity) (int, time.Time) {
	hits, last := 0, e.LastSeen
	for _, child := range s.list {
		if child.Parent != e.Key {
			continue
		}
		h, l := child.Hits, child.LastSeen
		if child.Type != TypeURL {
			h, l = s.sumLocked(child)
		}
		hits += h
		if l.After(last) {
			last = l
		}
	}
	return hits, last
}

// SyncTargets makes the store hold an entity for each seed — the domain
// or address each target names — and lets go of the ones created for a
// target that no longer names them, as long as nothing else has come of
// them (no requests, nothing under them, no links, nothing in their
// key/value store). The app calls it at startup and on every change to the
// targets; Use puts the seeds in again for the next engagement.
func (s *Store) SyncTargets(seeds []Seed) error {
	s.mu.Lock()
	s.seeds = make(map[string]Seed, len(seeds))
	for _, sd := range seeds {
		if sd.Type == TypeDomain || sd.Type == TypeHost {
			s.seeds[keyOf(sd.Type, "", sd.Name, "")] = sd
		}
	}
	c := s.change()
	s.seedLocked(c)

	err := s.commitLocked(c)
	events := s.eventsLocked(c, Updated)
	// Letting a name host go can leave the domain made with it idle too.
	for {
		var gone []*Entity
		for _, e := range s.list {
			if _, ok := s.seeds[e.Key]; !ok && e.Origin == OriginTarget && s.idleLocked(e) {
				gone = append(gone, e)
			}
		}
		if len(gone) == 0 {
			break
		}
		evs, derr := s.deleteLocked(gone)
		events = append(events, evs...)
		if err == nil {
			err = derr
		}
	}
	s.mu.Unlock()
	s.emit(events...)
	return err
}

// seedLocked creates each seed that isn't in the store yet.
func (s *Store) seedLocked(c *change) {
	keys := make([]string, 0, len(s.seeds))
	for k := range s.seeds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := time.Now()
	for _, k := range keys {
		sd := s.seeds[k]
		s.ensureLocked(c, sd.Type, "", sd.Name, "", sd.TargetID, OriginTarget, now)
	}
}

// idleLocked reports whether nothing has come of an entity: no requests,
// nothing under it, no links and an empty key/value store.
func (s *Store) idleLocked(e *Entity) bool {
	if e.Hits > 0 || len(e.Links) > 0 || len(e.KV) > 0 {
		return false
	}
	for _, other := range s.list {
		if other.Parent == e.Key {
			return false
		}
	}
	return true
}

// Seeded reports whether key is an entity a target stands for.
func (s *Store) Seeded(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seeds[key]
	return ok
}

// Delete removes an entity and everything under it, and takes their hits
// off what they sat under. Traffic that reaches them again puts them back.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	e, ok := s.byKey[key]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	events, err := s.deleteLocked([]*Entity{e})
	s.mu.Unlock()
	s.emit(events...)
	return err
}

// deleteLocked removes roots and everything under them, returning the
// events: Deleted for each, children first, then Updated for each
// remaining ancestor whose hits went down.
func (s *Store) deleteLocked(roots []*Entity) ([]Event, error) {
	doomed := map[*Entity]bool{}
	var order []*Entity // parents before children
	var walk func(e *Entity)
	walk = func(e *Entity) {
		if doomed[e] {
			return
		}
		doomed[e] = true
		order = append(order, e)
		for _, child := range s.list {
			if child.Parent == e.Key {
				walk(child)
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}

	// What they sat under loses their hits.
	c := s.change()
	for _, r := range roots {
		if r.Hits == 0 {
			continue
		}
		for at := s.byKey[r.Parent]; at != nil && !doomed[at]; at = s.byKey[at.Parent] {
			at.Hits -= r.Hits
			if at.Hits < 0 {
				at.Hits = 0
			}
			c.touch(at)
		}
	}

	var err error
	if s.db != nil {
		err = s.removeLocked(order)
	}
	kept := s.list[:0]
	for _, e := range s.list {
		if !doomed[e] {
			kept = append(kept, e)
		}
	}
	s.list = kept
	for _, e := range order {
		delete(s.byKey, e.Key)
	}
	if werr := s.commitLocked(c); err == nil {
		err = werr
	}

	events := make([]Event, 0, len(order)+len(c.order))
	for i := len(order) - 1; i >= 0; i-- {
		events = append(events, Event{Kind: Deleted, Entity: order[i].Clone(), InTarget: s.inTargetLocked(order[i])})
	}
	return append(events, s.eventsLocked(c, Updated)...), err
}

// SetKV sets a key in an entity's key/value store, adding it at the end
// when it is new. An empty name is refused.
func (s *Store) SetKV(key, name, value string) (Entity, error) {
	if name == "" {
		return Entity{}, errors.New("a key/value pair needs a key")
	}
	return s.editKV(key, func(kv []Pair) ([]Pair, error) {
		for i := range kv {
			if kv[i].Key == name {
				kv[i].Value = value
				return kv, nil
			}
		}
		if len(kv) >= maxKV {
			return nil, fmt.Errorf("an entity keeps at most %d key/value pairs", maxKV)
		}
		return append(kv, Pair{name, value}), nil
	})
}

// DeleteKV removes a key from an entity's key/value store.
func (s *Store) DeleteKV(key, name string) (Entity, error) {
	return s.editKV(key, func(kv []Pair) ([]Pair, error) {
		out := kv[:0]
		for _, p := range kv {
			if p.Key != name {
				out = append(out, p)
			}
		}
		return out, nil
	})
}

// ReplaceKV replaces an entity's whole key/value store, as the UI's editor
// does. Pairs with no key are dropped, and a repeated key keeps its last
// value at its first place.
func (s *Store) ReplaceKV(key string, pairs []Pair) (Entity, error) {
	return s.editKV(key, func([]Pair) ([]Pair, error) {
		var out []Pair
		at := map[string]int{}
		for _, p := range pairs {
			if p.Key == "" {
				continue
			}
			if i, ok := at[p.Key]; ok {
				out[i].Value = p.Value
				continue
			}
			if len(out) >= maxKV {
				return nil, fmt.Errorf("an entity keeps at most %d key/value pairs", maxKV)
			}
			at[p.Key] = len(out)
			out = append(out, p)
		}
		return out, nil
	})
}

func (s *Store) editKV(key string, edit func([]Pair) ([]Pair, error)) (Entity, error) {
	s.mu.Lock()
	e, ok := s.byKey[key]
	if !ok {
		s.mu.Unlock()
		return Entity{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	kv, err := edit(append([]Pair(nil), e.KV...))
	if err != nil {
		s.mu.Unlock()
		return Entity{}, err
	}
	if slices.Equal(kv, e.KV) {
		// Nothing changed: nothing to write or tell anyone, so a plugin
		// that sets a value on every update doesn't call itself forever.
		out := e.Clone()
		s.mu.Unlock()
		return out, nil
	}
	e.KV = kv
	c := s.change()
	c.touch(e)
	err = s.commitLocked(c)
	events := s.eventsLocked(c, Updated)
	out := e.Clone()
	s.mu.Unlock()
	s.emit(events...)
	return out, err
}

// Clear empties the store, and the engagement's copy of it. The traffic is
// untouched. Each entity is announced as Deleted, then the store as
// Cleared; the targets' entities are put back at once.
func (s *Store) Clear() error {
	s.mu.Lock()
	var err error
	if s.db != nil {
		_, err = s.db.Exec(`DELETE FROM entities`) // sightings and links cascade
	}
	events := make([]Event, 0, len(s.list)+1)
	for i := len(s.list) - 1; i >= 0; i-- {
		events = append(events, Event{Kind: Deleted, Entity: s.list[i].Clone(), InTarget: s.inTargetLocked(s.list[i])})
	}
	events = append(events, Event{Kind: Cleared})
	s.list, s.byKey = nil, make(map[string]*Entity)
	s.related, s.known = map[string]bool{}, map[string]bool{}
	c := s.change()
	s.seedLocked(c)
	if werr := s.commitLocked(c); err == nil {
		err = werr
	}
	events = append(events, s.eventsLocked(c, Updated)...)
	s.mu.Unlock()

	s.emit(events...)
	return err
}

// sortByID keeps the list in ID order after a load.
func sortByID(list []*Entity) {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
}

// FlowIDs returns the flows of a URL's sightings, or of every URL under a
// host or domain, in arrival order.
func (s *Store) FlowIDs(key string) []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, ok := s.byKey[key]
	if !ok {
		return nil
	}
	var ids []uint64
	for _, e := range s.list {
		if e.Type != TypeURL {
			continue
		}
		for at := e; at != nil; at = s.byKey[at.Parent] {
			if at == root {
				ids = append(ids, e.FlowIDs()...)
				break
			}
			if at.Parent == "" {
				break
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
