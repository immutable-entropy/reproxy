package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/capture"
)

// openEntityRequests opens a window listing flows (the requests made to
// one URL of the Explorer, or to the URLs under a host or domain), newest
// first, with a button that opens the selected one in Replay. The window
// isn't modal, so several can stay open beside the main window, and it
// deletes itself when closed.
func openEntityRequests(parent *qt.QWidget, entity string, flows []capture.Flow, store *capture.Store, onReplay func(capture.Flow)) {
	dlg := qt.NewQDialog(parent)
	dlg.SetAttribute(qt.WA_DeleteOnClose)
	dlg.SetWindowTitle("Requests · " + entity)
	dlg.Resize(860, 460)

	v := qt.NewQVBoxLayout(dlg.QWidget)
	v.SetContentsMargins(16, 14, 16, 14)
	v.SetSpacing(10)

	title := qt.NewQLabel3(fmt.Sprintf("%s to %s", countOf(len(flows), "request", "requests"), entity))
	setName(title.QObject, "PanelTitle")
	title.SetTextInteractionFlags(qt.TextSelectableByMouse)
	v.AddWidget(title.QWidget)

	list := qt.NewQTreeWidget2()
	setName(list.QObject, "RequestsTable")
	list.SetRootIsDecorated(false)
	list.SetUniformRowHeights(true)
	list.SetAllColumnsShowFocus(true)
	list.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	cols := []string{"#", "Method", "URL", "Status", "Time", "Duration", "Size"}
	list.SetColumnCount(len(cols))
	list.SetHeaderLabels(cols)
	header := list.Header()
	header.SetStretchLastSection(false)
	for i := range cols {
		header.SetSectionResizeMode2(i, qt.QHeaderView__ResizeToContents)
	}
	header.SetSectionResizeMode2(2, qt.QHeaderView__Stretch)

	ids := make(map[uintptr]uint64, len(flows))
	for i := len(flows) - 1; i >= 0; i-- {
		f := flows[i]
		status, tone := statusLabel(f)
		if f.StatusCode != 0 {
			status = f.StatusText
		}
		duration, size := "", ""
		if f.State == capture.StateComplete {
			duration, size = formatDuration(f.Duration), capture.FormatBytes(f.Response.Size)
		}
		item := qt.NewQTreeWidgetItem3(list)
		for c, text := range []string{fmt.Sprint(f.ID), f.Method, f.URL(), status,
			f.Started.Format("15:04:05"), duration, size} {
			item.SetText(c, text)
		}
		item.SetForeground(1, qt.NewQBrush3(color(methodColor(f.Method))))
		item.SetForeground(3, qt.NewQBrush3(color(tone)))
		item.SetToolTip(2, f.URL())
		ids[uintptr(item.UnsafePointer())] = f.ID
	}
	v.AddWidget2(list.QWidget, 1)

	replay := qt.NewQPushButton3("Open in Replay")
	replay.SetDefault(true)
	replay.SetToolTip("Open the selected request in the Replay tool to edit and resend it")
	closeBtn := qt.NewQPushButton3("Close")
	closeBtn.OnClicked(func() { dlg.Close() })

	open := func() {
		item := list.CurrentItem()
		if item == nil {
			return
		}
		// The store has the flow as it is now, not as the window opened.
		if f, ok := store.Get(ids[uintptr(item.UnsafePointer())]); ok {
			onReplay(f)
		}
	}
	replay.OnClicked(open)
	list.OnItemDoubleClicked(func(*qt.QTreeWidgetItem, int) { open() })
	list.OnCurrentItemChanged(func(current, _ *qt.QTreeWidgetItem) { replay.SetEnabled(current != nil) })

	buttons := qt.NewQHBoxLayout2()
	buttons.AddStretch()
	buttons.AddWidget(closeBtn.QWidget)
	buttons.AddWidget(replay.QWidget)
	v.AddLayout(buttons.QLayout)

	replay.SetEnabled(false)
	if list.TopLevelItemCount() > 0 {
		list.SetCurrentItem(list.TopLevelItem(0))
	}
	dlg.Show()
}
