package findings

import (
	"database/sql"
	"fmt"
	"time"
)

// Findings live in three tables of the open engagement's capture.db:
// findings, finding_types and finding_flows. Package archive creates them —
// it owns that database's schema and its version — and hands this package
// the connection; everything here is the queries over them.
//
// A finding's row is keyed by the same seq the Store uses as its in-memory
// ID, so a finding edited after an engagement is reopened updates its row
// rather than adding one.
//
// Note what finding_flows.flow_seq is not: a foreign key into flows. A
// finding outlives the traffic it came from — Clear Requests empties the
// flows table, and the archive only keeps the newest flows in memory — so
// each sighting carries its own method/URL snapshot and the flow reference
// is allowed to stop resolving.

// loadLocked reads every finding into memory. The caller holds the lock.
//
// It costs three queries however many findings there are, like
// archive.Recent: one for the findings, one for their types, one for their
// sightings.
func (s *Store) loadLocked() error {
	byID, order, err := s.readFindings()
	if err != nil {
		return err
	}
	if err := s.readTypes(byID); err != nil {
		return err
	}
	if err := s.readLinks(byID); err != nil {
		return err
	}

	s.list = make([]*Finding, 0, len(order))
	s.byID = make(map[uint64]*Finding, len(order))
	for _, id := range order {
		f := byID[id]
		s.list = append(s.list, f)
		s.byID[id] = f
		if id >= s.nextID {
			s.nextID = id + 1
		}
	}
	return nil
}

func (s *Store) readFindings() (map[uint64]*Finding, []uint64, error) {
	rows, err := s.db.Query(`
		SELECT seq, value, notes, fields, source, author, viewed, hidden, confidence, concern, created, updated
		FROM findings ORDER BY seq`)
	if err != nil {
		return nil, nil, fmt.Errorf("reading findings: %w", err)
	}
	defer rows.Close()

	byID := map[uint64]*Finding{}
	var order []uint64
	for rows.Next() {
		var (
			f                   Finding
			fields              string
			viewed, hidden      bool
			confidence, concern sql.NullInt64
			created, updated    int64
			source, author      string
		)
		if err := rows.Scan(&f.ID, &f.Value, &f.Notes, &fields, &source, &author,
			&viewed, &hidden, &confidence, &concern, &created, &updated); err != nil {
			return nil, nil, fmt.Errorf("reading findings: %w", err)
		}
		f.Fields = parseFields(fields)
		f.Source, f.Author, f.Viewed, f.Hidden = Source(source), author, viewed, hidden
		if confidence.Valid { // a null confidence means the finding is unreviewed
			c := int(confidence.Int64)
			f.Confidence = &c
		}
		if concern.Valid { // a null concern means it hasn't been assessed
			c := int(concern.Int64)
			f.Concern = &c
		}
		f.Created, f.Updated = time.Unix(0, created), time.Unix(0, updated)
		stored := f
		byID[f.ID] = &stored
		order = append(order, f.ID)
	}
	return byID, order, rows.Err()
}

func (s *Store) readTypes(byID map[uint64]*Finding) error {
	rows, err := s.db.Query(`SELECT seq, type_id FROM finding_types ORDER BY seq, pos`)
	if err != nil {
		return fmt.Errorf("reading finding types: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		var typeID string
		if err := rows.Scan(&id, &typeID); err != nil {
			return fmt.Errorf("reading finding types: %w", err)
		}
		if f, ok := byID[id]; ok {
			f.Types = append(f.Types, typeID)
		}
	}
	return rows.Err()
}

func (s *Store) readLinks(byID map[uint64]*Finding) error {
	rows, err := s.db.Query(`
		SELECT seq, flow_seq, method, url, location, detail, added
		FROM finding_flows ORDER BY seq, pos`)
	if err != nil {
		return fmt.Errorf("reading finding sightings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uint64
		var l Link
		var added int64
		if err := rows.Scan(&id, &l.FlowID, &l.Method, &l.URL, &l.Location, &l.Detail, &added); err != nil {
			return fmt.Errorf("reading finding sightings: %w", err)
		}
		l.Added = time.Unix(0, added)
		if f, ok := byID[id]; ok {
			f.Links = append(f.Links, l)
		}
	}
	return rows.Err()
}

// saveLocked writes one finding and its types and sightings, in a single
// transaction. The caller holds the lock.
//
// The child rows are replaced rather than diffed: a finding has a handful of
// types and sightings, and an edit is a human-paced event, so the simplest
// correct write is the right one.
func (s *Store) saveLocked(f *Finding) error {
	if s.db == nil {
		return nil // no engagement open: the finding lives for this session only
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}
	defer tx.Rollback() // no-op once committed

	fields, err := fieldsJSON(f.Fields)
	if err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}
	// A nil score is stored as NULL (unreviewed/unassessed), told apart from a
	// deliberate value; database/sql turns a nil interface into NULL.
	nullable := func(v *int) any {
		if v == nil {
			return nil
		}
		return *v
	}
	if _, err := tx.Exec(`
		INSERT INTO findings (seq, value, value_key, notes, fields, source, author, viewed, hidden, confidence, concern, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (seq) DO UPDATE SET
			value = excluded.value, value_key = excluded.value_key, notes = excluded.notes,
			fields = excluded.fields, source = excluded.source, author = excluded.author,
			viewed = excluded.viewed, hidden = excluded.hidden,
			confidence = excluded.confidence, concern = excluded.concern, updated = excluded.updated`,
		f.ID, f.Value, Draft{Value: f.Value}.Key(), f.Notes, fields, string(f.Source), f.Author,
		f.Viewed, f.Hidden, nullable(f.Confidence), nullable(f.Concern), f.Created.UnixNano(), f.Updated.UnixNano()); err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}

	if err := replaceTypes(tx, f); err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}
	if err := replaceLinks(tx, f); err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saving finding %d: %w", f.ID, err)
	}
	return nil
}

func replaceTypes(tx *sql.Tx, f *Finding) error {
	if _, err := tx.Exec(`DELETE FROM finding_types WHERE seq = ?`, f.ID); err != nil {
		return err
	}
	for i, id := range f.Types {
		if _, err := tx.Exec(`INSERT INTO finding_types (seq, type_id, pos) VALUES (?, ?, ?)`,
			f.ID, id, i); err != nil {
			return err
		}
	}
	return nil
}

func replaceLinks(tx *sql.Tx, f *Finding) error {
	if _, err := tx.Exec(`DELETE FROM finding_flows WHERE seq = ?`, f.ID); err != nil {
		return err
	}
	for i, l := range f.Links {
		if _, err := tx.Exec(`
			INSERT INTO finding_flows (seq, flow_seq, method, url, location, detail, added, pos)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, l.FlowID, l.Method, l.URL, l.Location, l.Detail, l.Added.UnixNano(), i); err != nil {
			return err
		}
	}
	return nil
}

// deleteLocked removes a finding's row; its types and sightings go with it
// (ON DELETE CASCADE). The caller holds the lock.
func (s *Store) deleteLocked(id uint64) error {
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM findings WHERE seq = ?`, id); err != nil {
		return fmt.Errorf("deleting finding %d: %w", id, err)
	}
	return nil
}
