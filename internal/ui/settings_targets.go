package ui

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/target"
)

// newTargetsSettingsPage is Settings → Global → Targets: whether traffic
// must belong to a target to be collected, whether plugins' entity hooks
// run for entities outside the targets, and the suppression list every
// engagement shares — the hosts never suggested as related to a target
// (see target.Suppression). An open engagement can override the first and
// add to the second (Settings → Engagement → Targets); the page says so
// when it does.
func newTargetsSettingsPage(ctrl Controller) *settingsPage {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Scope"))
	required := qt.NewQCheckBox3("Only collect traffic that belongs to a target")
	v.AddWidget(required.QWidget)
	v.AddWidget(requiredHelp().QWidget)
	override := newOverrideNote()
	v.AddWidget(override.QWidget)

	v.AddSpacing(8)
	v.AddWidget(sectionTitle("Plugins"))
	allEntities := qt.NewQCheckBox3("Run entity hooks for every entity, not only those in a target")
	v.AddWidget(allEntities.QWidget)
	v.AddWidget(helpText("Off (the default), a plugin's created and updated hooks for domains, hosts and " +
		"URLs (entity.on_created, url.on_updated and the rest) only run for entities that belong to a " +
		"target. Turn it on to run them for every entity the Explorer learns of, including related hosts " +
		"and traffic no target covers. Deleted hooks always run, and while no target is set up every " +
		"entity counts as in scope.").QWidget)

	v.AddSpacing(8)
	sup := newSuppressionEditor(ctrl)
	v.AddLayout(sup.header("Suppressed"))
	v.AddWidget(helpText("Hosts here are never suggested under Targets → Related, however your targets' " +
		"traffic refers to them: analytics, ad networks, the CDNs every site uses. Their URLs " +
		"are still kept, so removing a suppression brings a host back at once. This list applies to " +
		"every engagement; an engagement can add its own in Settings → Engagement → Targets.").QWidget)
	v.AddWidget2(sup.root, 1)

	return &settingsPage{
		title:       settingsTargets,
		group:       settingsGroupGlobal,
		description: "Whether traffic has to belong to a target before it is collected, which entities plugins hear about, and the hosts never suggested as related.",
		body:        body,
		load: func(c config.Config) {
			required.SetChecked(c.Targets.Required)
			allEntities.SetChecked(c.Targets.AllEntityHooks)
			sup.load(c.Targets.Suppressed)
		},
		store: func(c *config.Config) {
			c.Targets.Required = required.IsChecked()
			c.Targets.AllEntityHooks = allEntities.IsChecked()
			c.Targets.Suppressed = sup.list()
		},
		shown: func() {
			sup.showSelected() // what an entry hides depends on the traffic since
			e, name, ok := ctrl.EngagementSettings()
			override.show(ok && e.TargetsRequired != nil, fmt.Sprintf("The open engagement, %s, sets this "+
				"for itself (Settings → Engagement → Targets): only collecting what belongs to a target is %s "+
				"while it is open.", name, onOff(e.TargetsRequired)))
		},
	}
}

// requiredHelp explains "Only collect traffic that belongs to a target".
func requiredHelp() *qt.QLabel {
	return helpText("On (the default), a request that matches none of the targets is " +
		"passed straight through to the server: it is never recorded, never held for " +
		"editing, and never reaches the request list. Only what you are authorised to look at " +
		"is collected. Turn it off to record everything that goes through the proxy. While no " +
		"target is set up, everything is collected regardless.")
}

func onOff(b *bool) string {
	if b != nil && *b {
		return "on"
	}
	return "off"
}

// overrideNote is the notice on a global page whose setting the open
// engagement overrides.
type overrideNote struct{ *qt.QLabel }

func newOverrideNote() overrideNote {
	l := qt.NewQLabel2()
	setName(l.QObject, "WarningText")
	l.SetWordWrap(true)
	l.Hide()
	return overrideNote{l}
}

