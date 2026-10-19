package ui

import (
	"fmt"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/capture"
	"reproxy/internal/replay"
)

// historySummary is the one-line label shown in the history list for a send.
func historySummary(e replay.Entry) string {
	ts := e.At.Format("15:04:05")
	line := requestLine(e.Request)
	who := ""
	if e.By != replay.OwnerUser {
		who = "  · agent"
	}
	if e.Err != "" {
		return fmt.Sprintf("%s  %s → failed%s", ts, line, who)
	}
	return fmt.Sprintf("%s  %s → %s · %s · %s%s", ts, line, e.Response.Status,
		formatDuration(e.Response.Duration), capture.FormatBytes(int64(e.Response.Size)), who)
}

// requestLine extracts "METHOD request-target" from a raw request, so the
// history shows which request produced each response even as it is edited.
func requestLine(raw []byte) string {
	line := string(raw)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if fields := strings.Fields(line); len(fields) >= 2 {
		return fields[0] + " " + fields[1]
	}
	return line
}

// replayHistory is the Replay window's bottom pane: a log of past sends,
// newest first. Selecting one loads its request and response back into the
// editors, so a request can be inspected, tweaked and sent again.
type replayHistory struct {
	root     *qt.QWidget
	stack    *qt.QStackedWidget
	list     *qt.QListWidget
	entries  []replay.Entry // newest first; index matches list row
	onSelect func(replay.Entry)
}

const (
	replayHistoryEmpty = 0
	replayHistoryList  = 1
)

func newReplayHistory(onSelect func(replay.Entry)) *replayHistory {
	h := &replayHistory{
		stack:    qt.NewQStackedWidget2(),
		list:     qt.NewQListWidget2(),
		onSelect: onSelect,
	}

	setName(h.list.QObject, "ReplayHistory")
	h.list.SetFrameShape(qt.QFrame__NoFrame)
	h.list.OnCurrentRowChanged(func(row int) {
		if row >= 0 && row < len(h.entries) && h.onSelect != nil {
			h.onSelect(h.entries[row])
		}
	})

	empty := qt.NewQLabel3("Sent requests will be listed here.")
	setName(empty.QObject, "EmptyState")
	empty.SetAlignment(qt.AlignCenter)

	h.stack.AddWidget(empty.QWidget)
	h.stack.AddWidget(h.list.QWidget)
	h.stack.SetCurrentIndex(replayHistoryEmpty)

	h.root = wrapReplayPanel("Replay History", h.stack.QWidget)
	return h
}

// add records a send and shows it at the top of the list. The new entry is not
// auto-selected, so an in-progress edit isn't disturbed; the response pane is
// already showing this send's result.
func (h *replayHistory) add(e replay.Entry) {
	h.entries = append([]replay.Entry{e}, h.entries...)
	h.list.InsertItem2(0, historySummary(e))
	h.stack.SetCurrentIndex(replayHistoryList)
}
