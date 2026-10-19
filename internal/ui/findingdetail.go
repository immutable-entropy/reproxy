package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/findings"
)

// findingDetail is the pane that slides out of the right of the Findings
// section: everything one finding holds.
//
// It is a reading view. Editing goes through the same dialog that creates a
// finding (Edit…, see findingdialog.go), so there is one place where a
// finding's value, types, notes and key/value entries are entered, whether
// it is being made or changed.
type findingDetail struct {
	root    *qt.QWidget
	actions findingDetailActions
	shown   uint64
	// finding is what is on screen, kept so the value can be redrawn when
	// the eye is clicked; revealed is false again for every finding shown.
	finding  findings.Finding
	revealed bool

	summary      *qt.QLabel
	pills        *qt.QHBoxLayout
	value        *codeEditor
	reveal       *iconButton
	confGauge    *gauge
	concernGauge *gauge
	notes        *codeEditor
	fields       *qt.QVBoxLayout
	seen         *qt.QVBoxLayout
	meta         *qt.QLabel

	// Sections that disappear when the finding has nothing to put in them.
	valueBox  *qt.QWidget
	notesBox  *qt.QWidget
	fieldsBox *qt.QWidget
	seenBox   *qt.QWidget
}

// findingDetailActions are the pane's buttons, wired by the Findings pane.
type findingDetailActions struct {
	onClose    func()
	onEdit     func(id uint64)
	onRemove   func(id uint64)
	onShowFlow func(flowID uint64)
	// onShowFile brings a recovered file into view in the Files section, by
	// its path under the recovered-files root.
	onShowFile func(path string)
}

func newFindingDetail(actions findingDetailActions) *findingDetail {
	d := &findingDetail{actions: actions}

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	d.root = panel.QWidget
	outer := qt.NewQVBoxLayout(d.root)
	outer.SetContentsMargins(1, 1, 1, 1)
	outer.SetSpacing(0)
	outer.AddWidget(d.buildHeader())

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(rowPadX, 12, rowPadX, 12)
	v.SetSpacing(14)

	pillHost := qt.NewQWidget2()
	d.pills = qt.NewQHBoxLayout(pillHost)
	d.pills.SetContentsMargins(0, 0, 0, 0)
	d.pills.SetSpacing(pillSpacing)
	v.AddWidget(pillHost)

	// Two radial gauges for the finding's scores: how likely it is a true
	// positive (confidence) and how big an issue it is (concern). They are
	// set by an agent's review or through Edit.
	d.confGauge = newGauge("Confidence", confidenceColor)
	d.concernGauge = newGauge("Concern", concernColor)
	gaugeHost := qt.NewQWidget2()
	gl := qt.NewQHBoxLayout(gaugeHost)
	gl.SetContentsMargins(0, 2, 0, 2)
	gl.SetSpacing(28)
	gl.AddStretch()
	gl.AddWidget(d.confGauge.root)
	gl.AddWidget(d.concernGauge.root)
	gl.AddStretch()
	v.AddWidget(gaugeHost)

	d.value = newCodeEditor(editorOptions{title: "Value", wrap: true})
	d.value.setReadOnly(true)
	d.value.edit.SetMinimumHeight(60)
	d.value.edit.SetMaximumHeight(140)
	// A hidden finding's value is masked until the eye is clicked, so a
	// password or a session token isn't sitting on screen while someone
	// else is looking (or while the window is being recorded).
	d.reveal = newIconButton(iconEye, revealTip, func() {
		d.revealed = !d.revealed
		d.showValue()
	})
	d.value.addHeaderWidget(d.reveal.w)
	d.valueBox = section("", d.value.root)
	v.AddWidget(d.valueBox)

	d.notes = newCodeEditor(editorOptions{title: "Notes", wrap: true})
	d.notes.setReadOnly(true)
	d.notes.edit.SetMinimumHeight(90)
	d.notes.edit.SetMaximumHeight(220)
	d.notesBox = section("", d.notes.root)
	v.AddWidget(d.notesBox)

	fieldHost := qt.NewQWidget2()
	d.fields = qt.NewQVBoxLayout(fieldHost)
	d.fields.SetContentsMargins(0, 0, 0, 0)
	d.fields.SetSpacing(4)
	d.fieldsBox = section("Details", fieldHost)
	v.AddWidget(d.fieldsBox)

	seenHost := qt.NewQWidget2()
	d.seen = qt.NewQVBoxLayout(seenHost)
	d.seen.SetContentsMargins(0, 0, 0, 0)
	d.seen.SetSpacing(4)
	d.seenBox = section("Seen in", seenHost)
	v.AddWidget(d.seenBox)

	d.meta = qt.NewQLabel2()
	setName(d.meta.QObject, "Muted")
	d.meta.SetWordWrap(true)
	v.AddWidget(d.meta.QWidget)
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "DetailScroll")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)
	outer.AddWidget2(scroll.QWidget, 1)
	return d
}

func (d *findingDetail) buildHeader() *qt.QWidget {
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)

	d.summary = qt.NewQLabel2()
	setName(d.summary.QObject, "PanelTitle")
	d.summary.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)

	edit := qt.NewQPushButton3("Edit…")
	edit.SetAutoDefault(false)
	edit.SetToolTip("Change this finding's value, types, notes or details")
	edit.OnClicked(func() {
		if d.actions.onEdit != nil && d.shown != 0 {
			d.actions.onEdit(d.shown)
		}
	})
	// Delete says so in words: beside a ✕ that closes the pane, a second
	// icon would be read as another way to close it.
	remove := qt.NewQPushButton3("Delete")
	remove.SetAutoDefault(false)
	remove.SetToolTip("Remove this finding from the engagement")
	remove.OnClicked(func() {
		if d.actions.onRemove != nil && d.shown != 0 {
			d.actions.onRemove(d.shown)
		}
	})
	closeBtn := newIconButton(iconClose, "Close (Esc in the list)", func() {
		if d.actions.onClose != nil {
			d.actions.onClose()
		}
	})

	h.AddWidget2(d.summary.QWidget, 1)
	h.AddWidget(edit.QWidget)
	h.AddWidget(remove.QWidget)
	h.AddWidget(closeBtn.w)
	header.SetFixedHeight(edit.SizeHint().Height() + 16)
	return header
}

