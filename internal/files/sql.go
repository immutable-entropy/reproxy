package files

import (
	"fmt"
	"time"
)

// Recovered files live in one table of the open engagement's capture.db:
// files. Package archive creates it — it owns that database's schema and
// its version — and hands this package the connection; everything here is
// the queries over it.
//
// A file's row is keyed by the same seq the Store uses as its in-memory ID,
// so a file recovered again after an engagement is reopened updates its row
// rather than adding one, and its path is UNIQUE, which is the dedup rule
// written down where the database can enforce it.
//
// Note what files.flow_seq is not: a foreign key into flows. A recovered
// file outlives the traffic it came from — Clear Requests empties the flows
// table — so each file carries its own method/URL snapshot and the flow
// reference is allowed to stop resolving, exactly as a finding's sighting
// does.

// loadLocked reads every file's record into memory. The caller holds the
// lock. The bytes are not read: they stay on disk until someone asks for
// them (Store.Read).
func (s *Store) loadLocked() error {
	rows, err := s.db.Query(`
		SELECT seq, path, bytes, sha256, media_type, kind, source, author,
		       batch, batch_label, notes, flow_seq, method, url, host, entity,
		       created, updated
		FROM files ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("reading the recovered files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		f := &File{}
		var source string
		var created, updated int64
		if err := rows.Scan(&f.ID, &f.Path, &f.Size, &f.SHA256, &f.MediaType, &f.Kind,
			&source, &f.Author, &f.Batch, &f.BatchLabel, &f.Notes, &f.FlowID,
			&f.Method, &f.URL, &f.Host, &f.Entity, &created, &updated); err != nil {
			return fmt.Errorf("reading the recovered files: %w", err)
		}
		f.Source = Source(source)
		f.Created, f.Updated = time.Unix(0, created), time.Unix(0, updated)
		s.list = append(s.list, f)
		s.byID[f.ID] = f
		s.byPath[f.Path] = f
		if f.ID >= s.nextID {
			s.nextID = f.ID + 1
		}
	}
	return rows.Err()
}

// saveLocked writes one file's record. The caller holds the lock.
func (s *Store) saveLocked(f *File) error {
	if s.db == nil {
		return nil // no engagement open: the record lives for this session only
	}
	if _, err := s.db.Exec(`
		INSERT INTO files (seq, path, bytes, sha256, media_type, kind, source, author,
		                   batch, batch_label, notes, flow_seq, method, url, host, entity,
		                   created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (seq) DO UPDATE SET
			path = excluded.path, bytes = excluded.bytes, sha256 = excluded.sha256,
			media_type = excluded.media_type, kind = excluded.kind,
			source = excluded.source, author = excluded.author,
			batch = excluded.batch, batch_label = excluded.batch_label,
			notes = excluded.notes, flow_seq = excluded.flow_seq,
			method = excluded.method, url = excluded.url, host = excluded.host,
			entity = excluded.entity, updated = excluded.updated`,
		f.ID, f.Path, f.Size, f.SHA256, f.MediaType, f.Kind, string(f.Source), f.Author,
		f.Batch, f.BatchLabel, f.Notes, f.FlowID, f.Method, f.URL, f.Host, f.Entity,
		f.Created.UnixNano(), f.Updated.UnixNano()); err != nil {
		return fmt.Errorf("saving the recovered file %s: %w", f.Path, err)
	}
	return nil
}

// deleteLocked removes one file's row. The caller holds the lock.
func (s *Store) deleteLocked(id uint64) error {
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM files WHERE seq = ?`, id); err != nil {
		return fmt.Errorf("deleting the recovered file %d: %w", id, err)
	}
	return nil
}
