package ui

import (
	"strings"
	"unicode/utf8"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/editassist"
)

// Typing assists for the body editor (JSON, XML and HTML): auto-closed
// brackets, quotes and tags, indentation on Enter, and a tag-name completion
// popup. The rules live in editassist; this file turns key presses into calls
// and applies the edits.

func (c *codeEditor) installAssist() {
	c.completer = qt.NewQCompleter4(c.edit.QObject)
	c.completions = qt.NewQStringListModel3(c.completer.QObject)
	c.completer.SetModel(c.completions.QAbstractItemModel)
	c.completer.SetWidget(c.edit.QWidget)
	c.completer.SetCompletionMode(qt.QCompleter__PopupCompletion)
	c.completer.SetCaseSensitivity(qt.CaseInsensitive)
	c.completer.SetMaxVisibleItems(8)
	c.completer.Popup().SetFont(monospaceFont())
	c.completer.OnActivated(c.insertCompletion)
	c.edit.OnKeyPressEvent(c.keyPress)
}

// assistLang returns the language to assist with, if assists are on, the
// editor is editable and its language has assists.
func (c *codeEditor) assistLang() (editassist.Lang, bool) {
	if !c.assist || c.edit.IsReadOnly() {
		return 0, false
	}
	switch c.lang {
	case LangJSON:
		return editassist.JSON, true
	case LangXML:
		return editassist.XML, true
	case LangHTML:
		return editassist.HTML, true
	}
	return 0, false
}

func (c *codeEditor) keyPress(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
	key := qt.Key(e.Key())
	popup := c.completer.Popup()
	if popup.IsVisible() {
		switch key {
		case qt.Key_Return, qt.Key_Enter, qt.Key_Escape, qt.Key_Tab, qt.Key_Backtab:
			e.Ignore() // the completer picks or dismisses
			return
		}
	}
	lang, ok := c.assistLang()
	if !ok {
		super(e)
		return
	}
	if !c.assistKey(lang, key, e) {
		super(e)
	}
	// Keep the completions in step with typing; anything else closes them.
	if key == qt.Key_Backspace || e.Text() != "" && e.Modifiers()&qt.ControlModifier == 0 {
		c.updateCompletions(lang)
	} else {
		popup.Hide()
	}
}

// assistKey applies an assist for the key, reporting whether it did.
func (c *codeEditor) assistKey(lang editassist.Lang, key qt.Key, e *qt.QKeyEvent) bool {
	if e.Modifiers()&^(qt.ShiftModifier|qt.KeypadModifier) != 0 {
		return false
	}
	cur := c.edit.TextCursor()
	if cur.HasSelection() {
		return false
	}
	text := c.text()
	if len(text) > maxCheckSize {
		return false
	}
	pos := byteOffset(text, cur.Position())
	var edit editassist.Edit
	ok := false
	switch key {
	case qt.Key_Return, qt.Key_Enter:
		edit, ok = editassist.Newline(lang, text, pos)
	case qt.Key_Backspace:
		edit, ok = editassist.Backspace(lang, text, pos)
	default:
		if t := e.Text(); len(t) == 1 {
			edit, ok = editassist.Type(lang, text, pos, t)
		}
	}
	if ok {
		c.applyEdit(text, edit)
	}
	return ok
}

// applyEdit makes an editassist edit to text (the editor's current text) as
// one undo step.
func (c *codeEditor) applyEdit(text string, e editassist.Edit) {
	start := utf16Count(text[:e.Start])
	cur := c.edit.TextCursor()
	cur.BeginEditBlock()
	cur.SetPosition(start)
	cur.SetPosition2(start+utf16Count(text[e.Start:e.End]), qt.QTextCursor__KeepAnchor)
	cur.InsertText(e.Text)
	cur.SetPosition(start + utf16Count(e.Text[:e.Cursor-e.Start]))
	cur.EndEditBlock()
	c.edit.SetTextCursor(cur)
}

// updateCompletions shows tag names matching the start tag being typed, or
// hides the popup when there is none.
func (c *codeEditor) updateCompletions(lang editassist.Lang) {
	popup := c.completer.Popup()
	text := c.text()
	pos := byteOffset(text, c.edit.TextCursor().Position())
	prefix, ok := "", false
	if lang != editassist.JSON && len(text) <= maxCheckSize {
		prefix, ok = editassist.TagPrefix(text, pos)
	}
	if !ok || prefix == "" {
		popup.Hide()
		return
	}
	// Leave out the half-typed name itself.
	others := text[:pos-len(prefix)] + text[pos:]
	c.completions.SetStringList(editassist.TagNames(lang, others))
	c.completer.SetCompletionPrefix(prefix)
	n := c.completer.CompletionCount()
	if n == 0 || n == 1 && strings.EqualFold(c.completer.CurrentCompletion(), prefix) {
		popup.Hide()
		return
	}
	popup.SetCurrentIndex(c.completer.CompletionModel().Index(0, 0, qt.NewQModelIndex()))
	rect := c.edit.CursorRect2()
	rect.SetWidth(popup.SizeHintForColumn(0) + popup.VerticalScrollBar().SizeHint().Width() + 16)
	c.completer.CompleteWithRect(rect)
}

// insertCompletion replaces the half-typed tag name with the chosen one.
func (c *codeEditor) insertCompletion(name string) {
	text := c.text()
	cur := c.edit.TextCursor()
	prefix, ok := editassist.TagPrefix(text, byteOffset(text, cur.Position()))
	if !ok {
		return
	}
	cur.SetPosition2(cur.Position()-utf16Count(prefix), qt.QTextCursor__KeepAnchor)
	cur.InsertText(name)
	c.edit.SetTextCursor(cur)
}

// byteOffset converts an editor position (UTF-16 code units) to a byte
// offset in text.
func byteOffset(text string, units int) int {
	off := 0
	for off < len(text) && units > 0 {
		r, n := utf8.DecodeRuneInString(text[off:])
		off += n
		units -= utf16Len(r)
	}
	return off
}
