package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/texttools"
)

// installEditorMenu gives a text editor a context menu with the standard
// actions (cut/copy/paste/select all), a Tools submenu of text
// transformations (see internal/texttools) and, where the editor's owner
// offers it, Create Finding.
//
// A tool works on the selection, or on the word under the cursor when there
// is none. In an editable editor it replaces the text in place; in a
// read-only one (a raw view, or a message that isn't held) it copies the
// result to the clipboard instead.
//
// onFinding is called with the same text and the position it was taken from,
// so the owner can say which part of which message the finding came from.
// It is consulted when the menu is built, not when the editor is created, so
// an owner can wire it up later; nil, or a nil result, leaves the item out.
func installEditorMenu(edit *qt.QPlainTextEdit, onFinding func(value string, pos int)) {
	edit.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), e *qt.QContextMenuEvent) {
		menu := edit.CreateStandardContextMenuWithPosition(e.Pos())

		// Target: the selection, else the word under the click point.
		target := edit.TextCursor()
		if !target.HasSelection() {
			target = edit.CursorForPosition(e.Pos())
			target.Select(qt.QTextCursor__WordUnderCursor)
		}
		input := normalizeSelection(target.SelectedText())
		globalPos := e.GlobalPos()

		if onFinding != nil {
			menu.AddSeparator()
			act := menu.AddActionWithText(findingMenuItem)
			act.SetEnabled(input != "")
			at := target.SelectionStart()
			act.OnTriggered(func() { onFinding(input, at) })
		}

		menu.AddSeparator()
		tools := menu.AddMenuWithTitle("Tools")
		for _, tool := range texttools.Tools() {
			tool := tool
			act := tools.AddActionWithText(tool.Name)
			act.SetEnabled(input != "")
			act.OnTriggered(func() { applyTool(edit, target, globalPos, tool, input) })
		}

		menu.ExecWithPos(globalPos)
		menu.DeleteLater()
	})
}

// findingMenuItem is the wording used everywhere a finding can be raised, so
// the action reads the same in a body, a raw view and a header field.
const findingMenuItem = "Create Finding…"

// installFieldMenu gives a one-line field (a start line, a header's name or
// its value) the standard context menu plus Create Finding, so a credential
// in a header can be recorded the same way as one in a body.
//
// onFinding is called with the selected text, or the whole field when
// nothing is selected: a header value is usually the thing itself, so
// selecting it first would be busywork.
func installFieldMenu(edit *qt.QLineEdit, onFinding func(value string)) {
	edit.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), e *qt.QContextMenuEvent) {
		menu := edit.CreateStandardContextMenu()
		if onFinding != nil {
			value := edit.SelectedText()
			if value == "" {
				value = edit.Text()
			}
			value = normalizeSelection(value)
			menu.AddSeparator()
			act := menu.AddActionWithText(findingMenuItem)
			act.SetEnabled(strings.TrimSpace(value) != "")
			act.OnTriggered(func() { onFinding(value) })
		}
		menu.ExecWithPos(e.GlobalPos())
		menu.DeleteLater()
	})
}

func applyTool(edit *qt.QPlainTextEdit, cur *qt.QTextCursor, at *qt.QPoint, tool texttools.Tool, input string) {
	out, err := tool.Apply(input)
	if err != nil {
		qt.QMessageBox_Warning(edit.QWidget, tool.Name, tool.Name+" failed: "+err.Error())
		return
	}
	if edit.IsReadOnly() {
		qt.QGuiApplication_Clipboard().SetText(out)
		qt.QToolTip_ShowText2(at, tool.Name+" → copied to clipboard", edit.QWidget)
		return
	}
	cur.InsertText(out)
	edit.SetTextCursor(cur)
}

// normalizeSelection converts the Unicode line/paragraph separators Qt uses
// in QTextCursor.SelectedText back into ordinary newlines.
func normalizeSelection(s string) string {
	s = strings.ReplaceAll(s, " ", "\n") // paragraph separator
	s = strings.ReplaceAll(s, " ", "\n") // line separator
	return s
}
