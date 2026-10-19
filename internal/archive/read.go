package archive

import (
	"database/sql"
	"fmt"
	"time"

	"reproxy/internal/capture"
)

// Defaults for Recent. Reopening an engagement must not try to pull a year of
// traffic into memory, so the newest flows are loaded up to both a count and
// a byte budget — whichever runs out first. What is left behind stays in the
// archive: it is still on disk, still queryable, and Flow fetches any of it.
//
// The count is what the load costs: around 40µs a flow, so this is about a
// tenth of a second between asking for an engagement and seeing it.
const (
	DefaultLimit    = 2000
	DefaultMaxBytes = 128 << 20 // 128 MiB of message bodies
)

// LoadOptions bounds a Recent call. A zero value means the defaults.
type LoadOptions struct {
	// Limit is the most flows to return; 0 means DefaultLimit and a
	// negative limit means no cap.
	Limit int
	// MaxBytes caps the message bodies loaded; 0 means DefaultMaxBytes and
	// a negative value means no cap. Every flow returned keeps its
	// metadata, including each message's size — only the bodies stop once
	// the budget is spent, so the list still shows what it always showed and
	// an older body can be fetched with Flow when the user opens it.
	MaxBytes int64
}

func (o LoadOptions) limit() int {
	switch {
	case o.Limit == 0:
		return DefaultLimit
	case o.Limit < 0:
		return -1
	}
	return o.Limit
}

func (o LoadOptions) maxBytes() int64 {
	switch {
	case o.MaxBytes == 0:
		return DefaultMaxBytes
	case o.MaxBytes < 0:
		return -1
	}
	return o.MaxBytes
}

// flowColumns is the metadata every read selects, in scanFlow's order.
const flowColumns = `seq, state, client_addr, dest, method, host, path, secure,
	target_id, status_code, status_text, started, duration, error, source, origin, protocol`

// Count returns how many flows the archive holds.
func (a *Archive) Count() (int64, error) {
	var n int64
	err := a.db.QueryRow(`SELECT count(*) FROM flows`).Scan(&n)
	return n, err
}

// Recent returns the newest flows in arrival order, ready to seed a
// capture.Store. Each flow's ID is its row, so the store that holds them and
// the archive that wrote them agree on what a flow is: an edit to a loaded
// flow updates its row rather than adding another.
//
// It costs three queries however many flows are loaded — metadata, message
// sizes, then the bodies that fit the budget — rather than one per flow.
func (a *Archive) Recent(opts LoadOptions) ([]capture.Flow, error) {
	flows, err := a.recentFlows(opts.limit())
	if err != nil || len(flows) == 0 {
		return nil, err
	}

	// The query returned the newest first, so the oldest loaded row is last.
	oldest, newest := int64(flows[len(flows)-1].ID), int64(flows[0].ID)
	byRow := make(map[int64]*capture.Flow, len(flows))
	for i := range flows {
		byRow[int64(flows[i].ID)] = &flows[i]
	}

	stored, err := a.fillMessageSizes(byRow, oldest, newest)
	if err != nil {
		return nil, err
	}
	// Bodies are filled newest first, so a budget that runs out leaves the
	// traffic the user is most likely to open.
	if cutoff := bodyCutoff(flows, stored, opts.maxBytes()); cutoff <= newest {
		if err := a.fillMessageBodies(byRow, cutoff, newest); err != nil {
			return nil, err
		}
	}

	if err := a.fillWSMessages(byRow, oldest, newest); err != nil {
		return nil, err
	}
	for i, j := 0, len(flows)-1; i < j; i, j = i+1, j-1 {
		flows[i], flows[j] = flows[j], flows[i]
	}
	return flows, nil
}

