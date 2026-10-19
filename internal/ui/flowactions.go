package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/capture"
	"reproxy/internal/files"
	"reproxy/internal/plugin"
)

// The traffic list's context menu: the actions the running plugins offer
// for the request under the pointer (plugin.Manager.Actions). Which actions
// apply is decided by the plugin package, from each action's filter; the
// menu only lists them and runs the one picked.

// showFlowMenu opens the menu for the row at pos, selecting it first so the
// viewer shows the request the action will run on.
func (w *mainWindow) showFlowMenu(pos *qt.QPoint) {
	idx := w.list.IndexAt(pos)
	if !idx.IsValid() {
		return
	}
	f, ok := w.model.at(idx.Row())
	if !ok {
		return
	}
	w.list.SetCurrentIndex(idx)

	menu := qt.NewQMenu2()
	menu.SetToolTipsVisible(true)

	// Core actions come first: things reproxy itself can do with the flow,
	// not a plugin. "Export recovered sources…" is offered when the response
	// could be a JavaScript source map.
	core := false
	if looksLikeSourceMap(f) {
		item := menu.AddActionWithText("Export recovered sources…")
		item.SetToolTip("Recover the original source files embedded in this source map and write them under the engagement.")
		item.OnTriggered(func() { w.runExportSources(f) })
		core = true
	}

	actions := w.ctrl.Plugins().Actions(f)
	if core && len(actions) > 0 {
		menu.AddSeparator()
	}
	if !core && len(actions) == 0 {
		none := menu.AddActionWithText("No actions for this request")
		none.SetEnabled(false)
	}
	section := ""
	for _, a := range actions {
		if a.Plugin != section {
			// A heading of the plugin's name. QMenu.AddSection would be the
			// natural way, but most styles draw a section as a bare divider
			// and drop its text, and the name is what tells two plugins'
			// actions apart.
			if section != "" {
				menu.AddSeparator()
			}
			heading := menu.AddActionWithText(a.Plugin)
			heading.SetEnabled(false)
			font := heading.Font()
			font.SetBold(true)
			heading.SetFont(font)
			section = a.Plugin
		}
		item := menu.AddActionWithText(a.Label)
		if a.Description != "" {
			item.SetToolTip(a.Description)
		}
		item.OnTriggered(func() { w.runFlowAction(a, f) })
	}
	menu.Exec3(w.list.Viewport().MapToGlobalWithQPoint(pos), nil)
	menu.DeleteLater()
}

// runFlowAction runs an action off the UI thread (it may send requests and
// wait for them), then says how it went: in the status line and with a
// flash of the row, or in a message box when it failed.
func (w *mainWindow) runFlowAction(a plugin.Action, f capture.Flow) {
	w.flash(fmt.Sprintf("%s: running…", a.Label))
	go func() {
		res, err := w.ctrl.Plugins().RunAction(a.Plugin, a.ID, f)
		mainthread.Start(func() {
			if err != nil {
				w.pulseRow(f.ID, colorServerError)
				qt.QMessageBox_Warning(w.win.QWidget, a.Label,
					fmt.Sprintf("The %s plugin couldn't finish this action.\n\n%s\n\nThe plugin's log in the Plugins window has the details.", a.Plugin, sentence(err.Error())))
				return
			}
			w.pulseRow(f.ID, colorSuccess)
			msg := firstLineOf(res.Message)
			if msg == "" {
				msg = "done"
			}
			w.flash(fmt.Sprintf("%s: %s", a.Label, msg))
		})
	}()
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// looksLikeSourceMap reports whether a flow's response could be a JavaScript
// source map, so the export item is offered. It is a cheap gate on the URL
// and content type; whether it really is one is decided when the export runs.
func looksLikeSourceMap(f capture.Flow) bool {
	if len(f.Response.Raw) == 0 {
		return false
	}
	u := f.URL()
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if strings.HasSuffix(u, ".map") {
		return true
	}
	head, _, _ := strings.Cut(string(f.Response.Raw), "\r\n\r\n")
	ct := ""
	for _, line := range strings.Split(head, "\r\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "content-type") {
			ct = strings.ToLower(value)
			break
		}
	}
	return strings.Contains(ct, "json") || strings.Contains(ct, "javascript") || strings.Contains(ct, "octet-stream")
}

// runExportSources writes a flow's recovered source tree off the UI thread,
// then names the directory it wrote to, or explains why it couldn't.
func (w *mainWindow) runExportSources(f capture.Flow) {
	w.flash("Export recovered sources: running…")
	go func() {
		res, err := w.ctrl.ExportSourceMap(f.ID, files.SourceUser, "")
		mainthread.Start(func() {
			if err != nil {
				w.pulseRow(f.ID, colorServerError)
				qt.QMessageBox_Warning(w.win.QWidget, "Export recovered sources",
					fmt.Sprintf("Couldn't recover source from this response.\n\n%s", sentence(err.Error())))
				return
			}
			w.pulseRow(f.ID, colorSuccess)
			msg := exportMessage(res)
			qt.QMessageBox_Information(w.win.QWidget, "Export recovered sources", msg)
			w.flash(fmt.Sprintf("Recovered %d source files", res.Files))
		})
	}()
}

// exportMessage is what the user is told after a recovery: how much was
// recovered, how much of it is the application's own code rather than a
// dependency's, and where it went. Files already recovered byte for byte
// are called out, since a second run of the same export writes nothing and
// the dialog would otherwise look like it failed.
func exportMessage(res files.Result) string {
	dir := res.Dir
	if sub := res.CommonDir(); sub != "" {
		dir = filepath.Join(dir, filepath.FromSlash(sub))
	}
	msg := fmt.Sprintf("Recovered %d source files (%d the app's own) to:\n\n%s",
		res.Files, res.CountKind(files.KindSourceMap), dir)
	if res.Unchanged > 0 {
		msg += fmt.Sprintf("\n\n%d file(s) were already recovered, unchanged.", res.Unchanged)
	}
	if len(res.Skipped) > 0 {
		msg += fmt.Sprintf("\n\n%d path(s) were skipped as unsafe or over the limit.", len(res.Skipped))
	}
	return msg
}
