package archive

import (
	"sync"

	"reproxy/internal/capture"
)

// Recorder is the capture.Store subscriber that follows the open engagement.
// Opening an engagement swaps the archive underneath it, so the store is
// subscribed once, at startup, and never has to know that the file changed.
//
// Record takes a read lock, so the proxy's goroutines never contend with each
// other; only a swap (rare, and on the user's own action) excludes them.
type Recorder struct {
	mu  sync.RWMutex
	cur *Archive
}

// NewRecorder returns a recorder writing to a, which may be nil for one that
// discards what it is given.
func NewRecorder(a *Archive) *Recorder { return &Recorder{cur: a} }

// Record implements the store subscription.
func (r *Recorder) Record(e capture.Event) {
	r.mu.RLock()
	a := r.cur
	if a != nil {
		a.Record(e)
	}
	r.mu.RUnlock()
}

// Current returns the archive being written to, or nil.
func (r *Recorder) Current() *Archive {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// Path returns the archive's file, or "" when there is none.
func (r *Recorder) Path() string {
	if a := r.Current(); a != nil {
		return a.Path()
	}
	return ""
}

// Use switches to next and returns the archive that was in use, for the
// caller to flush and close. Every event is recorded into exactly one of
// them: the swap excludes Record rather than racing it.
func (r *Recorder) Use(next *Archive) (previous *Archive) {
	r.mu.Lock()
	previous, r.cur = r.cur, next
	r.mu.Unlock()
	return previous
}
