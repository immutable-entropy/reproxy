package ui

import (
	"fmt"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/target"
)

// The editor behind the pencil in the Targets list: one dialog with a
// General tab (the address, an optional name, notes and the path rules) and a
// Settings tab (colour, collection, delete).

var kindOptions = func() []option[target.Kind] {
	opts := make([]option[target.Kind], 0, len(target.Kinds))
	for _, k := range target.Kinds {
		opts = append(opts, option[target.Kind]{kindLabel(k), k})
	}
	return opts
}()

var targetActionOptions = []option[target.Action]{
	{"Allow", target.Allow},
	{"Deny", target.Deny},
}

// kindPlaceholders show what each kind expects.
var kindPlaceholders = map[target.Kind]string{
	target.KindDomain:   "app.example.test",
	target.KindWildcard: "*.staging.example.test",
	target.KindRegex:    `api\d+\.example\.test`,
	target.KindIP:       "198.51.100.7",
	target.KindCIDR:     "10.0.0.0/8",
}

// addressEditor is the kind/value pair that is a target's address. The kind
// follows what is typed — "10.0.0.0/8" switches itself to CIDR range — until
// the user picks one, after which their choice stands.
type addressEditor struct {
	row   *qt.QWidget
	kind  *qt.QComboBox
	value *qt.QLineEdit
	// chosen records that the user picked the kind themselves.
	chosen bool
}

