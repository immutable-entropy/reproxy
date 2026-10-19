package entities

import (
	"sync"
	"sync/atomic"
	"time"

	"reproxy/internal/capture"
)

// Tuning for the indexer.
const (
	// indexInterval is the least time between two batches, so that a burst
	// of traffic is merged in a few transactions rather than one per flow.
	indexInterval = 200 * time.Millisecond
	// maxBatch is the most flows parsed into one batch.
	maxBatch = 256
	// maxQueued bounds the queue. A flow arriving when it is full is not
	// indexed (and counted): the proxy must never wait on the Explorer.
	maxQueued = 50000
)

// Indexer follows a capture.Store and puts the traffic in the Explorer.
//
// Record is the store subscriber, and it only queues — it runs on the
// proxy's goroutine. A single background goroutine parses each finished
// flow once, merges it into the Store in batches, and puts the URLs its
// response links to in it as discovered.
//
// Clearing the traffic doesn't touch the Explorer: Record ignores FlowsCleared,
// and a FlowsReset (an engagement opened) only changes which flows count as
// indexed, since the IDs that follow are another archive's.
type Indexer struct {
	store *Store

	mu      sync.Mutex
	cond    *sync.Cond
	queue   map[uint64]capture.Flow
	order   []uint64
	indexed map[uint64]bool // flows already merged this session
	busy    bool            // a batch has left the queue but isn't applied

	dropped atomic.Uint64
	lastErr atomic.Value // error

	wake chan struct{}
	quit chan struct{}
	done chan struct{}
}

// NewIndexer starts an indexer feeding store. Close stops it.
func NewIndexer(store *Store) *Indexer {
	x := &Indexer{
		store:   store,
		queue:   make(map[uint64]capture.Flow),
		indexed: make(map[uint64]bool),
		wake:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	x.cond = sync.NewCond(&x.mu)
	go x.run()
	return x
}

// Record is the capture.Store subscriber. It queues flows that have
// finished, and nothing else; it never parses or writes.
func (x *Indexer) Record(e capture.Event) {
	switch e.Kind {
	case capture.FlowAdded, capture.FlowUpdated:
		if !final(e.Flow.State) || e.Flow.IsDNS() { // a lookup has no path to map
			return
		}
	case capture.FlowsReset:
		// The flows loaded came from an archive: they were indexed when
		// they were recorded, or will be by its backfill. Editing one later
		// must not count it again.
		indexed := make(map[uint64]bool, len(e.Flows))
		for _, f := range e.Flows {
			indexed[f.ID] = true
		}
		x.mu.Lock()
		x.indexed = indexed
		x.mu.Unlock()
		return
	default:
		return
	}
	x.mu.Lock()
	if x.indexed[e.Flow.ID] {
		x.mu.Unlock()
		return
	}
	if _, queued := x.queue[e.Flow.ID]; !queued {
		if len(x.order) >= maxQueued {
			x.mu.Unlock()
			x.dropped.Add(1)
			return
		}
		x.order = append(x.order, e.Flow.ID)
	}
	x.queue[e.Flow.ID] = e.Flow
	x.mu.Unlock()

	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Feed queues flows to index, as the backfill of an engagement's archived
// traffic does. It waits while the queue is full rather than dropping, and
// call it once per flow: unlike Record, it doesn't skip a flow already
// indexed.
func (x *Indexer) Feed(flows []capture.Flow) {
	for _, f := range flows {
		if !final(f.State) || f.IsDNS() {
			continue
		}
		x.mu.Lock()
		for len(x.order) >= maxQueued {
			x.cond.Wait()
		}
		// Fed flows are indexed even when Record counts them as done: a
		// FlowsReset marks what it loads that way, and the backfill is what
		// actually does it.
		if _, queued := x.queue[f.ID]; !queued {
			x.order = append(x.order, f.ID)
		}
		x.queue[f.ID] = f
		x.mu.Unlock()
	}
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Flush waits until everything queued so far is in the store.
func (x *Indexer) Flush() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
	x.mu.Lock()
	for len(x.order) > 0 || x.busy {
		x.cond.Wait()
	}
	x.mu.Unlock()
}

// Dropped counts the flows not indexed because the queue was full.
func (x *Indexer) Dropped() uint64 { return x.dropped.Load() }

// Err returns the last error writing the store, if any.
func (x *Indexer) Err() error {
	err, _ := x.lastErr.Load().(error)
	return err
}

// Close stops the indexer once the queue is drained.
func (x *Indexer) Close() {
	select {
	case <-x.quit:
	default:
		close(x.quit)
	}
	<-x.done
}

func (x *Indexer) run() {
	defer close(x.done)
	var last time.Time
	for {
		select {
		case <-x.wake:
		case <-x.quit:
			for x.step() {
			}
			return
		}
		if wait := indexInterval - time.Since(last); wait > 0 && !last.IsZero() {
			select {
			case <-time.After(wait):
			case <-x.quit:
			}
		}
		for x.step() {
		}
		last = time.Now()
	}
}

// step indexes one batch from the queue, and reports whether there was one.
func (x *Indexer) step() bool {
	x.mu.Lock()
	n := len(x.order)
	if n == 0 {
		x.mu.Unlock()
		return false
	}
	if n > maxBatch {
		n = maxBatch
	}
	flows := make([]capture.Flow, 0, n)
	for _, id := range x.order[:n] {
		flows = append(flows, x.queue[id])
		delete(x.queue, id)
		x.indexed[id] = true
	}
	x.order = append(x.order[:0:0], x.order[n:]...)
	x.busy = true
	x.cond.Broadcast() // room in the queue for Feed
	x.mu.Unlock()

	var b Batch
	for _, f := range flows {
		v, links, ok := Parse(f)
		if !ok {
			continue
		}
		b.Visits = append(b.Visits, v)
		b.Discoveries = append(b.Discoveries, links...)
	}
	if _, err := x.store.Apply(b); err != nil {
		x.lastErr.Store(err)
	}

	x.mu.Lock()
	x.busy = false
	x.cond.Broadcast()
	x.mu.Unlock()
	return true
}

// final reports whether a flow will no longer change.
func final(s capture.FlowState) bool {
	return s == capture.StateComplete || s == capture.StateFailed || s == capture.StateDropped
}