func (n overrideNote) show(on bool, text string) {
	n.SetText(text)
	n.SetVisible(on)
}

// suppressionEditor edits a suppression list, laid out like Plugins →
// Plugins: the entries on the left with the buttons that work on the whole
// list under them, and the selected entry on the right.
//
//	Suppressed                                        [+]
//	┌──────────────┐ ┌──────────────────────────────────┐
//	│ *.doubleclick│ │ *.doubleclick.net                │
//	│ fonts.gstatic│ │ Every host matching ...          │
//	└──────────────┘ │ Note [............]   [Remove]   │
//	[Import…][Export…]└──────────────────────────────────┘
//
// The global Targets page and the engagement's each have one.
type suppressionEditor struct {
	ctrl Controller
	root *qt.QWidget

	// entries is the draft list, saved with the page.
	entries []target.Suppression
	list_   *qt.QListWidget
	stack   *qt.QStackedWidget
	pattern *qt.QLabel
	covers  *qt.QLabel
	added   *qt.QLabel
	note    *qt.QLineEdit
	hiding  *qt.QLabel
	// loading guards the note's change signal while an entry is shown.
	loading bool
}

func newSuppressionEditor(ctrl Controller) *suppressionEditor {
	e := &suppressionEditor{ctrl: ctrl, root: qt.NewQWidget2()}
	row := qt.NewQHBoxLayout(e.root)
	row.SetContentsMargins(0, 0, 0, 0)
	row.SetSpacing(12)

	left := qt.NewQVBoxLayout2()
	left.SetSpacing(6)
	e.list_ = qt.NewQListWidget2()
	setName(e.list_.QObject, "MasterList")
	e.list_.SetFixedWidth(230)
	e.list_.SetMinimumHeight(240)
	e.list_.SetFrameShape(qt.QFrame__NoFrame)
	e.list_.OnCurrentRowChanged(func(int) { e.showSelected() })
	left.AddWidget(e.list_.QWidget)
	importBtn := qt.NewQPushButton3("Import…")
	importBtn.SetAutoDefault(false)
	importBtn.SetToolTip("Add the entries of a suppression list file to this one")
	importBtn.OnClicked(e.importList)
	exportBtn := qt.NewQPushButton3("Export…")
	exportBtn.SetAutoDefault(false)
	exportBtn.SetToolTip("Save this suppression list to a file, one entry per line")
	exportBtn.OnClicked(e.exportList)
	left.AddWidget(importBtn.QWidget)
	left.AddWidget(exportBtn.QWidget)
	left.AddStretch()
	row.AddLayout(left.QLayout)

	e.stack = qt.NewQStackedWidget2()
	empty := qt.NewQWidget2()
	ev := qt.NewQVBoxLayout(empty)
	emptyLabel := qt.NewQLabel3("Nothing selected. Choose an entry on the left, add one with the + above, " +
		"or suppress a host from Targets → Related.")
	setName(emptyLabel.QObject, "EmptyState")
	emptyLabel.SetWordWrap(true)
	ev.AddStretch()
	ev.AddWidget(emptyLabel.QWidget)
	ev.AddStretch()
	e.stack.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	e.stack.AddWidget(empty)
	e.stack.AddWidget(e.buildDetail())

	detailPanel := qt.NewQFrame2()
	setName(detailPanel.QObject, "Panel")
	detailPanel.SetFrameShape(qt.QFrame__StyledPanel)
	dp := qt.NewQVBoxLayout(detailPanel.QWidget)
	dp.SetContentsMargins(12, 10, 12, 10)
	dp.AddWidget(e.stack.QWidget)
	row.AddWidget2(detailPanel.QWidget, 1)
	return e
}

