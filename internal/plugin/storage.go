package plugin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"reproxy/internal/atomicfile"
)

// maxStorage caps a plugin's own storage file.
const maxStorage = 1 << 20

// kv is a plugin's storage: its own (a JSON file in the data directory) or
// the open engagement's (a table in its capture.db).
type kv interface {
	get(key string) (json.RawMessage, bool, error)
	set(key string, value json.RawMessage) error
	delete(key string) error
	keys() ([]string, error)
}

// store returns the storage for a scope: "global" or "engagement".
func (r *Running) store(scope string) kv {
	if scope == "engagement" {
		return &engagementStorage{plugin: r.src.Name, db: r.deps.EngagementDB}
	}
	if r.storage == nil {
		r.storage = &fileStorage{path: filepath.Join(r.deps.DataDir, "plugin-data", r.src.Name, "storage.json")}
	}
	return r.storage
}

// fileStorage is a plugin's own storage: one JSON object in
// <data dir>/plugin-data/<name>/storage.json, read on first use and
// rewritten whole (atomically) on every change.
type fileStorage struct {
	path   string
	mu     sync.Mutex
	loaded bool
	values map[string]json.RawMessage
}

func (s *fileStorage) load() error {
	if s.loaded {
		return nil
	}
	s.values = map[string]json.RawMessage{}
	if s.path == "" {
		s.loaded = true
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.values); err != nil {
			return fmt.Errorf("reading %s: %w", s.path, err)
		}
	}
	s.loaded = true
	return nil
}

func (s *fileStorage) get(key string) (json.RawMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, false, err
	}
	v, ok := s.values[key]
	return v, ok, nil
}

func (s *fileStorage) set(key string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	prev, had := s.values[key]
	s.values[key] = value
	if err := s.save(); err != nil {
		if had {
			s.values[key] = prev
		} else {
			delete(s.values, key)
		}
		return err
	}
	return nil
}

func (s *fileStorage) delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if _, ok := s.values[key]; !ok {
		return nil
	}
	delete(s.values, key)
	return s.save()
}

func (s *fileStorage) keys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (s *fileStorage) save() error {
	data, err := json.Marshal(s.values)
	if err != nil {
		return err
	}
	if len(data) > maxStorage {
		return fmt.Errorf("storage is limited to %d bytes", maxStorage)
	}
	// A plugin's storage holds whatever it put there, which for a plugin
	// that harvests credentials is those credentials.
	return atomicfile.Write(s.path, data, atomicfile.Private, 0o755)
}

// engagementStorage is a plugin's storage in the open engagement: the
// plugin_kv table of its capture.db (see package archive).
type engagementStorage struct {
	plugin string
	db     func() *sql.DB
}

var errNoEngagement = errors.New("E_NO_ENGAGEMENT: no engagement is open")

func (s *engagementStorage) conn() (*sql.DB, error) {
	if s.db == nil {
		return nil, errNoEngagement
	}
	db := s.db()
	if db == nil {
		return nil, errNoEngagement
	}
	return db, nil
}

func (s *engagementStorage) get(key string) (json.RawMessage, bool, error) {
	db, err := s.conn()
	if err != nil {
		return nil, false, err
	}
	var v string
	err = db.QueryRow(`SELECT value FROM plugin_kv WHERE plugin = ? AND key = ?`, s.plugin, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return json.RawMessage(v), err == nil, err
}

func (s *engagementStorage) set(key string, value json.RawMessage) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO plugin_kv (plugin, key, value, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (plugin, key) DO UPDATE SET value = excluded.value, updated = excluded.updated`,
		s.plugin, key, string(value), time.Now().UnixNano())
	return err
}

func (s *engagementStorage) delete(key string) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM plugin_kv WHERE plugin = ? AND key = ?`, s.plugin, key)
	return err
}

func (s *engagementStorage) keys() ([]string, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT key FROM plugin_kv WHERE plugin = ? ORDER BY key`, s.plugin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
