package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"
)

// checkFilter is the drop-down of checkboxes shared by the sections that
// narrow a list by category: the Explorer's entity types and content kinds,
// and the Traffic panel's request methods. The checkboxes sit in the menu
// as widgets, so ticking one leaves the menu open for the next, and Select
// All / Select None are there for the whole list at once.
//
// It has two faces. With a label it is a ComboButton summing up what is
// shown ("3 of 8 methods"); with an icon (newIconCheckFilter) it is one of
// a header's icons, tinted while it is hiding something, with the summary
// as its tooltip.

// filterEntry is one checkbox of a checkFilter.
type filterEntry struct {
	id, label string
	color     string // palette key of a swatch beside the label; "" for none
}

// checkFilter is a drop-down of checkboxes, one per entry, summed up on its
// button. The checkboxes sit in the menu as widgets, so ticking one leaves
// the menu open for the next.
type checkFilter struct {
	// Exactly one of button and icon carries the filter: a labelled
	// ComboButton, or a header icon (see newIconCheckFilter).
	button  *qt.QToolButton
	icon    *iconButton
	menu    *qt.QMenu
	tip     string
	entries []filterEntry
	boxes   map[string]*qt.QCheckBox
	hidden  map[string]bool
	summary func(hidden int) string
	changed func(id string, shown bool)
}

func newCheckFilter(tip string, entries []filterEntry, changed func(string, bool), summary func(int) string) *checkFilter {
	f := newFilterMenu(tip, entries, changed, summary)
	f.button = qt.NewQToolButton2()
	setName(f.button.QObject, "ComboButton")
	f.button.SetPopupMode(qt.QToolButton__InstantPopup)
	f.button.SetToolButtonStyle(qt.ToolButtonTextOnly)
	f.button.SetToolTip(tip)
	f.button.SetMenu(f.menu)
	f.update()
	return f
}

// newIconCheckFilter is the same filter behind one of a panel header's
// icons: clicking it drops the checkboxes under it, and the icon is tinted
// while anything is hidden, so a narrowed list is visible without opening
// the menu. The summary is its tooltip.
func newIconCheckFilter(icon iconKind, tip string, entries []filterEntry,
	changed func(string, bool), summary func(int) string) *checkFilter {
	f := newFilterMenu(tip, entries, changed, summary)
	f.icon = newIconButton(icon, tip, nil)
	f.icon.onClick = f.popup
	// The menu grabs the mouse while it is open, so the icon never hears
	// the pointer leave: it is asked again once the menu is gone.
	f.menu.OnAboutToHide(func() { f.icon.setHovered(f.icon.w.UnderMouse()) })
	f.update()
	return f
}

// popup drops the menu under the icon, left edges aligned.
func (f *checkFilter) popup() {
	b := f.icon.w
	f.menu.Popup(b.MapToGlobalWithQPoint(qt.NewQPoint2(0, b.Height())))
}

// widget is the filter's control, whichever face it has.
func (f *checkFilter) widget() *qt.QWidget {
	if f.icon != nil {
		return f.icon.w
	}
	return f.button.QWidget
}

// onOpen runs fn each time the menu is about to drop, for counts that are
// only worth working out when they will be read.
func (f *checkFilter) onOpen(fn func()) { f.menu.OnAboutToShow(fn) }

