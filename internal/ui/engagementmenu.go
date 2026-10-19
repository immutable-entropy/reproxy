package ui

import (
	"fmt"
	"path/filepath"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/engagement"
)

// The File menu's engagement items: New, Open, Open Recent and Close. An
// engagement is the container for a piece of work — its targets, and in time
// its captured traffic — so opening one swaps what the rest of the window is
// looking at (see internal/engagement).

// refreshEngagement brings the window title and the File menu in line with
// the engagement that is open. It runs on the main thread, from the manager's
// subscription as well as at startup.
func (w *mainWindow) refreshEngagement() {
	e, open := w.ctrl.Engagements().Current()
	if open {
		w.win.SetWindowTitle(e.Label() + " — " + AppName)
	} else {
		w.win.SetWindowTitle(AppName)
	}
	w.closeEngagement.SetEnabled(open)
	w.closeEngagement.SetToolTip("Close the engagement and go back to working without one")
	w.rebuildRecentMenu(e, open)
}

// rebuildRecentMenu refills Open Recent from the manager's list. The open
// engagement is listed but not clickable, so the menu doubles as a reminder
// of which one it is.
func (w *mainWindow) rebuildRecentMenu(current engagement.Engagement, open bool) {
	w.recentMenu.Clear()
	recent := w.ctrl.Engagements().Recent()
	if len(recent) == 0 {
		none := w.addAction(w.recentMenu, "No recent engagements", "", nil)
		none.SetEnabled(false)
		return
	}
	for _, dir := range recent {
		dir := dir
		label := filepath.Base(dir)
		if open && dir == current.Dir {
			label += "  (open)"
		}
		a := w.addAction(w.recentMenu, label, "", func() { w.openEngagementDir(dir) })
		a.SetToolTip(dir)
		if open && dir == current.Dir {
			a.SetEnabled(false)
		}
	}
}

// newEngagement runs the New Engagement wizard and opens what it creates.
// The requests on screen are not lost by switching: they belong to the
// engagement that was open and stay in its archive.
func (w *mainWindow) newEngagement() {
	created := runNewEngagementWizard(w.win.QWidget, w.ctrl.DataDir(), func(plan engagementPlan) error {
		_, err := w.ctrl.Engagements().Create(plan.dir, plan.name, plan.description, plan.targets)
		return err
	})
	if !created {
		return
	}
	w.selectSection(sectionTargets)
}

// openEngagement asks for an engagement folder and opens it.
func (w *mainWindow) openEngagement() {
	dir := qt.QFileDialog_GetExistingDirectory3(w.win.QWidget,
		"Open engagement", w.engagementBrowseStart())
	if dir == "" {
		return
	}
	w.openEngagementDir(dir)
}

// engagementBrowseStart is where the Open dialog starts: beside the
// engagements already made, if any.
func (w *mainWindow) engagementBrowseStart() string {
	if recent := w.ctrl.Engagements().Recent(); len(recent) > 0 {
		return filepath.Dir(recent[0])
	}
	return filepath.Join(w.ctrl.DataDir(), "engagements")
}

// openEngagementDir opens one engagement by path, reporting a folder that
// turns out not to hold one. A folder that has gone is forgotten, so the
// recent list doesn't keep offering it.
func (w *mainWindow) openEngagementDir(dir string) {
	if _, err := w.ctrl.Engagements().Open(dir); err != nil {
		qt.QMessageBox_Warning(w.win.QWidget, "Couldn't open the engagement", sentence(err.Error()))
		if _, statErr := engagement.Open(dir); statErr != nil {
			w.ctrl.Engagements().Forget(dir)
		}
		return
	}
	// The request list follows: the app loads the engagement's own traffic
	// into the store, which reaches this window as a reset.
	w.selectSection(sectionTargets)
}

// closeCurrentEngagement closes the open engagement, putting the window back
// to working with the data directory's own targets.
func (w *mainWindow) closeCurrentEngagement() {
	if _, open := w.ctrl.Engagements().Current(); !open {
		return
	}
	if err := w.ctrl.Engagements().Close(); err != nil {
		qt.QMessageBox_Warning(w.win.QWidget, "Couldn't close the engagement", sentence(err.Error()))
	}
}

// clearRequests empties the request list. With the traffic being kept, that
// also deletes it from the engagement's archive, so it asks first.
func (w *mainWindow) clearRequests() {
	n := w.ctrl.Store().Len()
	if n == 0 {
		return
	}
	where := "the archive"
	if e, open := w.ctrl.Engagements().Current(); open {
		where = "\"" + e.Label() + "\""
	}
	answer := qt.QMessageBox_Question6(w.win.QWidget, "Clear requests",
		fmt.Sprintf("Delete the %d %s shown?\n\nThey are removed from the list and from %s "+
			"on disk. This can't be undone.\n\nThe Explorer and the findings are kept.", n, plural(n, "request", "requests"), where),
		qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
	if answer == qt.QMessageBox__Yes {
		w.ctrl.Store().Clear()
	}
}