// header is the list's section heading with its + button.
func (e *suppressionEditor) header(title string) *qt.QLayout {
	head := qt.NewQHBoxLayout2()
	head.SetContentsMargins(0, 0, 0, 0)
	head.AddWidget(sectionTitle(title))
	head.AddStretch()
	head.AddWidget(newIconButton(iconPlus, "Suppress a domain, wildcard, IP address or CIDR range", e.add).w)
	return head.QLayout
}

func (e *suppressionEditor) buildDetail() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(8)

	e.pattern = qt.NewQLabel2()
	setName(e.pattern.QObject, "PanelTitle")
	e.pattern.SetTextInteractionFlags(qt.TextSelectableByMouse)
	e.covers = helpText("")
	e.added = helpText("")
	v.AddWidget(e.pattern.QWidget)
	v.AddWidget(e.covers.QWidget)
	v.AddWidget(e.added.QWidget)

	noteRow := qt.NewQHBoxLayout2()
	noteRow.SetSpacing(8)
	e.note = qt.NewQLineEdit2()
	e.note.SetPlaceholderText("Why it is suppressed (optional)")
	e.note.OnTextChanged(func(text string) {
		if i := e.list_.CurrentRow(); !e.loading && i >= 0 && i < len(e.entries) {
			e.entries[i].Note = strings.TrimSpace(text)
		}
	})
	noteRow.AddWidget(qt.NewQLabel3("Note").QWidget)
	noteRow.AddWidget2(e.note.QWidget, 1)
	v.AddLayout(noteRow.QLayout)

	v.AddWidget(sectionTitle("Hiding"))
	e.hiding = helpText("")
	e.hiding.SetTextInteractionFlags(qt.TextSelectableByMouse)
	v.AddWidget(e.hiding.QWidget)
	v.AddStretch()

	actions := qt.NewQHBoxLayout2()
	actions.AddStretch()
	remove := qt.NewQPushButton3("Remove")
	remove.SetAutoDefault(false)
	remove.SetToolTip("Stop suppressing this entry: the hosts it hides are suggested again")
	remove.OnClicked(e.remove)
	actions.AddWidget(remove.QWidget)
	v.AddLayout(actions.QLayout)
	return w
}

// load replaces the draft with list.
func (e *suppressionEditor) load(list []target.Suppression) {
	e.entries = append([]target.Suppression(nil), list...)
	e.refresh(0)
}

// list is the draft, nil when empty (as an unset list loads).
func (e *suppressionEditor) list() []target.Suppression {
	if len(e.entries) == 0 {
		return nil
	}
	return append([]target.Suppression(nil), e.entries...)
}

// refresh refills the list and selects row (clamped to the list).
func (e *suppressionEditor) refresh(row int) {
	e.list_.BlockSignals(true)
	e.list_.Clear()
	for _, s := range e.entries {
		e.list_.AddItem(s.Pattern)
	}
	e.list_.BlockSignals(false)
	if row >= len(e.entries) {
		row = len(e.entries) - 1
	}
	if row >= 0 {
		e.list_.SetCurrentRow(row) // shows it
		return
	}
	e.showSelected()
}

// showSelected puts the selected entry in the detail panel.
func (e *suppressionEditor) showSelected() {
	i := e.list_.CurrentRow()
	if i < 0 || i >= len(e.entries) {
		e.stack.SetCurrentIndex(0)
		return
	}
	s := e.entries[i]
	e.loading = true
	e.pattern.SetText(s.Pattern)
	e.covers.SetText(s.Describe() + ".")
	if s.Added.IsZero() {
		e.added.SetText("")
	} else {
		e.added.SetText("Suppressed " + s.Added.Local().Format("2 January 2006, 15:04"))
	}
	e.note.SetText(s.Note)
	e.loading = false
	e.hiding.SetText(e.hidingText(s))
	e.stack.SetCurrentIndex(1)
}

