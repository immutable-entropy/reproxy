package crawl

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// The crawl's rows live in the engagement's capture.db, in tables package
// archive creates (crawl_urls, crawl_meta). Writes are made under the
// crawler's lock, synchronously, as findings' are: they come at the
// crawl's own pace (a few a second), never from the proxy's goroutine.

const (
	metaRate    = "requests_per_second"
	metaThreads = "threads"
	metaOnlyNew = "only_new"
	// metaNoWarning is set once the user ticks "Don't show this again" on
	// the warning before a crawl starts.
	metaNoWarning = "warning_dismissed"
	metaAPIDocs   = "api_docs"
	// metaExcluded is the targets left out, a JSON array of IDs.
	metaExcluded = "excluded_targets"
	// metaDocsFound is the origins an API description was found on, a JSON
	// array.
	metaDocsFound = "docs_found"
)

func (c *Crawler) loadLocked() error {
	rows, err := c.db.Query(`SELECT url, state, depth, found_on, status, flow_seq, error, added, crawled FROM crawl_urls`)
	if err != nil {
		return fmt.Errorf("reading the crawl: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e entry
		var added, crawled int64
		if err := rows.Scan(&e.url, &e.state, &e.depth, &e.foundOn, &e.status, &e.flowID, &e.err, &added, &crawled); err != nil {
			return fmt.Errorf("reading the crawl: %w", err)
		}
		e.added = time.Unix(0, added)
		if crawled != 0 {
			e.crawled = time.Unix(0, crawled)
		}
		c.urls[e.url] = &e
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading the crawl: %w", err)
	}
	c.rebuildQueueLocked()
	c.recentFromRowsLocked()

	meta := map[string]string{}
	mrows, err := c.db.Query(`SELECT key, value FROM crawl_meta`)
	if err != nil {
		return fmt.Errorf("reading the crawl's settings: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var k, v string
		if err := mrows.Scan(&k, &v); err != nil {
			return fmt.Errorf("reading the crawl's settings: %w", err)
		}
		meta[k] = v
	}
	s := DefaultSettings()
	if v, err := strconv.ParseFloat(meta[metaRate], 64); err == nil {
		s.RequestsPerSecond = v
	}
	if v, err := strconv.Atoi(meta[metaThreads]); err == nil {
		s.Threads = v
	}
	if v, err := strconv.ParseBool(meta[metaOnlyNew]); err == nil {
		s.OnlyNew = v
	}
	if v, err := strconv.ParseBool(meta[metaAPIDocs]); err == nil {
		s.APIDocs = v
	}
	if _ = json.Unmarshal([]byte(meta[metaExcluded]), &s.ExcludedTargets); len(s.ExcludedTargets) == 0 {
		s.ExcludedTargets = nil // none left out, however it was written
	}
	var found []string
	_ = json.Unmarshal([]byte(meta[metaDocsFound]), &found)
	for _, o := range found {
		c.docsFound[o] = true
	}
	// Saved on a machine with more CPUs, or edited by hand: keep within
	// this one's limits.
	s.RequestsPerSecond = min(max(s.RequestsPerSecond, MinRequestsPerSecond), MaxRequestsPerSecond)
	s.Threads = min(max(s.Threads, 1), MaxThreads())
	c.settings = s
	c.warningDismissed = meta[metaNoWarning] == "true"
	return mrows.Err()
}

func (c *Crawler) saveSettingsLocked() error {
	if c.db == nil {
		return nil
	}
	excluded, _ := json.Marshal(append([]string{}, c.settings.ExcludedTargets...))
	found := make([]string, 0, len(c.docsFound))
	for o := range c.docsFound {
		found = append(found, o)
	}
	sort.Strings(found)
	docs, _ := json.Marshal(found)
	return c.inTx(func(tx *sql.Tx) error {
		for k, v := range map[string]string{
			metaRate:      strconv.FormatFloat(c.settings.RequestsPerSecond, 'g', -1, 64),
			metaThreads:   strconv.Itoa(c.settings.Threads),
			metaOnlyNew:   strconv.FormatBool(c.settings.OnlyNew),
			metaNoWarning: strconv.FormatBool(c.warningDismissed),
			metaAPIDocs:   strconv.FormatBool(c.settings.APIDocs),
			metaExcluded:  string(excluded),
			metaDocsFound: string(docs),
		} {
			if _, err := tx.Exec(`INSERT INTO crawl_meta (key, value) VALUES (?, ?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// writeLocked upserts the given entries in one transaction.
func (c *Crawler) writeLocked(list ...*entry) error {
	if c.db == nil || len(list) == 0 {
		return nil
	}
	return c.inTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO crawl_urls (url, state, depth, found_on, status, flow_seq, error, added, crawled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (url) DO UPDATE SET state = excluded.state, status = excluded.status,
				flow_seq = excluded.flow_seq, error = excluded.error, crawled = excluded.crawled`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range list {
			var crawled int64
			if !e.crawled.IsZero() {
				crawled = e.crawled.UnixNano()
			}
			if _, err := stmt.Exec(e.url, e.state, e.depth, e.foundOn, e.status, e.flowID, e.err, e.added.UnixNano(), crawled); err != nil {
				return err
			}
		}
		return nil
	})
}

// deleteLocked removes the given entries' rows.
func (c *Crawler) deleteLocked(list ...*entry) error {
	if c.db == nil || len(list) == 0 {
		return nil
	}
	return c.inTx(func(tx *sql.Tx) error {
		for _, e := range list {
			if _, err := tx.Exec(`DELETE FROM crawl_urls WHERE url = ?`, e.url); err != nil {
				return err
			}
		}
		return nil
	})
}

// inTx runs fn in a transaction, remembering a failure for Status.
func (c *Crawler) inTx(fn func(*sql.Tx) error) error {
	err := func() error {
		tx, err := c.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback() // no-op once committed
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		err = fmt.Errorf("recording the crawl: %w", err)
		c.writeErr = err
	}
	return err
}

// recentFromRowsLocked fills the activity list from what earlier sessions
// crawled, latest last.
func (c *Crawler) recentFromRowsLocked() {
	var done []*entry
	for _, e := range c.urls {
		if e.state == Crawled || e.state == Failed {
			done = append(done, e)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].crawled.Before(done[j].crawled) })
	if len(done) > maxRecent {
		done = done[len(done)-maxRecent:]
	}
	c.recent = c.recent[:0]
	for _, e := range done {
		c.recent = append(c.recent, Result{URL: e.url, State: e.state, Status: e.status, Error: e.err,
			FlowID: e.flowID, FoundOn: e.foundOn, At: e.crawled, Links: -1})
	}
}

// sortEntries orders entries by depth, then by when they were found.
func sortEntries(list []*entry) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if !a.added.Equal(b.added) {
			return a.added.Before(b.added)
		}
		return a.url < b.url
	})
}
