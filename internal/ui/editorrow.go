package ui

import qt "github.com/mappu/miqt/qt6"

// editorRow is a row in one of the list editors — the target list, a
// target's path rules, the upstream chain, a message's headers. They hold
// different fields, but every one of them is a widget in a vertical list
// that the user can take out again, which is all dropRow needs.
type editorRow interface {
	comparable
	widget() *qt.QWidget
}

// dropRow takes r out of rows and retires its widget, returning the rows
// that are left and whether r was among them. Qt owns the widget until the
// event loop comes back round, so it is hidden now and deleted later; a
// row removed and dropped in the same turn would otherwise still be drawn.
//
// Each editor wrote this loop out for its own row type, which is why they
// are all one call now: five implementations of one operation is five
// places for the next change to miss.
func dropRow[T editorRow](rows []T, r T) ([]T, bool) {
	found := false
	for i, row := range rows {
		if row == r {
			rows = append(rows[:i], rows[i+1:]...)
			found = true
			break
		}
	}
	w := r.widget()
	w.Hide()
	w.DeleteLater()
	return rows, found
}
