package ui

import (
	"fmt"
	"unicode"
	"unicode/utf16"

	qt "github.com/mappu/miqt/qt6"
)

// messageSearch finds text in a request and its response: a magnifying
// glass that, clicked or on Ctrl+F, gives way to a search field in the same
// place. Every occurrence of what is typed is highlighted as it is typed, in
// the start line, the headers and the body of both messages, in whichever
// view (Pretty, Raw or HTML) each is showing. Matching ignores case.
//
// The Traffic viewer and every Replay tab carry one each. Highlights follow
// the messages: a new request selected, a response arriving or a header
// edited is searched again (see messageView.searchChanged).
type messageSearch struct {
	root   *qt.QWidget
	button *iconButton
	field  *qt.QLineEdit
	count  *qt.QLabel
	close  *iconButton

	views []*messageView
	// shown is whether the field is out (and the messages highlighted).
	shown bool
	// again coalesces the re-searches the messages ask for: showing a new
	// message changes several parts of it, which is one search, not several.
	again *qt.QTimer
}

// maxMatches caps the highlights in one document, so a short term in a
// large body doesn't stall the UI thread.
const maxMatches = 5000

func newMessageSearch() *messageSearch {
	s := &messageSearch{root: qt.NewQWidget2()}
	h := qt.NewQHBoxLayout(s.root)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(6)

	s.button = newIconButton(iconSearch, "Search the request and response (Ctrl+F)", s.open)
	s.button.w.SetAccessibleName("Search")

	s.field = qt.NewQLineEdit2()
	s.field.SetPlaceholderText("Search request and response")
	s.field.SetClearButtonEnabled(true)
	s.field.SetFixedWidth(220)
	s.field.OnTextChanged(func(string) { s.refresh() })
	s.field.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if e.Key() == int(qt.Key_Escape) {
			s.hide()
			return
		}
		super(e)
	})

	s.count = qt.NewQLabel2()
	setName(s.count.QObject, "EmptyState") // the muted hint colour
	s.close = newIconButton(iconClose, "Close the search (Esc)", s.hide)

	s.again = qt.NewQTimer2(s.root.QObject)
	s.again.SetSingleShot(true)
	s.again.OnTimeout(s.refresh)

	h.AddWidget(s.button.w)
	h.AddWidget(s.field.QWidget)
	h.AddWidget(s.count.QWidget)
	h.AddWidget(s.close.w)
	s.showField(false)
	return s
}

// watch sets the messages searched, which are built after the bar.
func (s *messageSearch) watch(views ...*messageView) {
	s.views = views
	for _, v := range views {
		v.searchChanged = func() { s.again.Start(0) }
	}
}

// open shows the field in place of the button and puts the cursor in it,
// with what was searched for last selected, so typing starts a new search.
func (s *messageSearch) open() {
	s.showField(true)
	s.field.SetFocus()
	s.field.SelectAll()
	s.refresh()
}

// hide puts the button back and takes the highlights away. The term is
// kept for the next open.
func (s *messageSearch) hide() {
	s.again.Stop()
	for _, v := range s.views {
		v.search("")
	}
	s.showField(false)
}

func (s *messageSearch) showField(on bool) {
	s.shown = on
	s.button.w.SetVisible(!on)
	s.field.SetVisible(on)
	s.count.SetVisible(on)
	s.close.w.SetVisible(on)
}

// refresh searches the messages for the field's text and says how many
// matches there are.
func (s *messageSearch) refresh() {
	if !s.shown {
		return // closed: nothing is highlighted
	}
	term := s.field.Text()
	n := 0
	for _, v := range s.views {
		n += v.search(term)
	}
	switch {
	case term == "":
		s.count.SetText("")
	case n == 0:
		s.count.SetText("No matches")
	case n == 1:
		s.count.SetText("1 match")
	default:
		s.count.SetText(fmt.Sprintf("%d matches", n))
	}
}

// matchFormat is how a match is highlighted in the theme's own colours.
func matchFormat() *qt.QTextCharFormat {
	f := qt.NewQTextCharFormat()
	f.SetBackground(qt.NewQBrush3(color(colorSearchMatch)))
	return f
}

// pageMatchFormat is the highlight in the HTML view, which is drawn on
// white whatever the theme.
func pageMatchFormat() *qt.QTextCharFormat {
	f := qt.NewQTextCharFormat()
	f.SetBackground(qt.NewQBrush3(qt.NewQColor6("#ffe066")))
	f.SetForeground(qt.NewQBrush3(qt.NewQColor6("#000000")))
	return f
}

// findAll highlights every occurrence of term in doc, ignoring case.
func findAll(doc *qt.QTextDocument, term string, f *qt.QTextCharFormat) []qt.QTextEdit__ExtraSelection {
	if term == "" {
		return nil
	}
	var out []qt.QTextEdit__ExtraSelection
	for cur := doc.Find4(term, 0); !cur.IsNull() && len(out) < maxMatches; cur = doc.Find4(term, cur.SelectionEnd()) {
		sel := qt.NewQTextEdit__ExtraSelection2()
		sel.SetFormat(*f)
		sel.SetCursor(*cur)
		out = append(out, *sel)
	}
	return out
}

// markField highlights every occurrence of term in a one-line field, or
// clears its highlights when term is empty, and says how many there are.
//
// A QLineEdit has no extra selections, so the highlights go in as an input
// method's text formats: the same mechanism that underlines text still
// being composed, sent with nothing to compose. A read-only field ignores
// input method events, so it is made writable for the moment it takes them.
func markField(e *qt.QLineEdit, term string) int {
	ranges := foldMatches(e.Text(), term)
	f := matchFormat()
	at := e.CursorPosition() // the formats' positions are relative to it
	attrs := make([]qt.QInputMethodEvent__Attribute, 0, len(ranges))
	for _, r := range ranges {
		attrs = append(attrs, *qt.NewQInputMethodEvent__Attribute(
			qt.QInputMethodEvent__TextFormat, r[0]-at, r[1], *f.ToQVariant()))
	}
	readOnly := e.IsReadOnly()
	if readOnly {
		e.SetReadOnly(false)
	}
	qt.QCoreApplication_SendEvent(e.QObject, qt.NewQInputMethodEvent2("", attrs).QEvent)
	if readOnly {
		e.SetReadOnly(true)
	}
	return len(ranges)
}

// foldMatches finds term in text ignoring case, as Qt's document search
// does, and gives each match as a start and length in UTF-16 code units,
// which is how Qt counts positions.
func foldMatches(text, term string) [][2]int {
	if term == "" {
		return nil
	}
	hay, needle := foldRunes(text), foldRunes(term)
	// offsets[i] is where rune i starts in UTF-16, counted on the text as it
	// stands rather than folded.
	offsets := make([]int, len(hay)+1)
	for i, r := range []rune(text) {
		offsets[i+1] = offsets[i] + utf16.RuneLen(r)
	}
	var out [][2]int
	for i := 0; i+len(needle) <= len(hay) && len(out) < maxMatches; {
		if runesAt(hay, needle, i) {
			out = append(out, [2]int{offsets[i], offsets[i+len(needle)] - offsets[i]})
			i += len(needle)
			continue
		}
		i++
	}
	return out
}

func foldRunes(s string) []rune {
	rs := []rune(s)
	for i, r := range rs {
		rs[i] = unicode.ToLower(r)
	}
	return rs
}

func runesAt(hay, needle []rune, i int) bool {
	for j, r := range needle {
		if hay[i+j] != r {
			return false
		}
	}
	return true
}
