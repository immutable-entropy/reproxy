package app

import (
	"database/sql"
	"path/filepath"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
)

// The traffic archive follows the open engagement. The capture store is
// subscribed to one Recorder for the life of the process; opening an
// engagement swaps the database underneath it and reloads the store from the
// new one, so the window shows the traffic that belongs to the engagement
// the user just opened (see internal/archive).
//
// The findings, entities and recovered-file stores ride along: their tables
// are in the same capture.db, so opening an engagement brings its findings,
// its Entity Explorer and the files the work recovered with its traffic.

// archivePath is where traffic is recorded: inside the open engagement, or
// the data directory's own archive while none is open.
func (a *App) archivePath() string {
	if e, ok := a.engagements.Current(); ok {
		return e.CapturePath()
	}
	return filepath.Join(a.DataDir(), archive.FileName)
}

// recoveredDir is where the files the work recovered are written: inside
// the open engagement, beside its archive, or under the data directory
// while none is open — so a recovery is never refused for want of an
// engagement, and the files still land in reproxy's own state (goal 2).
func (a *App) recoveredDir() string {
	if e, ok := a.engagements.Current(); ok {
		return filepath.Join(e.Dir, recoveredFolder)
	}
	return filepath.Join(a.DataDir(), recoveredFolder)
}

// recoveredFolder is the folder name, unchanged from before recovered files
// were recorded, so an engagement's existing tree is still where it was.
const recoveredFolder = "recovered"

// followEngagement points the recorder at the archive the open engagement
// calls for and seeds the store from it. It is the engagement manager's
// subscriber, and is also called once at startup.
//
// A failure to open the archive stops recording rather than writing one
// engagement's traffic into another's file: the proxy keeps running and the
// session's traffic is still collected in memory.
func (a *App) followEngagement() {
	path := a.archivePath()
	if a.recorder.Path() == path {
		return
	}
	// A crawl running belongs to the engagement that was open: it stops,
	// and its last requests end, before anything moves, so none of them is
	// recorded in the new engagement's archive.
	a.crawler.Stop()
	a.crawler.Wait()

	next, err := archive.Open(path)
	if err != nil {
		a.log.Error("traffic will not be recorded to disk", "archive", path, "err", err)
	}
	previous := a.recorder.Use(next)
	// The findings share the archive's database, so they move with it. This
	// has to happen while the previous archive is still open: closing it
	// closes the handle the findings store was reading through.
	var db *sql.DB
	if next != nil {
		db = next.DB()
	}
	if err := a.findings.Use(db); err != nil {
		a.log.Error("reading the engagement's findings", "archive", path, "err", err)
	}
	// What the indexer has queued belongs to the engagement that was open,
	// so it is written there before the Explorer moves.
	a.indexer.Flush()
	if err := a.entities.Use(db); err != nil {
		a.log.Error("reading the engagement's entities", "archive", path, "err", err)
	}
	if err := a.crawler.Use(db); err != nil {
		a.log.Error("reading the engagement's crawl", "archive", path, "err", err)
	}
	// The recovered files: their records are in the same database, their
	// bytes in a folder beside it (recoveredDir).
	if err := a.files.Use(db, a.recoveredDir()); err != nil {
		a.log.Error("reading the engagement's recovered files", "archive", path, "err", err)
	}
	if previous != nil {
		if err := previous.Close(); err != nil {
			a.log.Error("closing the previous traffic archive", "err", err)
		}
	}

	var flows []capture.Flow
	if next != nil {
		if flows, err = next.Recent(archive.LoadOptions{}); err != nil {
			a.log.Error("reading the engagement's recorded traffic", "archive", path, "err", err)
		}
	}
	// Reset even when there is nothing to load: the flows on screen belong
	// to the engagement that was open, not to this one.
	a.store.Reset(flows)
	if n := len(flows); n > 0 {
		a.log.Info("loaded the engagement's recorded traffic", "requests", n)
	}
	if next != nil && a.entities.NeedsBackfill() {
		go a.backfillEntities(next, path)
	}
	a.notifyEngagement()
}

// backfillEntities indexes the traffic an archive already held when it was
// upgraded to keep an Entity Explorer, once: a page at a time, so a large
// archive never sits in memory. It stops if the engagement changes under it,
// leaving the marker for next time.
func (a *App) backfillEntities(ar *archive.Archive, path string) {
	const page = 500
	var after uint64
	total := 0
	for {
		if a.recorder.Path() != path {
			return
		}
		flows, err := ar.After(after, page)
		if err != nil {
			a.log.Warn("indexing the engagement's recorded traffic", "err", err)
			return
		}
		if len(flows) == 0 {
			break
		}
		a.indexer.Feed(flows)
		after = flows[len(flows)-1].ID
		total += len(flows)
	}
	a.indexer.Flush()
	if a.recorder.Path() != path {
		return
	}
	if err := a.entities.BackfillDone(ar.DB()); err != nil {
		a.log.Warn("recording the Entity Explorer's backfill", "err", err)
	}
	if total > 0 {
		a.log.Info("mapped the engagement's recorded traffic", "requests", total)
	}
}

// closeArchive stops recording and flushes what is queued. It runs after the
// proxy has stopped, so a request in flight still reaches the archive.
func (a *App) closeArchive() error {
	// The findings and the Explorer let go of the database first: it is
	// closed below.
	_ = a.findings.Use(nil)
	a.indexer.Flush()
	_ = a.entities.Use(nil)
	_ = a.crawler.Use(nil)
	previous := a.recorder.Use(nil)
	if previous == nil {
		return nil
	}
	return previous.Close()
}
