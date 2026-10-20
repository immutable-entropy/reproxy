package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/intercept"
)

// messageView shows one HTTP message as structured, editable parts: the
// start line, a key/value editor for the headers, and a body codeEditor
// (with its language selector, validation and assists). A Pretty/Raw toggle (in the always-visible toolbar) swaps in the
// exact raw text.
//
// Pretty shows the body as it reads: chunked framing and gzip, deflate,
// brotli or zstd coding undone, JSON and XML laid out (see
// intercept.EditableView). What was done appears beside the body's title, and
// Raw always has the message as it stood on the wire.
//
// When editable (a held message), the start line and header values can be
// changed and headers added or removed. serialize() turns the parts back into
// the text form intercept.ApplyEdit expects, so the wire-reconstruction logic
// (Content-Length, CRLF, binary bodies) stays in the tested intercept package.
type messageView struct {
	root  *qt.QWidget        // toolbar (always visible) above the stack
	stack *qt.QStackedWidget // 0: structured, 1: raw
	pages *qt.QComboBox

	split      *qt.QSplitter   // start line and headers above, body below
	headerArea *qt.QScrollArea // the start line and headers

	startLine *qt.QLineEdit
	headerBox *qt.QVBoxLayout
	addHeader *qt.QPushButton
	rows      []*headerRow

	body    *codeEditor
	rawView *qt.QPlainTextEdit

	editable bool

	// onFinding, when set by the view's owner (the Traffic detail pane or a
	// Replay window), puts Create Finding in the right-click menu of every
	// part of the message. The view says which part the text came from and,
	// for a header, which header; the owner knows which request and which
	// half of it this view is showing, and turns the two into a finding.
	onFinding func(value string, part messagePart, detail string)
}

// messagePart is which part of a message a right-click happened in. The
// owner maps it to a findings location, since only it knows whether this
// view holds the request or the response.
type messagePart int

const (
	partHeader messagePart = iota
	partBody
)

type headerRow struct {
	w      *qt.QWidget
	key    *qt.QLineEdit
	value  *qt.QLineEdit
	remove *iconButton
}

func (r *headerRow) widget() *qt.QWidget { return r.w }

// newMessageView builds a message view. With showToolbar it carries its own
// Pretty/Raw toolbar (the Replay tool, where the two views sit side by side);
// without it the caller drives the Pretty/Raw mode through showRaw (the
// intercept pane, which puts one shared selector in the tab row).
func newMessageView(showToolbar bool) *messageView {
	m := &messageView{root: qt.NewQWidget2(), stack: qt.NewQStackedWidget2()}

	rootLayout := qt.NewQVBoxLayout(m.root)
	rootLayout.SetContentsMargins(0, 0, 0, 0)
	rootLayout.SetSpacing(0)
	// The toolbar, when present, stays visible in both Pretty and Raw so the
	// toggle never disappears.
	if showToolbar {
		rootLayout.AddWidget(m.buildToolbar())
	}
	rootLayout.AddWidget(m.stack.QWidget)

	// Page 0: structured.
	structured := qt.NewQWidget2()
	outer := qt.NewQVBoxLayout(structured)
	outer.SetContentsMargins(0, 0, 0, 0)
	outer.SetSpacing(0)

	// The start line and headers scroll together above the body.
	top := qt.NewQWidget2()
	topLayout := qt.NewQVBoxLayout(top)
	topLayout.SetContentsMargins(rowPadX, 8, rowPadX, 8)
	topLayout.SetSpacing(6)

	m.startLine = qt.NewQLineEdit2()
	setName(m.startLine.QObject, "StartLine")
	m.startLine.SetFont(monospaceFont())
	installFieldMenu(m.startLine, func(value string) { m.raise(value, partHeader, "start line") })
	topLayout.AddWidget(m.startLine.QWidget)

	headersHost := qt.NewQWidget2()
	m.headerBox = qt.NewQVBoxLayout(headersHost)
	m.headerBox.SetContentsMargins(0, 0, 0, 0)
	m.headerBox.SetSpacing(3)
	topLayout.AddWidget(headersHost)

	m.addHeader = qt.NewQPushButton3("+ Add header")
	m.addHeader.SetAutoDefault(false)
	setName(m.addHeader.QObject, "LinkButton")
	m.addHeader.OnClicked(func() {
		r := m.addRow("", "")
		r.key.SetFocus()
	})
	addRow := qt.NewQHBoxLayout2()
	addRow.AddWidget(m.addHeader.QWidget)
	addRow.AddStretch()
	topLayout.AddLayout(addRow.QLayout)

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "HeaderScroll")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(top)
	scroll.SetMaximumHeight(260)
	scroll.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)

	m.body = newCodeEditor(editorOptions{
		title:            "Body",
		languageSelector: true,
		validate:         true,
		assist:           true,
	})
	m.body.onFinding = func(value string, _ int) { m.raise(value, partBody, "") }

	split := qt.NewQSplitter3(qt.Vertical)
	split.SetChildrenCollapsible(false)
	split.SetHandleWidth(1)
	split.AddWidget(scroll.QWidget)
	m.split, m.headerArea = split, scroll
	split.AddWidget(m.body.root)
	split.SetStretchFactor(0, 0)
	split.SetStretchFactor(1, 1)
	outer.AddWidget(split.QWidget)

	// Page 1: raw.
	m.rawView = qt.NewQPlainTextEdit2()
	m.rawView.SetReadOnly(true)
	m.rawView.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	m.rawView.SetFont(monospaceFont())

	// In Raw the whole message is one editor, so which part a selection
	// belongs to is decided by where it sits relative to the blank line.
	installEditorMenu(m.rawView, func(value string, pos int) {
		m.raise(value, m.rawPart(pos), "")
	})

	m.stack.AddWidget(structured)
	m.stack.AddWidget(m.rawView.QWidget)
	return m
}

