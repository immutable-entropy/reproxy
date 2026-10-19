package archive

import (
	"database/sql"
	"time"

	"reproxy/internal/capture"
)

// Record queues a store event for writing. It is the capture.Store
// subscriber, so it runs on the proxy's goroutine: it takes one uncontended
// lock, stores the flow (whose message bytes are shared with the store, not
// copied) and returns. No I/O, no blocking, nothing dropped.
//
// Recording the same flow again before the batch is written replaces the
// queued copy, so a request that is added, held, forwarded and completed
// inside one window costs a single row write.
func (a *Archive) Record(e capture.Event) {
	a.mu.Lock()
	switch e.Kind {
	case capture.FlowsCleared:
		// Everything queued belongs to what was just cleared.
		a.pending = make(map[uint64]capture.Flow)
		a.order = a.order[:0]
		a.wsPending = nil
		a.seq = make(map[uint64]int64)
		a.wipe = true
	case capture.FlowsReset:
		// The store was reloaded from an archive; those flows are already
		// recorded. Adopt their rows so later edits update them in place.
		a.pending = make(map[uint64]capture.Flow)
		a.order = a.order[:0]
		a.wsPending = nil
		a.seq = make(map[uint64]int64, len(e.Flows))
		for _, f := range e.Flows {
			a.seq[f.ID] = int64(f.ID)
		}
	case capture.WSMessageAdded:
		// A WebSocket message doesn't change its flow's row; queue it apart.
		a.wsPending = append(a.wsPending, wsRow{flowID: e.Flow.ID, msg: e.Message})
	default:
		if _, queued := a.pending[e.Flow.ID]; !queued {
			a.order = append(a.order, e.Flow.ID)
		}
		a.pending[e.Flow.ID] = e.Flow
	}
	a.mu.Unlock()

	select {
	case a.wake <- struct{}{}:
	default: // the writer is already awake
	}
}

// Flush waits for everything recorded so far to reach the database. The UI
// never needs it; tests and Close do.
func (a *Archive) Flush() error {
	a.mu.Lock()
	a.flushing = true
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.pending) > 0 || len(a.wsPending) > 0 || a.wipe || a.writing {
		a.cond.Wait()
	}
	return a.lastErr
}

// Err returns the last write error, or nil. A failed batch is dropped rather
// than retried forever — the flows are still in the store, and a proxy must
// not stall because a disk filled up — so this is how the app notices.
func (a *Archive) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// run is the writer. It wakes on a tick, on a queue long enough to be worth
// writing early, and on Flush.
func (a *Archive) run() {
	defer close(a.done)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.writeBatch()
		case <-a.wake:
			if a.ready() {
				a.writeBatch()
			}
		case <-a.quit:
			a.writeBatch()
			return
		}
	}
}

// ready reports whether the queue is worth writing before the next tick.
func (a *Archive) ready() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flushing || a.wipe || len(a.pending) >= flushThreshold || len(a.wsPending) >= flushThreshold
}

// writeBatch takes everything queued and applies it in one transaction.
func (a *Archive) writeBatch() {
	a.mu.Lock()
	flows, wipe := a.takeLocked()
	wsRows := a.wsPending
	a.wsPending = nil
	if len(flows) == 0 && len(wsRows) == 0 && !wipe {
		a.mu.Unlock()
		a.finish(nil)
		return
	}
	// The batch has left the queue but isn't committed: Flush waits on this
	// rather than on an empty queue, which it would otherwise mistake for a
	// finished write.
	a.writing = true
	a.mu.Unlock()

	seqs, err := a.apply(flows, wipe, wsRows)

	a.mu.Lock()
	if wipe {
		// Rows learned by a batch that was in flight when the store was
		// cleared point at rows this batch has just deleted.
		a.seq = make(map[uint64]int64, len(seqs))
	}
	for id, seq := range seqs {
		a.seq[id] = seq
	}
	a.mu.Unlock()
	a.finish(err)
}

// takeLocked empties the queue and returns its contents in arrival order.
func (a *Archive) takeLocked() ([]capture.Flow, bool) {
	wipe := a.wipe
	a.wipe = false
	if len(a.pending) == 0 {
		return nil, wipe
	}
	flows := make([]capture.Flow, 0, len(a.pending))
	for _, id := range a.order {
		if f, ok := a.pending[id]; ok {
			flows = append(flows, f)
		}
	}
	a.pending = make(map[uint64]capture.Flow)
	a.order = a.order[:0]
	return flows, wipe
}

// finish records the batch's outcome and releases anyone in Flush.
func (a *Archive) finish(err error) {
	a.mu.Lock()
	if err != nil {
		a.lastErr = err
	}
	a.flushing, a.writing = false, false
	a.cond.Broadcast()
	a.mu.Unlock()
}