// fillWSMessages loads the WebSocket messages of the flows in byRow, keyed by
// their row, in order. Most flows have none, so one range query covers them.
func (a *Archive) fillWSMessages(byRow map[int64]*capture.Flow, oldest, newest int64) error {
	rows, err := a.db.Query(`SELECT flow_seq, seq, dir, type, data, truncated, edited, dropped, original, at
		FROM websocket_messages WHERE flow_seq BETWEEN ? AND ? ORDER BY flow_seq, seq`, oldest, newest)
	if err != nil {
		return fmt.Errorf("reading websocket messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			seq  int64
			m    capture.WSMessage
			dir  int
			typ  int
			orig []byte
			at   int64
		)
		if err := rows.Scan(&seq, &m.Seq, &dir, &typ, &m.Data, &m.Truncated, &m.Edited, &m.Dropped, &orig, &at); err != nil {
			return fmt.Errorf("reading websocket messages: %w", err)
		}
		m.Dir, m.Type, m.Original, m.At = capture.WSDirection(dir), capture.WSType(typ), orig, time.Unix(0, at)
		if f, ok := byRow[seq]; ok {
			f.Messages = append(f.Messages, m)
		}
	}
	return rows.Err()
}

// recentFlows reads the newest rows' metadata, newest first.
func (a *Archive) recentFlows(limit int) ([]capture.Flow, error) {
	query := `SELECT ` + flowColumns + ` FROM flows ORDER BY seq DESC`
	var args []any
	if limit >= 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading flows: %w", err)
	}
	defer rows.Close()

	var flows []capture.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, fmt.Errorf("reading flows: %w", err)
		}
		flows = append(flows, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading flows: %w", err)
	}
	return flows, nil
}

// fillMessageSizes gives every loaded flow its messages' sizes without
// reading a single body, and returns how many stored bytes each flow's
// bodies would cost. Rows are keyed by seq, and the flows loaded are exactly
// those from oldest to newest, so one range scan covers them.
func (a *Archive) fillMessageSizes(byRow map[int64]*capture.Flow, oldest, newest int64) (map[int64]int64, error) {
	rows, err := a.db.Query(`SELECT seq, kind, size, truncated, length(raw)
		FROM messages WHERE seq BETWEEN ? AND ?`, oldest, newest)
	if err != nil {
		return nil, fmt.Errorf("reading message sizes: %w", err)
	}
	defer rows.Close()

	stored := make(map[int64]int64, len(byRow))
	for rows.Next() {
		var seq, length int64
		var kind int
		var m capture.Message
		if err := rows.Scan(&seq, &kind, &m.Size, &m.Truncated, &length); err != nil {
			return nil, fmt.Errorf("reading message sizes: %w", err)
		}
		f, ok := byRow[seq]
		if !ok {
			continue
		}
		if dst := messageOf(f, messageKind(kind)); dst != nil {
			dst.Size, dst.Truncated = m.Size, m.Truncated
		}
		stored[seq] += length
	}
	return stored, rows.Err()
}

// bodyCutoff is the oldest row whose bodies fit the budget. flows is newest
// first.
func bodyCutoff(flows []capture.Flow, stored map[int64]int64, budget int64) int64 {
	if budget < 0 {
		return int64(flows[len(flows)-1].ID) // no cap: every loaded flow
	}
	cutoff := int64(flows[0].ID) + 1 // nothing fits until one does
	for _, f := range flows {
		seq := int64(f.ID)
		if budget -= stored[seq]; budget < 0 {
			break
		}
		cutoff = seq
	}
	return cutoff
}