// splitEvenly gives the headers and the body half the height each, instead
// of keeping the headers compact, and lets the headers grow to fill theirs.
func (m *messageView) splitEvenly() {
	m.headerArea.SetMaximumHeight(qwidgetMaxSize)
	m.split.SetStretchFactor(0, 1)
	m.split.SetStretchFactor(1, 1)
	m.split.SetSizes([]int{1000, 1000})
}

// splitHeaderMajority gives the start line and headers about 60% of the
// height by default (the intercept pane), dropping the compact cap so they
// can fill it. The user can still drag the splitter to shrink them.
func (m *messageView) splitHeaderMajority() {
	m.headerArea.SetMaximumHeight(qwidgetMaxSize)
	m.split.SetStretchFactor(0, 3)
	m.split.SetStretchFactor(1, 2)
	m.split.SetSizes([]int{600, 400})
}

// qwidgetMaxSize is Qt's QWIDGETSIZE_MAX: no maximum.
const qwidgetMaxSize = (1 << 24) - 1

func (m *messageView) buildToolbar() *qt.QWidget {
	bar := qt.NewQWidget2()
	setName(bar.QObject, "EditorToolbar")
	bar.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(bar)
	h.SetContentsMargins(rowPadX, 5, 10, 5)
	h.SetSpacing(8)

	m.pages = qt.NewQComboBox2()
	m.pages.AddItems([]string{"Pretty", "Raw"})
	m.pages.OnCurrentIndexChanged(func(i int) { m.stack.SetCurrentIndex(i) })
	h.AddWidget(m.pages.QWidget)
	h.AddStretch()
	return bar
}

// setMessage populates the view from a raw message. editable enables editing
// the start line and headers and adding/removing them.
func (m *messageView) setMessage(raw []byte, editable bool) {
	m.editable = editable
	view, note := intercept.EditableView(raw)
	head, body, _ := strings.Cut(view, "\n\n")
	lines := strings.Split(head, "\n")

	m.startLine.SetText(lines[0])
	m.startLine.SetReadOnly(!editable)

	m.clearRows()
	contentType := ""
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			m.addRow(l, "")
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if strings.EqualFold(k, "Content-Type") {
			contentType = v
		}
		m.addRow(k, v)
	}
	m.addHeader.SetVisible(editable)

	// Validation and assists follow editability: a received body is what it is.
	m.body.setReadOnly(!editable)
	m.body.setTitle(bodyTitle(note))
	m.body.load(body, contentType)

	// Raw is the message as it stood on the wire, whatever Pretty had to undo
	// to make it readable.
	m.rawView.SetPlainText(intercept.EditableText(raw))
	if m.pages != nil {
		m.pages.SetCurrentIndex(0)
	}
	m.stack.SetCurrentIndex(0)
}

// raise passes a right-click's text up to the view's owner, which turns it
// into a finding. It does nothing where no owner offered it.
func (m *messageView) raise(value string, part messagePart, detail string) {
	if m.onFinding != nil {
		m.onFinding(value, part, detail)
	}
}

// rawPart says whether a position in the raw view is in the headers or in
// the body: the body starts after the blank line that ends the headers.
func (m *messageView) rawPart(pos int) messagePart {
	text := m.rawView.ToPlainText()
	i := strings.Index(text, "\n\n")
	if i < 0 || pos <= i {
		return partHeader
	}
	return partBody
}

