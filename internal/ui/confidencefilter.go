package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/findings"
)

// confidenceFilter is the Findings page's noise filter: a split button that
// hides findings scored below a cutoff. The button face toggles the filter
// off and on (clicking it is how the whole filter is switched off), and the
// drop-down beside it holds a slider for the cutoff, which stays open while
// the slider is dragged. The pane reads minConfidence() and rebuilds when
// changed fires.
type confidenceFilter struct {
	button  *qt.QToolButton
	menu    *qt.QMenu
	slider  *qt.QSlider
	valLbl  *qt.QLabel
	active  bool
	cutoff  int
	changed func()
}

// newConfidenceFilter builds the control with the given default cutoff, on.
func newConfidenceFilter(cutoff int, changed func()) *confidenceFilter {
	f := &confidenceFilter{active: true, cutoff: cutoff, changed: changed}

	f.button = qt.NewQToolButton2()
	setName(f.button.QObject, "ComboButton")
	f.button.SetToolButtonStyle(qt.ToolButtonTextOnly)
	// MenuButtonPopup splits the control: the main face fires OnClicked (which
	// toggles the filter), and the arrow drops the slider.
	f.button.SetPopupMode(qt.QToolButton__MenuButtonPopup)
	f.button.OnClicked(func() { f.setActive(!f.active) })

	f.menu = qt.NewQMenu2()
	box := qt.NewQWidget2()
	bl := qt.NewQVBoxLayout(box)
	bl.SetContentsMargins(12, 8, 12, 8)
	bl.SetSpacing(6)
	bl.AddWidget(sectionTitle("Hide findings scored below"))

	row := qt.NewQWidget2()
	rl := qt.NewQHBoxLayout(row)
	rl.SetContentsMargins(0, 0, 0, 0)
	rl.SetSpacing(10)
	f.slider = qt.NewQSlider3(qt.Horizontal)
	f.slider.SetRange(findings.ConfidenceMin, findings.ConfidenceMax)
	f.slider.SetSingleStep(5)
	f.slider.SetPageStep(10)
	f.slider.SetTickPosition(qt.QSlider__TicksBelow)
	f.slider.SetTickInterval(10)
	f.slider.SetFixedWidth(220)
	// Set the value before wiring the signal, so building the control doesn't
	// fire changed() before the pane it belongs to is ready.
	f.slider.SetValue(cutoff)
	f.valLbl = qt.NewQLabel3(fmt.Sprint(cutoff))
	f.valLbl.SetFixedWidth(28)
	f.slider.OnValueChanged(func(v int) {
		f.cutoff = v
		f.valLbl.SetText(fmt.Sprint(v))
		f.update()
		if f.active {
			f.changed()
		}
	})
	rl.AddWidget2(f.slider.QWidget, 1)
	rl.AddWidget(f.valLbl.QWidget)
	bl.AddWidget(row)
	bl.AddWidget(helpText("50 is unknown. Below leans false positive; above leans true positive. " +
		"Agents score new findings as they review them.").QWidget)

	act := qt.NewQWidgetAction(f.menu.QObject)
	act.SetDefaultWidget(box)
	f.menu.AddAction(act.QAction)
	f.button.SetMenu(f.menu)

	f.update()
	return f
}

// setActive turns the filter off or on and tells the pane to rebuild.
func (f *confidenceFilter) setActive(on bool) {
	f.active = on
	f.update()
	f.changed()
}

// minConfidence is the cutoff the pane should apply: the chosen value while
// the filter is on, or zero (keep everything) while it is off.
func (f *confidenceFilter) minConfidence() int {
	if !f.active {
		return 0
	}
	return f.cutoff
}

func (f *confidenceFilter) widget() *qt.QWidget { return f.button.QWidget }

// update puts the state on the button: what it is hiding, or that it is off.
func (f *confidenceFilter) update() {
	if f.active {
		f.button.SetText(fmt.Sprintf("Hiding below %d", f.cutoff))
		f.button.SetToolTip("Hiding findings scored below this confidence. Click to show all; " +
			"use the arrow to change the cutoff.")
	} else {
		f.button.SetText("Showing all")
		f.button.SetToolTip("Showing every finding, whatever its confidence. Click to hide the " +
			"low-confidence noise; use the arrow to set the cutoff.")
	}
}
