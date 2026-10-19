package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/findings"
)

// The Create Finding window: one dialog for recording a finding and for
// editing one, so a finding's parts are entered the same way whichever it
// is.
//
// It opens from three places: the right-click menu in a message (with the
// value and the sighting already filled in — see editormenu.go), the
// Findings section's + button, and the detail pane's Edit… button.
//
// Recording a value that is already a finding does not make a second one:
// the store merges it and the dialog says so, which is the dedup rule made
// visible rather than silently surprising.

// findingDialog is the window's widgets while it is open.
type findingDialog struct {
	dlg   *qt.QDialog
	store *findings.Store

	value   *codeEditor
	hidden  *qt.QCheckBox
	conf    *scoreField
	concern *scoreField
	notes   *codeEditor
	types   *qt.QListWidget
	fields  *qt.QWidget
	rows    []*fieldEditRow
	err     *qt.QLabel
}

// fieldEditRow is one editable key/value entry of a finding's own store.
type fieldEditRow struct {
	w     *qt.QWidget
	key   *qt.QLineEdit
	value *qt.QLineEdit
}

// runFindingDialog shows the window and returns the finding it saved.
//
// For a new finding, draft carries what is already known: the value taken
// from the user's selection, the request it was seen in, and the source.
// For an edit, existing is the finding and draft is ignored.
func runFindingDialog(parent *qt.QWidget, store *findings.Store, draft findings.Draft,
	existing *findings.Finding) (findings.Finding, bool) {

	d := &findingDialog{dlg: qt.NewQDialog(parent), store: store}
	editing := existing != nil
	title := "Create Finding"
	if editing {
		title = "Edit Finding"
	}
	d.dlg.SetWindowTitle(title)
	d.dlg.Resize(640, 620)
	d.dlg.SetMinimumSize2(520, 460)

	root := qt.NewQVBoxLayout(d.dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(20, 18, 20, 14)
	v.SetSpacing(14)

	// Value.
	// The editors are capped: a long value mustn't push the types, the
	// notes and the Save button off the window.
	d.value = newCodeEditor(editorOptions{title: "Value", wrap: true})
	d.value.edit.SetMinimumHeight(60)
	d.value.edit.SetMaximumHeight(110)
	d.value.edit.SetPlaceholderText("The token, the leaked field, the response that shouldn't have come back")
	v.AddWidget(d.value.root)

	// Hiding the value is the finding's own setting, not a view's: a
	// password stays masked in the list and the detail pane, on this
	// machine and the next time the engagement is opened.
	d.hidden = qt.NewQCheckBox3("Keep this value hidden until I click the eye")
	d.hidden.SetToolTip("For a value that is a secret in itself: a password, a key, a session token, a card number")
	v.AddWidget(d.hidden.QWidget)

	// Two assessments, each a gated 0–100 slider: how likely the finding is a
	// true positive (confidence), and how big an issue it is (concern). Each
	// is left unset until ticked, so the findings agent can assess it; a
	// finding both unlikely and low-concern is marked read automatically.
	d.conf = newScoreField("Confidence", "I've reviewed this finding",
		"Leave unticked to let the findings agent review and score it. Tick to set a confidence yourself.",
		findings.ConfidenceUnknown, confidenceText)
	d.concern = newScoreField("Concern", "I've assessed the concern",
		"Leave unticked to let the findings agent assess it. Tick to set a concern yourself (0 none, 100 critical).",
		0, concernText)
	v.AddWidget(d.conf.box)
	v.AddWidget(d.concern.box)

	// Types: the user's catalog, as checkboxes.
	catalog := store.Types()
	v.AddWidget(sectionTitle("Types"))
	if len(catalog) == 0 {
		v.AddWidget(helpText("No finding types are configured yet. Add some in " +
			"Settings → Findings to classify your findings.").QWidget)
	}
	d.types = qt.NewQListWidget2()
	setName(d.types.QObject, "TypeChecklist")
	d.types.SetFrameShape(qt.QFrame__NoFrame)
	d.types.SetMaximumHeight(118)
	d.types.SetVisible(len(catalog) > 0)
	for _, t := range catalog {
		item := qt.NewQListWidgetItem2(t.Name)
		item.SetFlags(item.Flags() | qt.ItemIsUserCheckable)
		item.SetCheckState(qt.Unchecked)
		d.types.AddItemWithItem(item)
	}
	v.AddWidget(d.types.QWidget)

	// Notes.
	d.notes = newCodeEditor(editorOptions{title: "Notes", wrap: true})
	d.notes.edit.SetMinimumHeight(90)
	d.notes.edit.SetMaximumHeight(160)
	d.notes.edit.SetPlaceholderText("What this is and why it matters")
	v.AddWidget(d.notes.root)

	// Details: the finding's own key/value store, empty by default.
	v.AddWidget(sectionTitle("Details"))
	v.AddWidget(helpText("Anything else worth keeping with the finding, as name and value.").QWidget)
	d.fields = qt.NewQWidget2()
	fields := qt.NewQVBoxLayout(d.fields)
	fields.SetContentsMargins(0, 0, 0, 0)
	fields.SetSpacing(4)
	v.AddWidget(d.fields)
	add := qt.NewQPushButton3("+ Add detail")
	add.SetAutoDefault(false)
	setName(add.QObject, "LinkButton")
	add.OnClicked(func() { d.addField(findings.Field{}).key.SetFocus() })
	addRow := qt.NewQHBoxLayout2()
	addRow.AddWidget(add.QWidget)
	addRow.AddStretch()
	v.AddLayout(addRow.QLayout)

	// Where it was seen. It is shown, not edited: the sighting comes from
	// the message the finding was raised in.
	if seen := seenText(draft, existing); seen != "" {
		v.AddWidget(sectionTitle("Seen in"))
		label := helpText(seen)
		label.SetTextInteractionFlags(qt.TextSelectableByMouse)
		v.AddWidget(label.QWidget)
	}
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)
	root.AddWidget2(scroll.QWidget, 1)

	// Seed the fields.
	if editing {
		d.load(*existing, catalog)
	} else {
		d.value.setText(draft.Value)
		d.hidden.SetChecked(draft.Hidden)
		d.conf.seed(draft.Confidence)
		d.concern.seed(draft.Concern)
		d.notes.setText(draft.Notes)
		d.checkTypes(draft.Types, catalog)
		for _, kv := range draft.Fields {
			d.addField(kv)
		}
	}

	// Footer.
	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	f := qt.NewQHBoxLayout(footer)
	f.SetContentsMargins(16, 10, 16, 10)
	f.SetSpacing(8)
	d.err = qt.NewQLabel2()
	setName(d.err.QObject, "ErrorText")
	d.err.SetWordWrap(true)
	cancel := qt.NewQPushButton3("Cancel")
	cancel.SetAutoDefault(false)
	cancel.OnClicked(d.dlg.Reject)
	save := qt.NewQPushButton3("Save")
	setName(save.QObject, "PrimaryButton")
	save.SetDefault(true)
	f.AddWidget2(d.err.QWidget, 1)
	f.AddWidget(cancel.QWidget)
	f.AddWidget(save.QWidget)
	root.AddWidget(footer)

	var saved findings.Finding
	var ok bool
	save.OnClicked(func() {
		result, err := d.commit(draft, existing)
		if err != nil {
			d.err.SetText(sentence(err.Error()))
			return
		}
		saved, ok = result, true
		d.dlg.Accept()
	})

	d.value.edit.SetFocus()
	d.dlg.Exec()
	d.dlg.DeleteLater()
	return saved, ok
}

