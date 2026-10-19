package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/capture"
)

// messagesView is the Messages tab shown for a WebSocket flow: the messages
// exchanged after the 101 upgrade, each with its direction, as a transcript
// above a reader for the selected message's payload.
//
// A message the browser sent reads "→"; one from the server "←". A message a
// plugin changed or dropped is marked, and its editor shows what arrived
// before the change.
type messagesView struct {
	root     *qt.QWidget
	list     *qt.QTreeWidget
	body     *codeEditor
	messages []capture.WSMessage
	shownID  uint64
}

const (
	msgColDir  = 0
	msgColType = 1
	msgColSize = 2
	msgColText = 3
)

func newMessagesView() *messagesView {
	v := &messagesView{root: qt.NewQWidget2()}
	outer := qt.NewQVBoxLayout(v.root)
	outer.SetContentsMargins(0, 0, 0, 0)
	outer.SetSpacing(0)

	split := qt.NewQSplitter2()
	split.SetOrientation(qt.Vertical)

	v.list = qt.NewQTreeWidget2()
	v.list.SetColumnCount(4)
	v.list.SetHeaderLabels([]string{"", "Type", "Size", "Message"})
	v.list.SetRootIsDecorated(false)
	v.list.SetUniformRowHeights(true)
	v.list.SetFrameShape(qt.QFrame__NoFrame)
	v.list.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	header := v.list.Header()
	header.SetStretchLastSection(true)
	header.ResizeSection(msgColDir, 28)
	header.SetSectionResizeMode2(msgColType, qt.QHeaderView__ResizeToContents)
	header.SetSectionResizeMode2(msgColSize, qt.QHeaderView__ResizeToContents)
	v.list.OnCurrentItemChanged(func(cur, _ *qt.QTreeWidgetItem) { v.showBody(cur) })

	v.body = newCodeEditor(editorOptions{title: "Message"})
	v.body.setReadOnly(true)

	split.AddWidget(v.list.QWidget)
	split.AddWidget(v.body.root)
	split.SetStretchFactor(0, 2)
	split.SetStretchFactor(1, 1)
	outer.AddWidget(split.QWidget)
	return v
}

// setFlow shows a flow's messages, keeping the selection when it is the same
// flow gaining messages.
func (v *messagesView) setFlow(f capture.Flow) {
	sameFlow := f.ID == v.shownID
	prev := v.list.CurrentItem()
	prevRow := -1
	if sameFlow && prev != nil {
		prevRow = v.list.IndexOfTopLevelItem(prev)
	}
	v.shownID = f.ID
	v.messages = f.Messages

	v.list.Clear()
	for _, m := range f.Messages {
		item := qt.NewQTreeWidgetItem()
		v.list.AddTopLevelItem(item)
		item.SetText(msgColDir, directionArrow(m.Dir))
		item.SetToolTip(msgColDir, m.Dir.String())
		item.SetText(msgColType, m.Type.String())
		item.SetText(msgColSize, capture.FormatBytes(int64(len(m.Data))))
		item.SetText(msgColText, messagePreview(m))
	}
	switch {
	case prevRow >= 0 && prevRow < v.list.TopLevelItemCount():
		v.list.SetCurrentItem(v.list.TopLevelItem(prevRow))
	case !sameFlow && v.list.TopLevelItemCount() > 0:
		v.list.SetCurrentItem(v.list.TopLevelItem(0))
	}
}

func (v *messagesView) showBody(item *qt.QTreeWidgetItem) {
	if item == nil {
		v.body.setText("")
		return
	}
	i := v.list.IndexOfTopLevelItem(item)
	if i < 0 || i >= len(v.messages) {
		return
	}
	m := v.messages[i]
	var text string
	switch {
	case m.Type == capture.WSBinary:
		text = fmt.Sprintf("‹%s of binary data›", capture.FormatBytes(int64(len(m.Data))))
	default:
		text = string(m.Data)
	}
	if m.Edited {
		text = "‹edited by a plugin — what arrived:›\n" + string(m.Original) + "\n\n‹forwarded:›\n" + text
	} else if m.Dropped {
		text = "‹dropped by a plugin — not forwarded. What arrived:›\n" + string(m.Original)
	}
	if m.Truncated {
		text += "\n\n‹truncated at the recording limit›"
	}
	v.body.setText(text)
}

func directionArrow(d capture.WSDirection) string {
	if d == capture.Received {
		return "←"
	}
	return "→"
}

func messagePreview(m capture.WSMessage) string {
	var s string
	if m.Type == capture.WSBinary {
		s = fmt.Sprintf("binary, %s", capture.FormatBytes(int64(len(m.Data))))
	} else {
		s = oneLine(string(m.Data), 200)
	}
	switch {
	case m.Dropped:
		s = "[dropped] " + s
	case m.Edited:
		s = "[edited] " + s
	}
	return s
}

// oneLine collapses newlines and clips to n runes for a preview.
func oneLine(s string, n int) string {
	out := make([]rune, 0, n)
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= n {
			out = append(out, '…')
			break
		}
	}
	return string(out)
}
