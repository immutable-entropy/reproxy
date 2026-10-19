package entities

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Entities live in three tables of the open engagement's capture.db:
// entities, entity_flows (a URL's sightings) and entity_links. Package
// archive creates them — it owns that database's schema and its version —
// and hands this package the connection.
//
// entities.key is UNIQUE, and every write is an upsert on it, so an entity
// is one row however many writers merge into it. Each row's attrs column
// holds the attributes of its type (for a URL, the parts only ever read
// whole: operations, path parameters, examples, kinds) as JSON, and kv its
// key/value store.

// loadLocked reads every entity into memory. The caller holds the lock.
func (s *Store) loadLocked() error {
	rows, err := s.db.Query(`
		SELECT seq, key, type, parent, scheme, host, path, target_id, origin, attrs, kv, hits, first_seen, last_seen
		FROM entities ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("reading entities: %w", err)
	}
	byID := map[uint64]*Entity{}
	for rows.Next() {
		var (
			e           Entity
			attrs, kv   string
			first, last int64
		)
		if err := rows.Scan(&e.ID, &e.Key, &e.Type, &e.Parent, &e.Scheme, &e.Host, &e.Path, &e.TargetID,
			&e.Origin, &attrs, &kv, &e.Hits, &first, &last); err != nil {
			rows.Close()
			return fmt.Errorf("reading entities: %w", err)
		}
		if !e.Type.Valid() {
			continue // written by a newer version: leave it be
		}
		fresh := newEntity(e.Type, e.Scheme, e.Host, e.Path, "", "")
		e.DomainAttrs, e.HostAttrs, e.URLAttrs = fresh.DomainAttrs, fresh.HostAttrs, fresh.URLAttrs
		if e.Parent == "" {
			e.Parent = fresh.Parent
		}
		_ = json.Unmarshal([]byte(attrs), e.attrs())
		_ = json.Unmarshal([]byte(kv), &e.KV)
		e.FirstSeen, e.LastSeen = time.Unix(0, first), time.Unix(0, last)
		stored := e
		s.list = append(s.list, &stored)
		s.byKey[e.Key] = &stored
		byID[e.ID] = &stored
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading entities: %w", err)
	}
	sortByID(s.list)

	rows, err = s.db.Query(`SELECT entity_seq, flow_seq, method, status, url, seen
		FROM entity_flows ORDER BY entity_seq, seen, flow_seq`)
	if err != nil {
		return fmt.Errorf("reading URL sightings: %w", err)
	}
	for rows.Next() {
		var id uint64
		var sg Sighting
		var seen int64
		if err := rows.Scan(&id, &sg.FlowID, &sg.Method, &sg.Status, &sg.URL, &seen); err != nil {
			rows.Close()
			return fmt.Errorf("reading URL sightings: %w", err)
		}
		sg.Seen = time.Unix(0, seen)
		if e := byID[id]; e != nil && e.URLAttrs != nil {
			e.URLAttrs.Sightings = append(e.URLAttrs.Sightings, sg)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading URL sightings: %w", err)
	}

	rows, err = s.db.Query(`SELECT entity_seq, source, from_url, from_flow_seq, detail, added
		FROM entity_links ORDER BY entity_seq, added`)
	if err != nil {
		return fmt.Errorf("reading entity links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		var l Link
		var added int64
		if err := rows.Scan(&id, &l.Source, &l.FromURL, &l.FromFlowID, &l.Detail, &added); err != nil {
			return fmt.Errorf("reading entity links: %w", err)
		}
		l.Added = time.Unix(0, added)
		if e := byID[id]; e != nil {
			e.Links = append(e.Links, l)
		}
	}
	return rows.Err()
}

// attrs is the entity's attributes, for the attrs column.
func (e *Entity) attrs() any {
	switch {
	case e.DomainAttrs != nil:
		return e.DomainAttrs
	case e.HostAttrs != nil:
		return e.HostAttrs
	case e.URLAttrs != nil:
		return e.URLAttrs
	}
	return &struct{}{}
}

// writeLocked writes the entities a change touched, in one transaction.
// The caller holds the lock. A new entity takes the row the upsert returns
// as its ID — which, if another process put the same key there first, is
// that row: the two merge rather than duplicate.
func (s *Store) writeLocked(c *change) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("writing entities: %w", err)
	}
	defer tx.Rollback() // no-op once committed

	upsert, err := tx.Prepare(`
		INSERT INTO entities (key, type, parent, scheme, host, path, target_id, visited, origin, attrs, kv, hits, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			parent = excluded.parent, visited = excluded.visited, attrs = excluded.attrs, kv = excluded.kv,
			hits = excluded.hits, last_seen = max(entities.last_seen, excluded.last_seen)
		RETURNING seq`)
	if err != nil {
		return fmt.Errorf("writing entities: %w", err)
	}
	defer upsert.Close()
	sighting, err := tx.Prepare(`
		INSERT OR IGNORE INTO entity_flows (entity_seq, flow_seq, method, status, url, seen)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("writing entities: %w", err)
	}
	defer sighting.Close()
	link, err := tx.Prepare(`
		INSERT OR IGNORE INTO entity_links (entity_seq, source, from_url, from_flow_seq, detail, added)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("writing entities: %w", err)
	}
	defer link.Close()

	ids := make(map[*Entity]uint64, len(c.order))
	for _, e := range c.order {
		attrs, err := json.Marshal(e.attrs())
		if err != nil {
			return fmt.Errorf("writing entity %s: %w", e.Key, err)
		}
		kv := []byte("[]")
		if len(e.KV) > 0 {
			if kv, err = json.Marshal(e.KV); err != nil {
				return fmt.Errorf("writing entity %s: %w", e.Key, err)
			}
		}
		var id uint64
		if err := upsert.QueryRow(e.Key, string(e.Type), e.Parent, e.Scheme, e.Host, e.Path, e.TargetID,
			e.Visited(), e.Origin, string(attrs), string(kv), e.Hits,
			e.FirstSeen.UnixNano(), e.LastSeen.UnixNano()).Scan(&id); err != nil {
			return fmt.Errorf("writing entity %s: %w", e.Key, err)
		}
		ids[e] = id
		p := c.touched[e]
		for _, sg := range p.sightings {
			if _, err := sighting.Exec(id, sg.FlowID, sg.Method, sg.Status, sg.URL, sg.Seen.UnixNano()); err != nil {
				return fmt.Errorf("writing entity %s: %w", e.Key, err)
			}
		}
		if len(p.sightings) > 0 && len(e.URLAttrs.Sightings) >= maxSightings {
			// Keep the table to what memory holds: the newest sightings.
			if _, err := tx.Exec(`DELETE FROM entity_flows WHERE entity_seq = ? AND flow_seq NOT IN (
				SELECT flow_seq FROM entity_flows WHERE entity_seq = ?
				ORDER BY seen DESC, flow_seq DESC LIMIT ?)`, id, id, maxSightings); err != nil {
				return fmt.Errorf("writing entity %s: %w", e.Key, err)
			}
		}
		for _, l := range p.links {
			if _, err := link.Exec(id, l.Source, l.FromURL, l.FromFlowID, l.Detail, l.Added.UnixNano()); err != nil {
				return fmt.Errorf("writing entity %s: %w", e.Key, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing entities: %w", err)
	}
	for e, id := range ids {
		e.ID = id
	}
	return nil
}

// removeLocked deletes the entities' rows (their sightings and links
// cascade). The caller holds the lock.
func (s *Store) removeLocked(list []*Entity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("deleting entities: %w", err)
	}
	defer tx.Rollback() // no-op once committed
	const chunk = 500
	for start := 0; start < len(list); start += chunk {
		end := min(start+chunk, len(list))
		args := make([]any, 0, end-start)
		for _, e := range list[start:end] {
			args = append(args, e.Key)
		}
		q := `DELETE FROM entities WHERE key IN (?` + strings.Repeat(",?", len(args)-1) + `)`
		if _, err := tx.Exec(q, args...); err != nil {
			return fmt.Errorf("deleting entities: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("deleting entities: %w", err)
	}
	return nil
}

// NeedsBackfill reports whether the engagement's archived traffic has yet
// to be indexed: true once, after the upgrade that created the store.
func (s *Store) NeedsBackfill() bool {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return false
	}
	var v string
	err := db.QueryRow(`SELECT value FROM entity_meta WHERE key = 'backfill'`).Scan(&v)
	return err == nil && v == "1"
}

// BackfillDone records that the archived traffic has been indexed.
func (s *Store) BackfillDone(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM entity_meta WHERE key = 'backfill'`)
	return err
}