// fillMessageBodies reads the bodies of the rows from cutoff to newest.
func (a *Archive) fillMessageBodies(byRow map[int64]*capture.Flow, cutoff, newest int64) error {
	rows, err := a.db.Query(`SELECT seq, kind, raw FROM messages WHERE seq BETWEEN ? AND ?`,
		cutoff, newest)
	if err != nil {
		return fmt.Errorf("reading message bodies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var seq int64
		var kind int
		var raw []byte
		if err := rows.Scan(&seq, &kind, &raw); err != nil {
			return fmt.Errorf("reading message bodies: %w", err)
		}
		f, ok := byRow[seq]
		if !ok {
			continue
		}
		if dst := messageOf(f, messageKind(kind)); dst != nil {
			dst.Raw = raw
		}
	}
	return rows.Err()
}

// Flow returns one flow with its messages, by the ID Recent gave it (its
// row). It is how a body left behind by Recent's byte budget is fetched when
// the user opens that request.
func (a *Archive) Flow(id uint64) (capture.Flow, bool, error) {
	row := a.db.QueryRow(`SELECT `+flowColumns+` FROM flows WHERE seq = ?`, int64(id))
	f, err := scanFlow(row)
	if err == sql.ErrNoRows {
		return capture.Flow{}, false, nil
	}
	if err != nil {
		return capture.Flow{}, false, fmt.Errorf("reading flow %d: %w", id, err)
	}
	byRow := map[int64]*capture.Flow{int64(id): &f}
	if _, err := a.fillMessageSizes(byRow, int64(id), int64(id)); err != nil {
		return capture.Flow{}, false, err
	}
	if err := a.fillMessageBodies(byRow, int64(id), int64(id)); err != nil {
		return capture.Flow{}, false, err
	}
	if err := a.fillWSMessages(byRow, int64(id), int64(id)); err != nil {
		return capture.Flow{}, false, err
	}
	return f, true, nil
}

// After returns up to n flows with their messages, oldest first, starting
// after the row after (0 for the first). It is how the whole archive is
// walked a page at a time — as the Explorer's backfill does — without
// holding it all in memory.
func (a *Archive) After(after uint64, n int) ([]capture.Flow, error) {
	rows, err := a.db.Query(`SELECT `+flowColumns+` FROM flows WHERE seq > ? ORDER BY seq LIMIT ?`,
		int64(after), n)
	if err != nil {
		return nil, fmt.Errorf("reading flows: %w", err)
	}
	var flows []capture.Flow
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading flows: %w", err)
		}
		flows = append(flows, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(flows) == 0 {
		return nil, err
	}
	byRow := make(map[int64]*capture.Flow, len(flows))
	for i := range flows {
		byRow[int64(flows[i].ID)] = &flows[i]
	}
	first, last := int64(flows[0].ID), int64(flows[len(flows)-1].ID)
	if _, err := a.fillMessageSizes(byRow, first, last); err != nil {
		return nil, err
	}
	if err := a.fillMessageBodies(byRow, first, last); err != nil {
		return nil, err
	}
	return flows, nil
}

// Clear empties the archive. It is what Edit → Clear Requests means once the
// traffic is being kept: the engagement forgets it.
func (a *Archive) Clear() error {
	if _, err := a.db.Exec(`DELETE FROM flows`); err != nil { // messages cascade
		return fmt.Errorf("clearing %s: %w", a.path, err)
	}
	a.mu.Lock()
	a.seq = make(map[uint64]int64)
	a.mu.Unlock()
	return nil
}

// scanner is what both Query rows and QueryRow satisfy.
type scanner interface{ Scan(dest ...any) error }

func scanFlow(s scanner) (capture.Flow, error) {
	var (
		f        capture.Flow
		seq      int64
		state    int
		started  int64
		duration int64
	)
	err := s.Scan(&seq, &state, &f.ClientAddr, &f.Target, &f.Method, &f.Host, &f.Path,
		&f.Secure, &f.TargetID, &f.StatusCode, &f.StatusText, &started, &duration, &f.Error,
		&f.Source, &f.Origin, &f.Protocol)
	if err != nil {
		return capture.Flow{}, err
	}
	f.ID = uint64(seq)
	f.State = capture.FlowState(state)
	f.Started = time.Unix(0, started)
	f.Duration = time.Duration(duration)
	return f, nil
}

// messageOf points at the message a row belongs to, creating the optional
// "before the edit" copies on demand. It returns nil for a kind this build
// doesn't know, so an archive from a later schema is read as far as it can be.
func messageOf(f *capture.Flow, kind messageKind) *capture.Message {
	switch kind {
	case msgRequest:
		return &f.Request
	case msgResponse:
		return &f.Response
	case msgOriginalRequest:
		if f.OriginalRequest == nil {
			f.OriginalRequest = &capture.Message{}
		}
		return f.OriginalRequest
	case msgOriginalResponse:
		if f.OriginalResponse == nil {
			f.OriginalResponse = &capture.Message{}
		}
		return f.OriginalResponse
	}
	return nil
}