func newAddressEditor(t target.Target) *addressEditor {
	a := &addressEditor{row: qt.NewQWidget2()}
	h := qt.NewQHBoxLayout(a.row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	a.kind = qt.NewQComboBox2()
	fillCombo(a.kind, kindOptions)
	selectOption(a.kind, kindOptions, t.Kind)
	a.kind.SetFixedWidth(targetColKind)
	a.kind.SetToolTip("How the address is matched against a request's host")
	a.value = qt.NewQLineEdit3(t.Value)
	a.value.SetMinimumWidth(240)
	a.chosen = t.Value != "" // an existing target's kind was settled already

	a.placeholder()
	a.kind.OnActivated(func(int) { // activated fires only on a user's pick
		a.chosen = true
		a.placeholder()
	})
	a.kind.OnCurrentIndexChanged(func(int) { a.placeholder() })
	a.value.OnTextEdited(func(text string) {
		if a.chosen {
			return
		}
		selectOption(a.kind, kindOptions, target.DetectKind(text))
	})

	h.AddWidget(a.kind.QWidget)
	h.AddWidget2(a.value.QWidget, 1)
	return a
}

func (a *addressEditor) placeholder() {
	a.value.SetPlaceholderText(kindPlaceholders[a.selectedKind()])
}

func (a *addressEditor) selectedKind() target.Kind { return selectedOption(a.kind, kindOptions) }

// apply writes the edited address onto t.
func (a *addressEditor) apply(t target.Target) target.Target {
	t.Kind = a.selectedKind()
	t.Value = strings.TrimSpace(a.value.Text())
	return t
}

// runTargetDialog edits one target over two tabs: General (address, name,
// notes, rules) and Settings (colour, collection, delete). isNew leaves out
// the delete action, there being nothing saved to delete yet. It returns the
// edited target, whether to delete it, and whether the user accepted.
// Nothing is saved here.
func runTargetDialog(parent *qt.QWidget, t target.Target, isNew bool) (target.Target, bool, bool) {
	dlg := qt.NewQDialog(parent)
	if isNew {
		dlg.SetWindowTitle("New target")
	} else {
		dlg.SetWindowTitle("Edit target")
	}
	dlg.Resize(760, 680)
	dlg.SetMinimumSize2(620, 520)

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	tabs := qt.NewQTabWidget2()
	root.AddWidget2(tabs.QWidget, 1)

	// ---- General tab -------------------------------------------------------
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(22, 18, 22, 18)
	v.SetSpacing(10)

	form := newSettingsForm()
	address := newAddressEditor(t)
	form.AddRow3("Address", address.row)
	name := qt.NewQLineEdit3(t.Name)
	name.SetMinimumWidth(320)
	name.SetPlaceholderText("Optional — the address is used instead")
	form.AddRow3("Name", name.QWidget)
	v.AddLayout(form.QLayout)
	v.AddWidget(helpText("One domain, IP address or CIDR range per target. A domain matches " +
		"exactly (any port), or as a wildcard when it contains * or ?; a wildcard takes * and ?; " +
		"a regex must match the whole host; IP and CIDR targets match the address the " +
		"connection reached as well as a host that is an IP.").QWidget)

	v.AddWidget(sectionTitle("Notes"))
	notes := newCodeEditor(editorOptions{title: "Notes"})
	notes.edit.SetPlainText(t.Notes)
	notes.root.SetMinimumHeight(96)
	notes.root.SetMaximumHeight(150)
	v.AddWidget(notes.root)

	v.AddSpacing(4)
	v.AddWidget(sectionTitle("Rules"))
	v.AddWidget(helpText("Which paths on this target to harvest. Rules are matched in order and " +
		"the first one that matches wins, like firewall rules, so put the exceptions above the " +
		"defaults. Each pattern is a regular expression matched against the request target, " +
		"e.g. \"^/admin\". The last rule allows everything else and can't be removed.").QWidget)
	rules := newPathRulesEditor()
	rules.load(t.Rules)
	v.AddWidget(rules.root)

	v.AddSpacing(8)
	v.AddWidget(sectionTitle("Interception"))
	v.AddWidget(helpText("While interception is on (the toggle at the top right, Ctrl+I), hold this " +
		"target's messages so you can edit, forward or drop them. Plugins can hold messages too, " +
		"from their own rules.").QWidget)
	holdRequests := qt.NewQCheckBox3("Hold requests")
	holdRequests.SetChecked(t.Intercept.Requests)
	holdResponses := qt.NewQCheckBox3("Hold responses")
	holdResponses.SetChecked(t.Intercept.Responses)
	holdDNS := qt.NewQCheckBox3("Hold DNS lookups")
	holdDNS.SetChecked(t.Intercept.DNS)
	holdDNS.SetToolTip("Hold the lookups of this target's names as well: the query when requests are " +
		"held (rename what is looked up), the answer when responses are (change the addresses " +
		"connected to). Answers are reused for a minute, so not every connection looks up again.")
	v.AddWidget(holdRequests.QWidget)
	v.AddWidget(holdResponses.QWidget)
	v.AddWidget(holdDNS.QWidget)
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)
	tabs.AddTab(scroll.QWidget, "General")

	// ---- Settings tab ------------------------------------------------------
	remove, accepted := false, false
	onDelete := func() { remove, accepted = true, true; dlg.Accept() }
	if isNew {
		onDelete = nil
	}
	settings := buildTargetSettingsTab(dlg, t, onDelete)
	tabs.AddTab(settings.root, "Settings")

	// Footer: the error from a failed validation, then the buttons.
	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	f := qt.NewQHBoxLayout(footer)
	f.SetContentsMargins(16, 10, 16, 10)
	f.SetSpacing(8)
	errLabel := qt.NewQLabel2()
	setName(errLabel.QObject, "ErrorText")
	errLabel.SetWordWrap(true)
	cancel := qt.NewQPushButton3("Cancel")
	cancel.SetAutoDefault(false)
	save := qt.NewQPushButton3("Save")
	if isNew {
		save.SetText("Add target")
	}
	save.SetDefault(true)
	setName(save.QObject, "PrimaryButton")
	f.AddWidget2(errLabel.QWidget, 1)
	f.AddWidget(cancel.QWidget)
	f.AddWidget(save.QWidget)
	root.AddWidget(footer)

	// compose builds the edited target from both tabs, keeping the fields the
	// dialog doesn't show (ID, Created).
	compose := func() target.Target {
		out := address.apply(t)
		out.Name = name.Text()
		out.Notes = notes.edit.ToPlainText()
		out.Rules = rules.rules()
		out.Color = settings.color()
		out.Enabled = settings.enabled()
		out.Intercept = target.Intercept{Requests: holdRequests.IsChecked(), Responses: holdResponses.IsChecked(),
			DNS: holdDNS.IsChecked()}
		return out.Normalized()
	}

	var result target.Target
	save.OnClicked(func() {
		next := compose()
		// Validate here so a bad address or regex is reported beside the
		// field that holds it rather than as a failed save afterwards.
		if err := next.Validate(); err != nil {
			errLabel.SetText(sentence(err.Error()))
			return
		}
		result, accepted = next, true
		dlg.Accept()
	})
	cancel.OnClicked(dlg.Reject)

	if t.Value == "" {
		address.value.SetFocus()
	}
	dlg.Exec()
	dlg.DeleteLater()
	return result, remove, accepted
}