// hidingText lists the hosts of the open engagement the entry keeps out of
// Related, including for an entry not saved yet.
func (e *suppressionEditor) hidingText(s target.Suppression) string {
	eps := e.ctrl.Entities()
	var hosts []string
	for _, list := range [][]entities.Related{eps.Suppressed(), eps.Related()} {
		for _, r := range list {
			if s.Matches(r.Host) {
				hosts = append(hosts, r.Host)
			}
		}
	}
	switch {
	case len(hosts) == 0:
		return "No host the open engagement's targets refer to matches it yet."
	case len(hosts) > 12:
		return fmt.Sprintf("%s and %d more.", strings.Join(hosts[:12], ", "), len(hosts)-12)
	}
	return strings.Join(hosts, ", ") + "."
}

// add asks for an entry and adds it.
func (e *suppressionEditor) add() {
	text := qt.QInputDialog_GetText(e.root, "Suppress",
		"Domain, *.wildcard, IP address or CIDR range never to suggest as related:")
	s := target.Suppression{Pattern: text, Added: time.Now()}.Normalized()
	if s.Pattern == "" {
		return
	}
	if err := s.Validate(); err != nil {
		qt.QMessageBox_Warning(e.root, "Can't suppress that", sentence(err.Error()))
		return
	}
	for i, have := range e.entries {
		if have.Pattern == s.Pattern {
			e.list_.SetCurrentRow(i) // already listed
			return
		}
	}
	e.entries = append(e.entries, s)
	e.refresh(len(e.entries) - 1)
}

func (e *suppressionEditor) remove() {
	i := e.list_.CurrentRow()
	if i < 0 || i >= len(e.entries) {
		return
	}
	e.entries = append(e.entries[:i:i], e.entries[i+1:]...)
	e.refresh(i)
}

// importList merges a suppression list file into the draft. Lines that
// aren't valid are reported, and the rest still imported.
func (e *suppressionEditor) importList() {
	path := qt.QFileDialog_GetOpenFileName4(e.root, "Import suppression list", "",
		"Suppression lists (*.txt *.list);;All files (*)")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		qt.QMessageBox_Warning(e.root, "Couldn't import", err.Error())
		return
	}
	found, errs := target.ParseSuppressions(bytes.NewReader(data), time.Now())
	var added int
	e.entries, added = target.MergeSuppressions(e.entries, found)
	e.refresh(len(e.entries) - added)

	msg := fmt.Sprintf("Added %s.", countOf(added, "entry", "entries"))
	if dup := len(found) - added; dup > 0 {
		msg += fmt.Sprintf(" %s already listed.", countOf(dup, "was", "were"))
	}
	if len(errs) > 0 {
		lines := make([]string, 0, 10)
		for i, err := range errs {
			if i == 10 {
				lines = append(lines, fmt.Sprintf("…and %d more.", len(errs)-10))
				break
			}
			lines = append(lines, err.Error())
		}
		msg += fmt.Sprintf("\n\n%s skipped:\n%s", countOf(len(errs), "line was", "lines were"), strings.Join(lines, "\n"))
		qt.QMessageBox_Warning(e.root, "Imported with problems", msg)
		return
	}
	qt.QMessageBox_Information(e.root, "Imported", msg+" They take effect when the settings are applied.")
}

// exportList writes the draft list (unsaved changes included) to a file.
func (e *suppressionEditor) exportList() {
	path := qt.QFileDialog_GetSaveFileName4(e.root, "Export suppression list",
		"reproxy-suppressed.txt", "Suppression lists (*.txt *.list);;All files (*)")
	if path == "" {
		return
	}
	var buf bytes.Buffer
	if err := target.ExportSuppressions(&buf, e.entries); err != nil {
		qt.QMessageBox_Warning(e.root, "Couldn't export", err.Error())
		return
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		qt.QMessageBox_Warning(e.root, "Couldn't export", err.Error())
		return
	}
	qt.QMessageBox_Information(e.root, "Exported",
		fmt.Sprintf("Saved %s to:\n%s", countOf(len(e.entries), "entry", "entries"), path))
}