// showRaw switches between the structured (Pretty) and raw views. It keeps an
// own toolbar, if any, in step, so an external selector and the toolbar never
// disagree.
func (m *messageView) showRaw(raw bool) {
	idx := 0
	if raw {
		idx = 1
	}
	m.stack.SetCurrentIndex(idx)
	if m.pages != nil && m.pages.CurrentIndex() != idx {
		m.pages.SetCurrentIndex(idx)
	}
}

// rawShown reports whether the raw view is the one currently visible.
func (m *messageView) rawShown() bool { return m.stack.CurrentIndex() == 1 }

// setPlaceholder shows a one-line message instead of a parsed message, for
// states like "waiting for response".
func (m *messageView) setPlaceholder(text string) {
	m.startLine.SetText("")
	m.startLine.SetReadOnly(true)
	m.clearRows()
	m.addHeader.SetVisible(false)
	m.body.setReadOnly(true)
	m.body.setTitle(bodyTitle(""))
	m.body.setText(text)
	m.body.setLanguage(LangPlain)
	m.rawView.SetPlainText(text)
}

// bodyTitle labels the body editor, naming what was undone to make the body
// readable so that nothing about the view is a surprise: the user sees, for
// example, "Body — decoded from brotli · formatted as JSON".
func bodyTitle(note string) string {
	if note == "" {
		return "Body"
	}
	return "Body — " + note
}

// onHeaderChanged re-detects the body language when the Content-Type header is
// edited, so the body reformats to match (the editor keeps a language the user
// picked by hand).
func (m *messageView) onHeaderChanged(r *headerRow) {
	if strings.EqualFold(strings.TrimSpace(r.key.Text()), "Content-Type") {
		m.body.contentTypeChanged(r.value.Text())
	}
}

// serialize rebuilds the editable text form intercept.ApplyEdit consumes:
// start line, then one "Key: Value" per non-empty header, a blank line, and
// the body.
func (m *messageView) serialize() string {
	var b strings.Builder
	b.WriteString(m.startLine.Text())
	for _, r := range m.rows {
		key := strings.TrimSpace(r.key.Text())
		if key == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(r.value.Text()))
	}
	b.WriteString("\n\n")
	b.WriteString(m.body.text())
	return b.String()
}

// hasFraming reports whether the headers say how long the body is: a
// Content-Length or a Transfer-Encoding.
func (m *messageView) hasFraming() bool {
	for _, r := range m.rows {
		key := strings.TrimSpace(r.key.Text())
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") {
			return true
		}
	}
	return false
}

func (m *messageView) addRow(key, value string) *headerRow {
	r := &headerRow{w: qt.NewQWidget2()}
	h := qt.NewQHBoxLayout(r.w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(6)

	r.key = qt.NewQLineEdit3(key)
	setName(r.key.QObject, "HeaderKey")
	r.key.SetReadOnly(!m.editable)
	r.key.SetFixedWidth(220)
	r.value = qt.NewQLineEdit3(value)
	setName(r.value.QObject, "HeaderValue")
	r.value.SetReadOnly(!m.editable)
	// The same button every other row's remove is, rather than a literal
	// glyph: it paints the icon set's close mark and reddens on hover.
	r.remove = newIconButton(iconClose, "Remove this header", func() { m.removeRow(r) }).asDanger()
	r.remove.w.SetVisible(m.editable)

	// Editing Content-Type reformats the body to the matching language.
	r.key.OnTextChanged(func(string) { m.onHeaderChanged(r) })
	r.value.OnTextChanged(func(string) { m.onHeaderChanged(r) })

	// A finding raised from a header names that header, so the detail pane
	// can say "response header (Set-Cookie)" rather than just "header".
	header := func(value string) { m.raise(value, partHeader, strings.TrimSpace(r.key.Text())) }
	installFieldMenu(r.key, header)
	installFieldMenu(r.value, header)

	h.AddWidget(r.key.QWidget)
	h.AddWidget2(r.value.QWidget, 1)
	h.AddWidget(r.remove.w)

	m.headerBox.AddWidget(r.w)
	m.rows = append(m.rows, r)
	return r
}

func (m *messageView) removeRow(r *headerRow) {
	m.rows, _ = dropRow(m.rows, r)
}

// refreshTheme recolours the body's syntax highlighting after a theme change.
// The other parts (header fields, start line, raw view) are styled by the
// stylesheet, which is re-applied at the same time.
func (m *messageView) refreshTheme() {
	m.body.refreshTheme()
}

func (m *messageView) clearRows() {
	for len(m.rows) > 0 {
		m.removeRow(m.rows[0])
	}
}