// ---- Target list editor ----------------------------------------------------

// Column widths shared by the target list's header and its rows.
const (
	targetColKind   = 128
	targetColAction = 110
	ruleColMove     = 28
	ruleColRemove   = 28
)

// targetListEditor edits several targets at once as kind/address/name rows.
// It is how the New Engagement wizard takes the engagement's initial
// targets; afterwards each one is edited on its own in runTargetDialog.
type targetListEditor struct {
	root   *qt.QWidget
	list   *qt.QVBoxLayout
	header *qt.QWidget
	empty  *qt.QLabel
	rows   []*targetListRow

	// An imported scope file is kept as a batch rather than as rows: a
	// program's scope runs to hundreds of hosts, which would bury the page
	// and nobody edits one by one here anyway. The summary says what came
	// in and offers to drop it again; the Targets section edits them once
	// the engagement exists.
	imported     []target.Target
	importedFrom string
	summary      *qt.QLabel
	summaryRow   *qt.QWidget
}

type targetListRow struct {
	w       *qt.QWidget
	address *addressEditor
	name    *qt.QLineEdit
}

func (r *targetListRow) widget() *qt.QWidget { return r.w }

func newTargetListEditor() *targetListEditor {
	e := &targetListEditor{root: qt.NewQWidget2()}
	setName(e.root.QObject, "RulesEditor")
	e.root.SetAttribute(qt.WA_StyledBackground)
	v := qt.NewQVBoxLayout(e.root)
	v.SetContentsMargins(10, 8, 10, 10)
	v.SetSpacing(6)

	e.header = headerRowFor([]columnSpec{
		{"Kind", targetColKind}, {"Domain, IP or CIDR", 0},
		{"Name (optional)", targetNameCol}, {"", ruleColRemove},
	})
	v.AddWidget(e.header)
	e.empty = helpText("No targets yet — nothing will be collected until one is added.")
	v.AddWidget(e.empty.QWidget)

	listBox := qt.NewQWidget2()
	e.list = qt.NewQVBoxLayout(listBox)
	e.list.SetContentsMargins(0, 0, 0, 0)
	e.list.SetSpacing(6)
	v.AddWidget(listBox)

	// What an imported scope file brought in, with a way to take it back out.
	e.summaryRow = qt.NewQWidget2()
	sh := qt.NewQHBoxLayout(e.summaryRow)
	sh.SetContentsMargins(0, 0, 0, 0)
	sh.SetSpacing(8)
	e.summary = qt.NewQLabel2()
	e.summary.SetWordWrap(true)
	clear := qt.NewQPushButton3("Remove")
	clear.SetAutoDefault(false)
	clear.OnClicked(func() {
		e.imported, e.importedFrom = nil, ""
		e.refresh()
	})
	sh.AddWidget2(e.summary.QWidget, 1)
	sh.AddWidget(clear.QWidget)
	v.AddWidget(e.summaryRow)

	add := qt.NewQPushButton3("+ Add target")
	add.SetAutoDefault(false)
	add.OnClicked(func() {
		e.addRow(target.Target{Kind: target.KindDomain})
		e.rows[len(e.rows)-1].address.value.SetFocus()
	})
	importBtn := qt.NewQPushButton3("Import scope file…")
	importBtn.SetAutoDefault(false)
	importBtn.OnClicked(e.importScope)
	addRow := qt.NewQHBoxLayout2()
	addRow.AddWidget(add.QWidget)
	addRow.AddWidget(importBtn.QWidget)
	addRow.AddStretch()
	v.AddLayout(addRow.QLayout)
	e.refresh()
	return e
}

// importScope reads a Burp Suite scope file — one saved from Burp, or a
// program's scope downloaded from HackerOne — and keeps its targets as the
// batch this page will create the engagement with. Importing again replaces
// the batch rather than adding to it, so clicking twice can't double the
// scope.
func (e *targetListEditor) importScope() {
	picked, ok := pickScopeFile(e.root)
	if !ok {
		return
	}
	if !confirmScopeImport(e.root, picked) {
		return
	}
	e.imported, e.importedFrom = picked.imp.Targets, picked.name
	e.refresh()
	if s := scopeProblems(picked.imp, ""); s != "" {
		qt.QMessageBox_Warning(e.root, "Imported with problems", s)
	}
}

