package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"
)

// scoreFilter is one of the Findings page's noise filters: a split button
// that hides findings scored below a cutoff on one axis (confidence, for
// "Hide False Positives", or concern, for "Hide Low Concern"). The button
// face toggles the filter, and reads as pressed while it is on; the
// drop-down beside it holds a slider for the cutoff, which stays open while
// the slider is dragged. The pane reads min() and rebuilds when changed
// fires.
type scoreFilter struct {
	button  *qt.QToolButton
	menu    *qt.QMenu
	slider  *qt.QSlider
	valLbl  *qt.QLabel
	spec    scoreFilterSpec
	cutoff  int
	changed func()
}

// scoreFilterSpec is what tells one score filter from another.
type scoreFilterSpec struct {
	label    string // the button's face: "Hide False Positives"
	what     string // the score, in tooltips: "confidence"
	heading  string // above the slider: "Hide findings scored below"
	help     string // under the slider: what the scale means
	min, max int    // the score's range
}

// newScoreFilter builds the control with the given default cutoff, on.
func newScoreFilter(spec scoreFilterSpec, cutoff int, changed func()) *scoreFilter {
	f := &scoreFilter{spec: spec, cutoff: cutoff, changed: changed}

	f.button = qt.NewQToolButton2()
	setName(f.button.QObject, "ComboButton")
	f.button.SetToolButtonStyle(qt.ToolButtonTextOnly)
	f.button.SetText(spec.label)
	// MenuButtonPopup splits the control: the main face toggles the filter,
	// and the arrow drops the slider.
	f.button.SetPopupMode(qt.QToolButton__MenuButtonPopup)
	f.button.SetCheckable(true)
	f.button.SetChecked(true) // before wiring, so building fires nothing
	f.button.OnToggled(func(bool) {
		f.update()
		f.changed()
	})

	f.menu = qt.NewQMenu2()
	box := qt.NewQWidget2()
	bl := qt.NewQVBoxLayout(box)
	bl.SetContentsMargins(12, 8, 12, 8)
	bl.SetSpacing(6)
	bl.AddWidget(sectionTitle(spec.heading))

	row := qt.NewQWidget2()
	rl := qt.NewQHBoxLayout(row)
	rl.SetContentsMargins(0, 0, 0, 0)
	rl.SetSpacing(10)
	f.slider = qt.NewQSlider3(qt.Horizontal)
	f.slider.SetRange(spec.min, spec.max)
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
		if f.active() {
			f.changed()
		}
	})
	rl.AddWidget2(f.slider.QWidget, 1)
	rl.AddWidget(f.valLbl.QWidget)
	bl.AddWidget(row)
	bl.AddWidget(helpText(spec.help).QWidget)

	act := qt.NewQWidgetAction(f.menu.QObject)
	act.SetDefaultWidget(box)
	f.menu.AddAction(act.QAction)
	f.button.SetMenu(f.menu)

	f.update()
	return f
}

func (f *scoreFilter) active() bool { return f.button.IsChecked() }

// min is the cutoff the pane should apply: the chosen value while the
// filter is on, or zero (keep everything) while it is off.
func (f *scoreFilter) min() int {
	if !f.active() {
		return 0
	}
	return f.cutoff
}

func (f *scoreFilter) widget() *qt.QWidget { return f.button.QWidget }

// update says in the tooltip what the filter is doing; the face keeps its
// label, and its pressed look is what says the filter is on.
func (f *scoreFilter) update() {
	if f.active() {
		f.button.SetToolTip(fmt.Sprintf("On: hiding findings whose %s is scored below %d. "+
			"Click to show them; use the arrow to change the cutoff.", f.spec.what, f.cutoff))
	} else {
		f.button.SetToolTip(fmt.Sprintf("Off: showing findings whatever their %s. "+
			"Click to hide those scored below %d; use the arrow to change the cutoff.", f.spec.what, f.cutoff))
	}
}
