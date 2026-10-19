package ui

import qt "github.com/mappu/miqt/qt6"

// messagePair is a request and its response, shown side by side. Both the
// Traffic detail pane and a Replay view are built around one, and both need
// the same three things of it — the Edit menu has to find every editor in
// either message, and a theme change has to reach them all.
//
// Embedding it keeps that in one place: whoever holds the pair gets the
// routing for free, and a fourth editor added to messageView is picked up
// by every holder at once rather than by whichever ones were remembered.
type messagePair struct {
	request  *messageView
	response *messageView
}

// each ranges over the two messages, request first.
func (p messagePair) each(fn func(*messageView)) {
	for _, mv := range []*messageView{p.request, p.response} {
		if mv != nil {
			fn(mv)
		}
	}
}

// refreshTheme recolours both messages' editors after a theme change.
func (p messagePair) refreshTheme() {
	p.each((*messageView).refreshTheme)
}

// plainEditors returns both messages' body and raw text editors, for
// Edit-menu routing.
func (p messagePair) plainEditors() []*qt.QPlainTextEdit {
	var out []*qt.QPlainTextEdit
	p.each(func(mv *messageView) {
		out = append(out, mv.body.edit, mv.rawView)
	})
	return out
}

// lineEditors returns both messages' start-line and header fields, for
// Edit-menu routing.
func (p messagePair) lineEditors() []*qt.QLineEdit {
	var out []*qt.QLineEdit
	p.each(func(mv *messageView) {
		out = append(out, mv.startLine)
		for _, r := range mv.rows {
			out = append(out, r.key, r.value)
		}
	})
	return out
}
