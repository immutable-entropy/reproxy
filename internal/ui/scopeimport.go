package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/target"
)

// Importing a scope file
//
// Burp Suite keeps a project's scope as include and exclude lists, and
// HackerOne hands a program's scope out in the same shape, so one file brings
// a whole engagement's targets in. The reading and the mapping are core
// (target.ParseBurpScope); this is only the file picker, the confirmation and
// the report, shared by the Targets section and the New Engagement wizard.

// scopeFileFilter is what the file dialog offers.
const scopeFileFilter = "Burp Suite scope files (*.json);;All files (*)"

// pickedScope is a scope file the user chose and what it held.
type pickedScope struct {
	imp  *target.ScopeImport
	name string // the file's base name, for the report
}

// pickScopeFile asks for a scope file and parses it, reporting anything that
// went wrong itself. ok is false when the user cancelled or the file was not
// a scope file.
func pickScopeFile(parent *qt.QWidget) (pickedScope, bool) {
	path := qt.QFileDialog_GetOpenFileName4(parent,
		"Import targets from a scope file", "", scopeFileFilter)
	if path == "" {
		return pickedScope{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		qt.QMessageBox_Warning(parent, "Couldn't import", sentence(err.Error()))
		return pickedScope{}, false
	}
	defer f.Close()
	imp, err := target.ParseBurpScope(f)
	if err != nil {
		qt.QMessageBox_Warning(parent, "Couldn't import", sentence(err.Error()))
		return pickedScope{}, false
	}
	if len(imp.Targets) == 0 {
		qt.QMessageBox_Warning(parent, "Nothing to import",
			scopeProblems(imp, "No target could be made from "+filepath.Base(path)+"."))
		return pickedScope{}, false
	}
	return pickedScope{imp: imp, name: filepath.Base(path)}, true
}

// confirmScopeImport shows what the file holds and asks whether to go ahead,
// because an import can add hundreds of targets at once and a target is what
// decides whether traffic is collected at all.
func confirmScopeImport(parent *qt.QWidget, p pickedScope) bool {
	body := []string{fmt.Sprintf("%s holds %s.", p.name,
		countOf(len(p.imp.Targets), "target", "targets"))}
	body = append(body, p.imp.Notes...)
	if s := scopeProblems(p.imp, ""); s != "" {
		body = append(body, s)
	}
	items := make([]string, 0, len(p.imp.Targets))
	for _, t := range p.imp.Targets {
		items = append(items, scopeItem(t))
	}
	ok, _ := confirm(parent, confirmOptions{
		title:   "Import targets",
		heading: "Add these targets to the engagement?",
		body:    body,
		items:   items,
		cancel:  "Cancel",
		ok:      fmt.Sprintf("Add %s", countOf(len(p.imp.Targets), "target", "targets")),
	})
	return ok
}

// scopeItem names one target in the confirmation, with what it narrows.
func scopeItem(t target.Target) string {
	s := t.Display()
	var extra []string
	if n := len(t.Rules); n > 0 {
		extra = append(extra, countOf(n, "path rule", "path rules"))
	}
	if !t.Enabled {
		extra = append(extra, "disabled")
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}

// scopeProblems lists the entries the import could not use, so nothing is
// lost silently. It returns "" when there were none and lead is empty.
func scopeProblems(imp *target.ScopeImport, lead string) string {
	if len(imp.Skipped) == 0 {
		return lead
	}
	lines := make([]string, 0, maxConfirmItems+1)
	for i, s := range imp.Skipped {
		if i == maxConfirmItems {
			lines = append(lines, fmt.Sprintf("…and %d more.", len(imp.Skipped)-maxConfirmItems))
			break
		}
		lines = append(lines, fmt.Sprintf("%s — %s", s.Entry, s.Reason))
	}
	msg := fmt.Sprintf("%s skipped:\n%s",
		countOf(len(imp.Skipped), "entry was", "entries were"), strings.Join(lines, "\n"))
	if lead != "" {
		return lead + "\n\n" + msg
	}
	return msg
}

// reportScopeImport says what was added once the store has it.
func reportScopeImport(parent *qt.QWidget, p pickedScope, added, duplicates int) {
	msg := fmt.Sprintf("Added %s from %s.", countOf(added, "target", "targets"), p.name)
	if duplicates > 0 {
		msg += fmt.Sprintf("\n\n%s already in the list and left alone.",
			countOf(duplicates, "target was", "targets were"))
	}
	if s := scopeProblems(p.imp, ""); s != "" {
		msg += "\n\n" + s
		qt.QMessageBox_Warning(parent, "Imported with problems", msg)
		return
	}
	qt.QMessageBox_Information(parent, "Imported", msg)
}