// apply writes one batch. It returns the row each flow now occupies, so the
// next change to the same flow updates that row instead of inserting again.
func (a *Archive) apply(flows []capture.Flow, wipe bool, wsRows []wsRow) (map[uint64]int64, error) {
	a.mu.Lock()
	known := make(map[uint64]int64, len(flows))
	for _, f := range flows {
		if seq, ok := a.seq[f.ID]; ok {
			known[f.ID] = seq
		}
	}
	a.mu.Unlock()

	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op once committed

	if wipe {
		// The messages go with their flows (ON DELETE CASCADE).
		if _, err := tx.Exec(`DELETE FROM flows`); err != nil {
			return nil, err
		}
		known = nil
	}

	insert, err := tx.Prepare(`
		INSERT INTO flows (state, client_addr, dest, method, host, path, secure,
			target_id, status_code, status_text, started, duration, error, source, origin, protocol)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insert.Close()
	update, err := tx.Prepare(`
		UPDATE flows SET state = ?, client_addr = ?, dest = ?, method = ?, host = ?,
			path = ?, secure = ?, target_id = ?, status_code = ?, status_text = ?,
			started = ?, duration = ?, error = ?, source = ?, origin = ?, protocol = ? WHERE seq = ?`)
	if err != nil {
		return nil, err
	}
	defer update.Close()
	message, err := tx.Prepare(`
		INSERT INTO messages (seq, kind, size, truncated, raw) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (seq, kind) DO UPDATE SET
			size = excluded.size, truncated = excluded.truncated, raw = excluded.raw`)
	if err != nil {
		return nil, err
	}
	defer message.Close()

	written := make(map[uint64]int64, len(flows))
	for _, f := range flows {
		seq, err := writeFlow(insert, update, known[f.ID], f)
		if err != nil {
			return nil, err
		}
		written[f.ID] = seq
		if err := writeMessages(message, seq, f); err != nil {
			return nil, err
		}
	}
	if len(wsRows) > 0 {
		ws, err := tx.Prepare(`
			INSERT OR IGNORE INTO websocket_messages
				(flow_seq, seq, dir, type, data, truncated, edited, dropped, original, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return nil, err
		}
		defer ws.Close()
		for _, r := range wsRows {
			seq, ok := written[r.flowID]
			if !ok {
				seq, ok = known[r.flowID]
			}
			if !ok {
				seq, ok = a.rowOf(r.flowID)
			}
			if !ok {
				continue // its flow isn't in the archive; drop the message
			}
			m := r.msg
			if _, err := ws.Exec(seq, m.Seq, int(m.Dir), int(m.Type), m.Data,
				m.Truncated, m.Edited, m.Dropped, m.Original, m.At.UnixNano()); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return written, nil
}

// rowOf returns a flow's row from the seq map, under the lock.
func (a *Archive) rowOf(flowID uint64) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	seq, ok := a.seq[flowID]
	return seq, ok
}

// writeFlow inserts or updates one flow's metadata and returns its row.
func writeFlow(insert, update *sql.Stmt, seq int64, f capture.Flow) (int64, error) {
	args := []any{
		int(f.State), f.ClientAddr, f.Target, f.Method, f.Host, f.Path, f.Secure,
		f.TargetID, f.StatusCode, f.StatusText, f.Started.UnixNano(), int64(f.Duration), f.Error,
		string(f.Source), f.Origin, string(f.Protocol),
	}
	if seq != 0 {
		if _, err := update.Exec(append(args, seq)...); err != nil {
			return 0, err
		}
		return seq, nil
	}
	res, err := insert.Exec(args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// writeMessages stores the messages a flow actually has. An absent one (no
// response yet, or no edit) is left out rather than stored empty, so the
// response arriving later is the only write it costs.
func writeMessages(stmt *sql.Stmt, seq int64, f capture.Flow) error {
	put := func(kind messageKind, m capture.Message) error {
		if len(m.Raw) == 0 && m.Size == 0 {
			return nil
		}
		raw := m.Raw
		if raw == nil {
			raw = []byte{} // the column is NOT NULL; a nil slice would be NULL
		}
		_, err := stmt.Exec(seq, int(kind), m.Size, m.Truncated, raw)
		return err
	}
	if err := put(msgRequest, f.Request); err != nil {
		return err
	}
	if err := put(msgResponse, f.Response); err != nil {
		return err
	}
	if f.OriginalRequest != nil {
		if err := put(msgOriginalRequest, *f.OriginalRequest); err != nil {
			return err
		}
	}
	if f.OriginalResponse != nil {
		if err := put(msgOriginalResponse, *f.OriginalResponse); err != nil {
			return err
		}
	}
	return nil
}