// targetNameCol is the width of the optional-name column.
const targetNameCol = 180

// targets returns what the page describes: the targets an imported scope file
// brought in, then the typed rows, each with its own colour and a fresh ID.
// Rows with no address are dropped.
func (e *targetListEditor) targets() []target.Target {
	out := make([]target.Target, 0, len(e.imported)+len(e.rows))
	used := make([]string, 0, len(e.imported)+len(e.rows))
	adopt := func(t target.Target) {
		ready := target.Adopt(t, target.PickColor(used))
		out = append(out, ready)
		used = append(used, ready.Color)
	}
	for _, t := range e.imported {
		adopt(t)
	}
	for _, r := range e.rows {
		value := strings.TrimSpace(r.address.value.Text())
		if value == "" {
			continue
		}
		adopt(target.Target{
			Kind:    r.address.selectedKind(),
			Value:   value,
			Name:    strings.TrimSpace(r.name.Text()),
			Enabled: true,
		})
	}
	return out
}

func (e *targetListEditor) addRow(t target.Target) {
	r := &targetListRow{w: qt.NewQWidget2(), address: newAddressEditor(t)}
	h := qt.NewQHBoxLayout(r.w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	r.name = qt.NewQLineEdit3(t.Name)
	r.name.SetFixedWidth(targetNameCol)
	r.name.SetPlaceholderText("Optional")

	remove := newIconButton(iconClose, "Remove this target", func() {
		e.removeRow(r)
		e.refresh()
	}).asDanger()

	h.AddWidget2(r.address.row, 1)
	h.AddWidget(r.name.QWidget)
	h.AddWidget(remove.w)

	e.list.AddWidget(r.w)
	e.rows = append(e.rows, r)
	e.refresh()
}

func (e *targetListEditor) removeRow(r *targetListRow) {
	e.rows, _ = dropRow(e.rows, r)
}

func (e *targetListEditor) refresh() {
	e.header.SetVisible(len(e.rows) > 0)
	e.empty.SetVisible(len(e.rows) == 0 && len(e.imported) == 0)
	e.summaryRow.SetVisible(len(e.imported) > 0)
	if len(e.imported) > 0 {
		e.summary.SetText(fmt.Sprintf("%s from %s will be added.",
			countOf(len(e.imported), "target", "targets"), e.importedFrom))
	}
}

// ---- Path rules editor -----------------------------------------------------

// pathRulesEditor edits the ordered allow/deny rules. The final default rule
// is always shown, always last, and can't be edited or removed — it is what
// every target falls back to.
type pathRulesEditor struct {
	root   *qt.QWidget
	list   *qt.QVBoxLayout
	header *qt.QWidget
	rows   []*pathRuleRow
}

type pathRuleRow struct {
	w       *qt.QWidget
	action  *qt.QComboBox
	pattern *qt.QLineEdit
	up      *iconButton
	down    *iconButton
}

func (r *pathRuleRow) widget() *qt.QWidget { return r.w }

func newPathRulesEditor() *pathRulesEditor {
	e := &pathRulesEditor{root: qt.NewQWidget2()}
	setName(e.root.QObject, "RulesEditor")
	e.root.SetAttribute(qt.WA_StyledBackground)
	v := qt.NewQVBoxLayout(e.root)
	v.SetContentsMargins(10, 8, 10, 10)
	v.SetSpacing(6)

	e.header = headerRowFor([]columnSpec{
		{"Action", targetColAction}, {"Path pattern", 0},
		{"", ruleColMove}, {"", ruleColMove}, {"", ruleColRemove},
	})
	v.AddWidget(e.header)

	listBox := qt.NewQWidget2()
	e.list = qt.NewQVBoxLayout(listBox)
	e.list.SetContentsMargins(0, 0, 0, 0)
	e.list.SetSpacing(6)
	v.AddWidget(listBox)

	add := qt.NewQPushButton3("+ Add rule")
	add.SetAutoDefault(false)
	add.OnClicked(func() {
		// New rules go above the default one, where they can take effect.
		e.insertRow(len(e.rows)-1, target.Rule{Action: target.Deny})
		e.rows[len(e.rows)-2].pattern.SetFocus()
	})
	addRow := qt.NewQHBoxLayout2()
	addRow.AddWidget(add.QWidget)
	addRow.AddStretch()
	v.AddLayout(addRow.QLayout)
	return e
}

func (e *pathRulesEditor) load(rules []target.Rule) {
	for len(e.rows) > 0 {
		e.removeRow(e.rows[0])
	}
	// Normalized guarantees the default rule is last; be defensive anyway so
	// the editor always shows it.
	if len(rules) == 0 || !rules[len(rules)-1].IsDefault() {
		rules = append(append([]target.Rule{}, rules...), target.DefaultRule())
	}
	for _, r := range rules {
		e.insertRow(len(e.rows), r)
	}
	e.refresh()
}

func (e *pathRulesEditor) rules() []target.Rule {
	out := make([]target.Rule, 0, len(e.rows))
	for _, r := range e.rows {
		out = append(out, target.Rule{
			Action:  selectedOption(r.action, targetActionOptions),
			Pattern: strings.TrimSpace(r.pattern.Text()),
		})
	}
	return out
}

// insertRow adds a rule at index. The default rule is locked: its controls
// are read-only and it has no remove button.
func (e *pathRulesEditor) insertRow(index int, rule target.Rule) {
	locked := rule.IsDefault() && index >= len(e.rows)
	r := &pathRuleRow{w: qt.NewQWidget2()}
	h := qt.NewQHBoxLayout(r.w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	r.action = qt.NewQComboBox2()
	fillCombo(r.action, targetActionOptions)
	selectOption(r.action, targetActionOptions, rule.Action)
	r.action.SetFixedWidth(targetColAction)
	r.pattern = qt.NewQLineEdit3(rule.Pattern)
	r.pattern.SetPlaceholderText("^/admin")
	r.pattern.SetFont(monospaceFont())

	r.up = newIconButton(iconUp, "Move this rule up", func() { e.move(r, -1) })
	r.down = newIconButton(iconDown, "Move this rule down", func() { e.move(r, 1) })
	remove := newIconButton(iconClose, "Remove this rule", func() {
		e.removeRow(r)
		e.refresh()
	}).asDanger()

	if locked {
		r.action.SetEnabled(false)
		r.pattern.SetReadOnly(true)
		r.pattern.SetToolTip("Every target ends with this rule; it can't be changed or removed")
		r.action.SetToolTip(r.pattern.ToolTip())
		remove.setEnabled(false)
		r.up.setEnabled(false)
		r.down.setEnabled(false)
	}

	h.AddWidget(r.action.QWidget)
	h.AddWidget2(r.pattern.QWidget, 1)
	h.AddWidget(r.up.w)
	h.AddWidget(r.down.w)
	h.AddWidget(remove.w)

	e.list.InsertWidget(index, r.w)
	e.rows = append(e.rows, nil)
	copy(e.rows[index+1:], e.rows[index:])
	e.rows[index] = r
	e.refresh()
}

// move shifts a rule by delta places, never past the locked default rule.
func (e *pathRulesEditor) move(r *pathRuleRow, delta int) {
	from := -1
	for i, row := range e.rows {
		if row == r {
			from = i
			break
		}
	}
	to := from + delta
	if from < 0 || to < 0 || to >= len(e.rows)-1 { // the default rule stays last
		return
	}
	e.rows[from], e.rows[to] = e.rows[to], e.rows[from]
	e.list.RemoveWidget(r.w)
	e.list.InsertWidget(to, r.w)
	e.refresh()
}

func (e *pathRulesEditor) removeRow(r *pathRuleRow) {
	e.rows, _ = dropRow(e.rows, r)
}

// refresh greys out the move buttons that would do nothing, so the order of
// the list is self-explanatory.
func (e *pathRulesEditor) refresh() {
	last := len(e.rows) - 1 // the default rule
	for i, r := range e.rows {
		if i == last {
			continue
		}
		r.up.setEnabled(i > 0)
		r.down.setEnabled(i < last-1)
	}
}

// columnSpec is one column of an editor's header; width 0 stretches.
type columnSpec struct {
	text  string
	width int
}

func headerRowFor(cols []columnSpec) *qt.QWidget {
	w := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)
	for _, c := range cols {
		l := qt.NewQLabel3(c.text)
		setName(l.QObject, "RulesHeader")
		if c.width > 0 {
			l.SetFixedWidth(c.width)
			h.AddWidget(l.QWidget)
		} else {
			h.AddWidget2(l.QWidget, 1)
		}
	}
	return w
}

// ---- Per-target settings tab -----------------------------------------------

// targetSettingsTab is the Settings tab's widget plus accessors for the
// values the dialog reads back when the user saves.
type targetSettingsTab struct {
	root    *qt.QWidget
	color   func() string
	enabled func() bool
}

// buildTargetSettingsTab builds the Settings tab: the per-target colour, the
// collection switch, and a delete action. onDelete is called when the user
// confirms deletion (it closes the dialog); a nil onDelete leaves the delete
// section out, for a target that hasn't been saved yet.
func buildTargetSettingsTab(dlg *qt.QDialog, t target.Target, onDelete func()) *targetSettingsTab {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(22, 18, 22, 18)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Colour"))
	v.AddWidget(helpText("Marks this target's requests in the request list.").QWidget)

	chosen := t.Color
	swatch := newColorSwatch(chosen, 22)
	picker := qt.NewQHBoxLayout2()
	picker.SetSpacing(6)
	picker.AddWidget(swatch.w)
	picker.AddSpacing(8)
	for _, c := range target.Palette {
		col := c
		b := qt.NewQPushButton2()
		b.SetAutoDefault(false)
		b.SetFixedSize2(24, 24)
		b.SetToolTip(col)
		// min-width has to be overridden: the stylesheet gives every button a
		// 64px floor, which would stretch these into bars.
		b.SetStyleSheet("background:" + col + "; border:1px solid " + hex("border") +
			"; border-radius:6px; min-width:0px; padding:0px;")
		b.OnClicked(func() {
			chosen = col
			swatch.setColor(col)
		})
		picker.AddWidget(b.QWidget)
	}
	picker.AddStretch()
	v.AddLayout(picker.QLayout)

	custom := qt.NewQPushButton3("Choose a colour…")
	custom.SetAutoDefault(false)
	custom.OnClicked(func() {
		c := qt.QColorDialog_GetColor3(qt.NewQColor6(chosen), dlg.QWidget, "Target colour")
		if c != nil && c.IsValid() {
			chosen = strings.ToLower(c.Name())
			swatch.setColor(chosen)
		}
	})
	customRow := qt.NewQHBoxLayout2()
	customRow.AddWidget(custom.QWidget)
	customRow.AddStretch()
	v.AddLayout(customRow.QLayout)

	v.AddSpacing(8)
	v.AddWidget(sectionTitle("Collection"))
	enabled := qt.NewQCheckBox3("Collect traffic for this target")
	enabled.SetChecked(t.Enabled)
	v.AddWidget(enabled.QWidget)
	v.AddWidget(helpText("Switching this off parks the target: it stops matching traffic " +
		"without being deleted, and the requests already captured under it stay.").QWidget)

	if onDelete != nil {
		v.AddSpacing(8)
		v.AddWidget(sectionTitle("Delete"))
		v.AddWidget(helpText("Requests already captured under this target are kept, but new " +
			"traffic will no longer match it.").QWidget)
		del := qt.NewQPushButton3("Delete target…")
		del.SetAutoDefault(false)
		del.OnClicked(func() {
			answer := qt.QMessageBox_Question6(dlg.QWidget, "Delete target",
				"Delete \""+t.Label()+"\"?\n\nRequests already captured under it are kept, but new "+
					"traffic will no longer match it.",
				qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
			if answer == qt.QMessageBox__Yes {
				onDelete()
			}
		})
		delRow := qt.NewQHBoxLayout2()
		delRow.AddWidget(del.QWidget)
		delRow.AddStretch()
		v.AddLayout(delRow.QLayout)
	}
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)

	return &targetSettingsTab{
		root:    scroll.QWidget,
		color:   func() string { return chosen },
		enabled: func() bool { return enabled.IsChecked() },
	}
}
