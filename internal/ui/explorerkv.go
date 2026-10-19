package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/entities"
)

// runKVDialog edits an entity's key/value store, a row per name and
// value like a finding's details, and returns the pairs to keep. Rows with
// no name are dropped by the store.
func runKVDialog(parent *qt.QWidget, e entities.Entity) ([]entities.Pair, bool) {
	dlg := qt.NewQDialog(parent)
	dlg.SetWindowTitle("Edit Details")
	dlg.Resize(560, 420)

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(20, 16, 20, 16)
	v.SetSpacing(8)
	v.AddWidget(sectionTitle(e.Label()))
	v.AddWidget(helpText("Anything worth keeping with this " + string(e.Type) + ", as name and value. " +
		"Agents and plugins read and write the same store.").QWidget)

	type row struct {
		w          *qt.QWidget
		key, value *qt.QLineEdit
	}
	var rows []*row
	list := qt.NewQWidget2()
	ll := qt.NewQVBoxLayout(list)
	ll.SetContentsMargins(0, 0, 0, 0)
	ll.SetSpacing(4)
	add := func(p entities.Pair) *row {
		r := &row{w: qt.NewQWidget2()}
		h := qt.NewQHBoxLayout(r.w)
		h.SetContentsMargins(0, 0, 0, 0)
		h.SetSpacing(6)
		r.key = qt.NewQLineEdit3(p.Key)
		r.key.SetPlaceholderText("Name")
		r.key.SetFixedWidth(160)
		r.value = qt.NewQLineEdit3(p.Value)
		r.value.SetPlaceholderText("Value")
		remove := newIconButton(iconClose, "Remove this detail", func() {
			for i, have := range rows {
				if have == r {
					rows = append(rows[:i], rows[i+1:]...)
					break
				}
			}
			r.w.SetParent(nil)
			r.w.DeleteLater()
		}).asDanger()
		h.AddWidget(r.key.QWidget)
		h.AddWidget2(r.value.QWidget, 1)
		h.AddWidget(remove.w)
		ll.AddWidget(r.w)
		rows = append(rows, r)
		return r
	}
	for _, p := range e.KV {
		add(p)
	}
	v.AddWidget(list)
	more := qt.NewQPushButton3("+ Add detail")
	more.SetAutoDefault(false)
	setName(more.QObject, "LinkButton")
	more.OnClicked(func() { add(entities.Pair{}).key.SetFocus() })
	moreRow := qt.NewQHBoxLayout2()
	moreRow.AddWidget(more.QWidget)
	moreRow.AddStretch()
	v.AddLayout(moreRow.QLayout)
	v.AddStretch()
	if len(e.KV) == 0 {
		add(entities.Pair{})
	}

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(body)
	root.AddWidget2(scroll.QWidget, 1)

	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	f := qt.NewQHBoxLayout(footer)
	f.SetContentsMargins(16, 10, 16, 10)
	f.SetSpacing(8)
	cancel := qt.NewQPushButton3("Cancel")
	cancel.SetAutoDefault(false)
	cancel.OnClicked(dlg.Reject)
	save := qt.NewQPushButton3("Save")
	setName(save.QObject, "PrimaryButton")
	save.SetDefault(true)
	save.OnClicked(dlg.Accept)
	f.AddStretch()
	f.AddWidget(cancel.QWidget)
	f.AddWidget(save.QWidget)
	root.AddWidget(footer)

	ok := dlg.Exec() == int(qt.QDialog__Accepted)
	var out []entities.Pair
	for _, r := range rows {
		out = append(out, entities.Pair{Key: strings.TrimSpace(r.key.Text()), Value: strings.TrimSpace(r.value.Text())})
	}
	dlg.DeleteLater()
	return out, ok
}