// show fills the pane from a finding. types is the catalog, so a pill shows
// the type's name and colour (and a type since removed still shows its id).
func (d *findingDetail) show(f findings.Finding, types []findings.Type) {
	d.shown = f.ID
	d.finding, d.revealed = f, false
	d.summary.SetText(f.SummaryMasked())
	d.summary.SetToolTip(f.SummaryMasked())

	d.confGauge.set(f.Confidence)
	d.concernGauge.set(f.Concern)

	clearLayout(d.pills.QLayout)
	if len(f.Types) == 0 {
		d.pills.AddWidget(newPill("Untyped", "").w)
	}
	for _, id := range f.Types {
		label, hex := typeLabel(types, id)
		d.pills.AddWidget(newPill(label, hex).w)
	}
	d.pills.AddStretch()

	d.showValue()
	d.notes.setText(f.Notes)
	d.notesBox.SetVisible(f.Notes != "")

	clearLayout(d.fields.QLayout)
	for _, kv := range f.Fields {
		d.fields.AddWidget(fieldRow(kv))
	}
	d.fieldsBox.SetVisible(len(f.Fields) > 0)

	clearLayout(d.seen.QLayout)
	for _, l := range f.Links {
		d.seen.AddWidget(d.seenRow(l))
	}
	d.seenBox.SetVisible(len(f.Links) > 0)

	d.meta.SetText(findingMeta(f))
}

// The eye's tooltips: it both reveals the value and says whether it is
// showing.
const (
	revealTip = "Show the value (it is hidden because it is a secret)"
	hideTip   = "Hide the value again"
)

// showValue draws the value as the eye leaves it: masked for a hidden
// finding, in full once it has been revealed.
func (d *findingDetail) showValue() {
	f := d.finding
	d.valueBox.SetVisible(f.Value != "")
	d.reveal.w.SetVisible(f.Hidden)
	if !f.Hidden || d.revealed {
		d.value.setText(f.Value)
		d.reveal.setIcon(iconEyeOff)
		d.reveal.w.SetToolTip(hideTip)
		return
	}
	d.value.setText(f.Masked())
	d.reveal.setIcon(iconEye)
	d.reveal.w.SetToolTip(revealTip)
}

// seenRow is one sighting: where the finding was, and a button that brings
// that request back into view. A finding outlives its traffic, so the row
// still reads correctly once the flow is gone — only the button goes.
func (d *findingDetail) seenRow(l findings.Link) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	label := qt.NewQLabel3(l.Label())
	setName(label.QObject, "Muted")
	label.SetToolTip(l.Label())
	label.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	h.AddWidget2(label.QWidget, 1)

	if l.Location == findings.InRecoveredFile && l.Detail != "" && d.actions.onShowFile != nil {
		path := l.Detail
		h.AddWidget(newIconButton(iconFiles, "Open this file in Files", func() {
			d.actions.onShowFile(path)
		}).w)
	} else if l.FlowID != 0 && d.actions.onShowFlow != nil {
		flowID := l.FlowID
		h.AddWidget(newIconButton(iconTraffic, "Show this request in Traffic", func() {
			d.actions.onShowFlow(flowID)
		}).w)
	}
	return row
}

// findingMeta is the line under a finding: who recorded it and when.
func findingMeta(f findings.Finding) string {
	s := fmt.Sprintf("Recorded by %s at %s", findingSource(f), f.Created.Format("15:04:05 on 2 Jan 2006"))
	if f.Updated.After(f.Created) {
		s += ", last changed " + f.Updated.Format("15:04:05")
	}
	return s
}

// fieldRow is one key/value entry of a finding's own store.
func fieldRow(kv findings.Field) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(8)

	key := qt.NewQLabel3(kv.Key)
	setName(key.QObject, "FieldKey")
	key.SetFixedWidth(140)
	key.SetAlignment(qt.AlignRight | qt.AlignTop)
	value := qt.NewQLabel3(kv.Value)
	value.SetWordWrap(true)
	value.SetTextInteractionFlags(qt.TextSelectableByMouse)
	h.AddWidget(key.QWidget)
	h.AddWidget2(value.QWidget, 1)
	return row
}

// section wraps a widget with an optional heading, so the detail pane's
// parts can be shown and hidden as a unit.
func section(title string, body *qt.QWidget) *qt.QWidget {
	box := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(box)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(6)
	if title != "" {
		v.AddWidget(sectionTitle(title))
	}
	v.AddWidget(body)
	return box
}

// clearLayout removes and destroys every widget in a layout, for the parts
// of a view that are rebuilt rather than updated.
func clearLayout(l *qt.QLayout) {
	for l.Count() > 0 {
		item := l.TakeAt(0)
		if w := item.Widget(); w != nil {
			w.SetParent(nil)
			w.DeleteLater()
		}
	}
}

func (d *findingDetail) refreshTheme() {
	d.value.refreshTheme()
	d.notes.refreshTheme()
	// The gauges paint their colours from the theme, so repaint them.
	d.confGauge.dial.Update()
	d.concernGauge.dial.Update()
}