// load fills the widgets from an existing finding.
func (d *findingDialog) load(f findings.Finding, catalog []findings.Type) {
	d.value.setText(f.Value)
	d.hidden.SetChecked(f.Hidden)
	d.conf.seed(f.Confidence)
	d.concern.seed(f.Concern)
	d.notes.setText(f.Notes)
	d.checkTypes(f.Types, catalog)
	for _, kv := range f.Fields {
		d.addField(kv)
	}
}

// checkTypes ticks the types a finding carries. One no longer in the catalog
// has no row to tick; the finding keeps it either way, because commit only
// replaces the types when the ticks have actually been changed.
func (d *findingDialog) checkTypes(ids []string, catalog []findings.Type) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for i, t := range catalog {
		if want[t.ID] {
			d.types.Item(i).SetCheckState(qt.Checked)
		}
	}
}

// chosenTypes returns the ticked type IDs, in catalog order.
func (d *findingDialog) chosenTypes(catalog []findings.Type) []string {
	var out []string
	for i, t := range catalog {
		if item := d.types.Item(i); item != nil && item.CheckState() == qt.Checked {
			out = append(out, t.ID)
		}
	}
	return out
}

func (d *findingDialog) addField(kv findings.Field) *fieldEditRow {
	r := &fieldEditRow{w: qt.NewQWidget2()}
	h := qt.NewQHBoxLayout(r.w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(6)

	r.key = qt.NewQLineEdit3(kv.Key)
	r.key.SetPlaceholderText("Name")
	r.key.SetFixedWidth(160)
	r.value = qt.NewQLineEdit3(kv.Value)
	r.value.SetPlaceholderText("Value")
	remove := newIconButton(iconClose, "Remove this detail", func() { d.removeField(r) }).asDanger()

	h.AddWidget(r.key.QWidget)
	h.AddWidget2(r.value.QWidget, 1)
	h.AddWidget(remove.w)

	d.fields.Layout().AddWidget(r.w)
	d.rows = append(d.rows, r)
	return r
}

func (d *findingDialog) removeField(r *fieldEditRow) {
	for i, row := range d.rows {
		if row == r {
			d.rows = append(d.rows[:i], d.rows[i+1:]...)
			break
		}
	}
	r.w.SetParent(nil)
	r.w.DeleteLater()
}

// fields returns the key/value entries as typed. A row with no name is
// dropped by the store, since a value nobody can look up is noise.
func (d *findingDialog) fieldValues() []findings.Field {
	out := make([]findings.Field, 0, len(d.rows))
	for _, r := range d.rows {
		out = append(out, findings.Field{
			Key:   strings.TrimSpace(r.key.Text()),
			Value: strings.TrimSpace(r.value.Text()),
		})
	}
	return out
}

// scoreField is a nullable 0–100 assessment control used for both confidence
// and concern: an "assessed" tick that, unticked, stores no value (so the
// findings agent will assess it), and when ticked enables a slider shown in
// words. The default value the slider starts at and the words come from the
// caller, so confidence and concern read in their own terms.
type scoreField struct {
	box    *qt.QWidget
	check  *qt.QCheckBox
	slider *qt.QSlider
	label  *qt.QLabel
	def    int
	format func(*int) string
}

func newScoreField(title, assess, tip string, def int, format func(*int) string) *scoreField {
	sf := &scoreField{def: def, format: format}
	sf.box = qt.NewQWidget2()
	outer := qt.NewQVBoxLayout(sf.box)
	outer.SetContentsMargins(0, 0, 0, 0)
	outer.SetSpacing(4)
	outer.AddWidget(sectionTitle(title))

	sf.check = qt.NewQCheckBox3(assess)
	sf.check.SetToolTip(tip)
	sf.check.OnToggled(func(on bool) {
		sf.slider.SetEnabled(on)
		sf.update()
	})
	outer.AddWidget(sf.check.QWidget)

	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(10)
	sf.slider = qt.NewQSlider3(qt.Horizontal)
	sf.slider.SetRange(findings.ConfidenceMin, findings.ConfidenceMax)
	sf.slider.SetSingleStep(5)
	sf.slider.SetPageStep(10)
	sf.slider.SetTickPosition(qt.QSlider__TicksBelow)
	sf.slider.SetTickInterval(10)
	sf.slider.SetValue(def)
	sf.slider.SetEnabled(false) // not assessed by default
	sf.label = qt.NewQLabel3(format(nil))
	setName(sf.label.QObject, "Muted")
	sf.label.SetFixedWidth(150)
	sf.slider.OnValueChanged(func(int) { sf.update() })
	h.AddWidget2(sf.slider.QWidget, 1)
	h.AddWidget(sf.label.QWidget)
	outer.AddWidget(row)
	return sf
}

// update keeps the words beside the slider in step with it.
func (sf *scoreField) update() { sf.label.SetText(sf.format(sf.value())) }

// value is the score the field represents: nil when unticked, else the slider.
func (sf *scoreField) value() *int {
	if !sf.check.IsChecked() {
		return nil
	}
	v := sf.slider.Value()
	return &v
}

// seed sets the tick and the slider from a stored score, leaving both off for
// a finding that has none.
func (sf *scoreField) seed(c *int) {
	on := c != nil
	sf.check.SetChecked(on)
	sf.slider.SetEnabled(on)
	if on {
		sf.slider.SetValue(*c)
	} else {
		sf.slider.SetValue(sf.def)
	}
	sf.update()
}

// commit records the new finding, or applies the edit to the existing one.
func (d *findingDialog) commit(draft findings.Draft, existing *findings.Finding) (findings.Finding, error) {
	catalog := d.store.Types()
	value, notes := d.value.text(), d.notes.text()
	types, fields := d.chosenTypes(catalog), d.fieldValues()

	if existing == nil {
		draft.Value, draft.Notes, draft.Types, draft.Fields = value, notes, types, fields
		draft.Hidden = d.hidden.IsChecked()
		draft.Confidence = d.conf.value()
		draft.Concern = d.concern.value()
		if draft.Source == "" {
			draft.Source = findings.SourceUser
		}
		f, created, err := d.store.Record(draft)
		if err != nil {
			return findings.Finding{}, err
		}
		if !created {
			// The dedup rule, said out loud: the user should know their
			// finding joined one that was already there.
			qt.QMessageBox_Information(d.dlg.QWidget, "Added to an existing finding",
				"That value was already recorded as \""+f.SummaryMasked()+"\". This sighting was "+
					"added to it, with your notes and types.")
		}
		return f, nil
	}

	// An edit, applied one part at a time so the message names what failed.
	id := existing.ID
	if value != existing.Value {
		if err := d.store.SetValue(id, value); err != nil {
			return findings.Finding{}, err
		}
	}
	if !sameStrings(types, keptTypes(*existing, catalog, types)) {
		if err := d.store.Classify(id, keptTypes(*existing, catalog, types)); err != nil {
			return findings.Finding{}, err
		}
	}
	if notes != existing.Notes {
		if err := d.store.SetNotes(id, notes); err != nil {
			return findings.Finding{}, err
		}
	}
	if err := d.store.SetFields(id, fields); err != nil {
		return findings.Finding{}, err
	}
	if hidden := d.hidden.IsChecked(); hidden != existing.Hidden {
		if err := d.store.SetHidden(id, hidden); err != nil {
			return findings.Finding{}, err
		}
	}
	if conf := d.conf.value(); !sameScore(conf, existing.Confidence) {
		if err := d.store.SetConfidence(id, conf); err != nil {
			return findings.Finding{}, err
		}
	}
	if concern := d.concern.value(); !sameScore(concern, existing.Concern) {
		if err := d.store.SetConcern(id, concern); err != nil {
			return findings.Finding{}, err
		}
	}
	f, _ := d.store.Get(id)
	return f, nil
}

// sameScore reports whether two optional scores are equal, both nil (unset)
// included.
func sameScore(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// keptTypes is the finding's new type list: what the checklist says, plus
// any type it already carried that the catalog no longer offers. A type
// removed from Settings → Findings has no checkbox, and editing a finding's
// notes must not quietly strip it.
func keptTypes(f findings.Finding, catalog []findings.Type, chosen []string) []string {
	known := make(map[string]bool, len(catalog))
	for _, t := range catalog {
		known[t.ID] = true
	}
	out := append([]string(nil), chosen...)
	for _, id := range f.Types {
		if !known[id] {
			out = append(out, id)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seenText describes the sighting the dialog was opened with, or the
// sightings an existing finding already has.
func seenText(draft findings.Draft, existing *findings.Finding) string {
	if existing != nil {
		labels := make([]string, 0, len(existing.Links))
		for _, l := range existing.Links {
			labels = append(labels, l.Label())
		}
		return strings.Join(labels, "\n")
	}
	return draft.Link.Label()
}