// newFilterMenu builds the menu both faces share.
func newFilterMenu(tip string, entries []filterEntry, changed func(string, bool), summary func(int) string) *checkFilter {
	f := &checkFilter{
		entries: entries, boxes: map[string]*qt.QCheckBox{},
		hidden: map[string]bool{}, summary: summary, changed: changed, tip: tip,
	}

	menu := qt.NewQMenu2()
	// Select All / Select None, as widgets too so the menu stays open for
	// ticking a few back on after clearing them all.
	bulk := qt.NewQWidget2()
	bl := qt.NewQHBoxLayout(bulk)
	bl.SetContentsMargins(8, 4, 12, 2)
	bl.SetSpacing(2)
	var all []string
	for _, e := range entries {
		all = append(all, e.id)
	}
	for _, b := range []struct {
		label string
		hide  []string
	}{{"Select All", nil}, {"Select None", all}} {
		btn := qt.NewQToolButton2()
		setName(btn.QObject, "MenuLink")
		btn.SetText(b.label)
		btn.SetAutoRaise(true)
		btn.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
		btn.OnClicked(func() { f.setHidden(b.hide, true) })
		bl.AddWidget(btn.QWidget)
	}
	bl.AddStretch()
	bulkAction := qt.NewQWidgetAction(menu.QObject)
	bulkAction.SetDefaultWidget(bulk)
	menu.AddAction(bulkAction.QAction)
	menu.AddSeparator()

	for _, e := range entries {
		box := qt.NewQCheckBox3(e.label)
		box.SetChecked(true)
		box.OnToggled(func(on bool) {
			f.hidden[e.id] = !on
			f.update()
			f.changed(e.id, on)
		})
		f.boxes[e.id] = box
		row := qt.NewQWidget2()
		rl := qt.NewQHBoxLayout(row)
		rl.SetContentsMargins(12, 4, 16, 4)
		rl.AddWidget(box.QWidget)
		action := qt.NewQWidgetAction(menu.QObject)
		action.SetDefaultWidget(row)
		menu.AddAction(action.QAction)
	}
	f.menu = menu
	f.refreshTheme()
	return f
}

// setHidden hides exactly the given entries (unknown ones are ignored) and
// shows the rest, telling changed about each when notify is set.
func (f *checkFilter) setHidden(ids []string, notify bool) {
	hide := make(map[string]bool, len(ids))
	for _, id := range ids {
		hide[id] = true
	}
	for _, e := range f.entries {
		box := f.boxes[e.id]
		box.BlockSignals(true)
		box.SetChecked(!hide[e.id])
		box.BlockSignals(false)
		f.hidden[e.id] = hide[e.id]
		if notify {
			f.changed(e.id, !hide[e.id])
		}
	}
	f.update()
}

// hiddenIDs lists the entries hidden, in the filter's order.
func (f *checkFilter) hiddenIDs() []string {
	var out []string
	for _, e := range f.entries {
		if f.hidden[e.id] {
			out = append(out, e.id)
		}
	}
	return out
}

// update puts what is shown on the control: the ComboButton's label, or
// the icon's tooltip and tint.
func (f *checkFilter) update() {
	hidden := len(f.hiddenIDs())
	text := f.summary(hidden)
	if f.icon != nil {
		f.icon.setActive(hidden > 0)
		f.icon.w.SetToolTip(f.tip + " — " + text)
		return
	}
	f.button.SetText(text)
}

// setCounts labels each checkbox with how many things it covers, so the
// user can see what hiding it would hide.
func (f *checkFilter) setCounts(count func(id string) int) {
	for _, e := range f.entries {
		f.boxes[e.id].SetText(fmt.Sprintf("%s (%d)", e.label, count(e.id)))
	}
}

// refreshTheme repaints the entries' swatches.
func (f *checkFilter) refreshTheme() {
	for _, e := range f.entries {
		if e.color != "" {
			f.boxes[e.id].SetIcon(swatchIcon(hex(e.color)))
		}
	}
}

// swatchIcon is a small rounded square of a target's colour.
func swatchIcon(hexColor string) *qt.QIcon {
	const size = 12
	pix := qt.NewQPixmap2(size, size)
	pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
	pt := qt.NewQPainter2(pix.QPaintDevice)
	pt.SetRenderHint(qt.QPainter__Antialiasing)
	pt.SetPenWithStyle(qt.NoPen)
	pt.SetBrush(qt.NewQBrush3(qt.NewQColor6(hexColor)))
	pt.DrawRoundedRect(qt.NewQRectF4(1, 1, size-2, size-2), 3, 3)
	pt.End()
	return qt.NewQIcon2(pix)
}
