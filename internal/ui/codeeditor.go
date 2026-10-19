package ui

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/syntaxcheck"
)

// codeEditor is the app's text editor component, for use anywhere text is
// shown or edited (message bodies today). It is monospaced, with a
// line-number gutter, current-line highlight, syntax highlighting, and a
// right-click menu with the standard actions plus the Tools transforms.
//
// Options (see editorOptions) add the rest:
//   - a header row with a title and a language selector; the language is
//     detected from a Content-Type until the user picks one by hand;
//   - validation: the text is checked against the language as the user types,
//     problems are underlined, their line numbers turn red, hovering one shows
//     its message, and a bar under the editor summarises them;
//   - typing assists for JSON, XML and HTML (see editorassist.go).
//
// Validation and assists only apply while the editor is editable.
type codeEditor struct {
	root   *qt.QWidget
	edit   *qt.QPlainTextEdit
	gutter *qt.QWidget
	hl     *highlighter
	lang   Language

	header    *qt.QWidget
	headerRow *qt.QHBoxLayout
	// headerExtras is set once an owner has put a button of its own in the
	// header row, so the row stays even if nothing else is in it.
	headerExtras bool
	title        *qt.QLabel
	langSel      *qt.QComboBox
	// langManual is set once the user picks a language by hand, so that
	// Content-Type changes stop overriding their choice. settingLang guards
	// the selector's signal while it is set in code.
	langManual  bool
	settingLang bool

	validate   bool
	checkTimer *qt.QTimer
	issueBar   *qt.QLabel
	issues     []placedIssue
	issueLines map[int]bool // 0-based block numbers with a problem
	nextIssue  int          // which problem the bar's link jumps to next

	assist      bool
	completer   *qt.QCompleter // tag-name completions (see editorassist.go)
	completions *qt.QStringListModel

	// onFinding, when set by the editor's owner, puts Create Finding in the
	// right-click menu. It is called with the selected text (or the word
	// under the cursor) and where in the editor it starts.
	onFinding func(value string, pos int)
}

// editorOptions configures a codeEditor. The zero value is a bare editor:
// no header, no validation, no assists.
type editorOptions struct {
	// title labels the header row, e.g. "Body".
	title string
	// languageSelector shows a language picker in the header row.
	languageSelector bool
	// validate checks the text against its language while editable.
	validate bool
	// assist enables typing assists and completions while editable.
	assist bool
	// wrap wraps long lines instead of scrolling sideways. Code reads
	// better unwrapped, which is the default; prose (a finding's notes)
	// reads better wrapped.
	wrap bool
}

// addHeaderWidget puts a widget at the right of the header row, before the
// language selector: an action that belongs to this text rather than to the
// view around it (the eye that reveals a hidden finding, say).
func (c *codeEditor) addHeaderWidget(w *qt.QWidget) {
	c.headerRow.AddWidget(w)
	c.headerExtras = true
	c.updateHeaderVisibility()
}

// placedIssue is a syntax problem with its range in editor positions (UTF-16
// code units, which is what QTextCursor counts).
type placedIssue struct {
	syntaxcheck.Issue
	start, end int
}

// checkDelay is how long typing must pause before the body is re-checked.
const checkDelay = 250

// maxCheckSize skips validation of very large bodies, which would make typing
// sluggish.
const maxCheckSize = 4 << 20

func newCodeEditor(opts editorOptions) *codeEditor {
	c := &codeEditor{
		root:     qt.NewQWidget2(),
		edit:     qt.NewQPlainTextEdit2(), // direct construction: gutter geometry needs it
		gutter:   qt.NewQWidget2(),
		validate: opts.validate,
		assist:   opts.assist,
	}
	setName(c.edit.QObject, "CodeBody")
	c.edit.SetFont(monospaceFont())
	if opts.wrap {
		c.edit.SetLineWrapMode(qt.QPlainTextEdit__WidgetWidth)
	} else {
		c.edit.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	}
	c.edit.SetTabStopDistance(4 * charWidth(c.edit))
	c.hl = newHighlighter(c.edit.Document())

	setName(c.gutter.QObject, "Gutter")
	c.gutter.OnPaintEvent(func(_ func(*qt.QPaintEvent), e *qt.QPaintEvent) { c.paintGutter() })

	// Keep the gutter in sync with scrolling, edits and the cursor.
	c.edit.OnBlockCountChanged(func(int) { c.updateGutterWidth() })
	c.edit.OnUpdateRequest(func(*qt.QRect, int) { c.gutter.Update() })
	c.edit.OnCursorPositionChanged(func() { c.highlightCurrentLine() })
	c.updateGutterWidth()
	c.highlightCurrentLine()

	c.checkTimer = qt.NewQTimer2(c.edit.QObject)
	c.checkTimer.SetSingleShot(true)
	c.checkTimer.OnTimeout(c.runCheck)
	c.edit.OnTextChanged(func() {
		if c.validate && !c.edit.IsReadOnly() {
			c.checkTimer.Start(checkDelay)
		}
	})
	c.edit.OnViewportEvent(func(super func(*qt.QEvent) bool, e *qt.QEvent) bool {
		if e.Type() == qt.QEvent__ToolTip {
			c.showIssueTip(qt.UnsafeNewQHelpEvent(e.UnsafePointer()))
			return true
		}
		return super(e)
	})

	c.installAssist()
	// Create Finding is offered only where the owner wires it up: a message
	// body does, the Notes editor in the Create Finding window doesn't.
	installEditorMenu(c.edit, func(value string, pos int) {
		if c.onFinding != nil {
			c.onFinding(value, pos)
		}
	})

	c.issueBar = qt.NewQLabel2()
	setName(c.issueBar.QObject, "IssueBar")
	c.issueBar.SetTextFormat(qt.RichText)
	c.issueBar.OnLinkActivated(func(string) { c.jumpToNextIssue() })
	c.issueBar.Hide()

	editRow := qt.NewQHBoxLayout2()
	editRow.SetContentsMargins(0, 0, 0, 0)
	editRow.SetSpacing(0)
	editRow.AddWidget(c.gutter)
	editRow.AddWidget(c.edit.QWidget)
	layout := qt.NewQVBoxLayout(c.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.SetSpacing(0)
	layout.AddWidget(c.buildHeader(opts))
	layout.AddLayout(editRow.QLayout)
	layout.AddWidget(c.issueBar.QWidget)
	return c
}

func charWidth(e *qt.QPlainTextEdit) float64 {
	return float64(qt.NewQFontMetrics(e.Font()).HorizontalAdvance("9"))
}

// buildHeader makes the header row: the title on the left, the language
// selector on the right. It is hidden when it would be empty.
func (c *codeEditor) buildHeader(opts editorOptions) *qt.QWidget {
	c.header = qt.NewQWidget2()
	setName(c.header.QObject, "EditorHeader")
	c.header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(c.header)
	h.SetContentsMargins(rowPadX, 3, 8, 3)
	h.SetSpacing(8)
	c.headerRow = h

	c.title = qt.NewQLabel3(opts.title)
	setName(c.title.QObject, "EditorTitle")
	h.AddWidget(c.title.QWidget)
	h.AddStretch()

	c.langSel = qt.NewQComboBox2()
	setName(c.langSel.QObject, "LanguageSelect")
	c.langSel.SetToolTip("Language used to highlight, check and assist with this text")
	for _, l := range allLanguages {
		c.langSel.AddItem(string(l))
	}
	c.langSel.OnCurrentIndexChanged(func(i int) {
		if c.settingLang || i < 0 || i >= len(allLanguages) {
			return
		}
		c.langManual = true // the user chose this language explicitly
		c.applyLanguage(allLanguages[i])
	})
	h.AddWidget(c.langSel.QWidget)

	c.title.SetVisible(opts.title != "")
	c.langSel.SetVisible(opts.languageSelector)
	c.updateHeaderVisibility()
	return c.header
}

// setTitle changes the header title; an empty title hides it.
func (c *codeEditor) setTitle(title string) {
	c.title.SetText(title)
	c.title.SetVisible(title != "")
	c.updateHeaderVisibility()
}

// updateHeaderVisibility hides the header row when it has nothing in it.
// (isVisibleTo, not isVisible: the editor may not be on screen yet.)
func (c *codeEditor) updateHeaderVisibility() {
	c.header.SetVisible(c.headerExtras || c.title.IsVisibleTo(c.header) || c.langSel.IsVisibleTo(c.header))
}

func (c *codeEditor) setText(s string) { c.edit.SetPlainText(s) }
func (c *codeEditor) text() string     { return c.edit.ToPlainText() }

// setReadOnly switches editing; validation and assists only run while
// editable.
func (c *codeEditor) setReadOnly(ro bool) {
	c.edit.SetReadOnly(ro)
	c.completer.Popup().Hide()
	c.runCheck()
}

// load replaces the text and detects its language from contentType (falling
// back to sniffing the text). It forgets any language the user picked by
// hand, since this is new content.
func (c *codeEditor) load(text, contentType string) {
	c.setText(text)
	c.langManual = false
	c.setLanguage(detectLanguage(contentType, text))
}

// contentTypeChanged re-detects the language after the content type changed
// (e.g. its header was edited), unless the user has picked one by hand.
func (c *codeEditor) contentTypeChanged(contentType string) {
	if !c.langManual {
		c.setLanguage(detectLanguage(contentType, c.text()))
	}
}

// setLanguage sets the language from code: the selector follows, and it does
// not count as the user's choice.
func (c *codeEditor) setLanguage(lang Language) {
	c.settingLang = true
	for i, l := range allLanguages {
		if l == lang {
			c.langSel.SetCurrentIndex(i)
			break
		}
	}
	c.settingLang = false
	c.applyLanguage(lang)
}

// applyLanguage changes highlighting, and the checks when validating.
func (c *codeEditor) applyLanguage(lang Language) {
	c.lang = lang
	c.hl.setLanguage(lang)
	c.runCheck()
}

// runCheck re-checks the text now and redraws the problem markers.
func (c *codeEditor) runCheck() {
	c.checkTimer.Stop()
	c.issues, c.issueLines, c.nextIssue = nil, nil, 0
	if check := checkerFor(c.lang); c.validate && !c.edit.IsReadOnly() && check != nil {
		if text := c.text(); len(text) <= maxCheckSize {
			c.placeIssues(text, check(text))
		}
	}
	c.updateIssueBar()
	c.highlightCurrentLine() // also draws the problem underlines
	c.gutter.Update()
}

// placeIssues converts the checker's byte offsets into editor positions.
// Issues arrive sorted by offset, so one pass over the text suffices.
func (c *codeEditor) placeIssues(text string, found []syntaxcheck.Issue) {
	c.issueLines = map[int]bool{}
	pos, units := 0, 0
	advance := func(to int) int {
		for pos < to {
			r, n := utf8.DecodeRuneInString(text[pos:])
			pos += n
			units += utf16Len(r)
		}
		return units
	}
	for _, is := range found {
		start := advance(is.Offset)
		c.issues = append(c.issues, placedIssue{Issue: is, start: start})
		c.issueLines[is.Line-1] = true
	}
	// Ends may overlap later starts, so measure them separately.
	for i := range c.issues {
		is := &c.issues[i]
		is.end = is.start + utf16Count(text[is.Offset:is.Offset+is.Len])
		if is.end == is.start && is.start > 0 {
			is.start-- // a problem at the very end: mark the last character
		}
	}
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

func utf16Count(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Len(r)
	}
	return n
}

// updateIssueBar shows the first problem (as a link that jumps to it) and how
// many others there are; the tooltip lists them all.
func (c *codeEditor) updateIssueBar() {
	if len(c.issues) == 0 {
		c.issueBar.Hide()
		return
	}
	first := c.issues[0]
	text := fmt.Sprintf(`<a href="next" style="color:%s">Line %d, col %d</a>: %s`,
		color(colorServerError).Name(), first.Line, first.Col, html.EscapeString(first.Message))
	if n := len(c.issues) - 1; n > 0 {
		text += fmt.Sprintf(" &nbsp;·&nbsp; %d more %s", n, plural(n, "problem", "problems"))
	}
	var tip strings.Builder
	fmt.Fprintf(&tip, "%s problems:", c.lang)
	for _, is := range c.issues {
		fmt.Fprintf(&tip, "\nLine %d, col %d: %s", is.Line, is.Col, is.Message)
	}
	c.issueBar.SetText(text)
	c.issueBar.SetToolTip(tip.String())
	c.issueBar.Show()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// jumpToNextIssue moves the cursor to the next problem, cycling through them.
func (c *codeEditor) jumpToNextIssue() {
	if len(c.issues) == 0 {
		return
	}
	is := c.issues[c.nextIssue%len(c.issues)]
	c.nextIssue++
	cur := c.edit.TextCursor()
	cur.SetPosition(is.start)
	c.edit.SetTextCursor(cur)
	c.edit.CenterCursor()
	c.edit.SetFocus()
}

// showIssueTip shows the message of the problem under the mouse, if any.
func (c *codeEditor) showIssueTip(e *qt.QHelpEvent) {
	pos := c.edit.CursorForPosition(e.Pos()).Position()
	var msgs []string
	for _, is := range c.issues {
		// A position is the gap before a character, so the character under
		// the mouse is at pos or pos-1.
		if is.start <= pos && pos <= is.end {
			msgs = append(msgs, is.Message)
		}
	}
	if len(msgs) == 0 {
		qt.QToolTip_HideText()
		e.Ignore()
		return
	}
	qt.QToolTip_ShowText(e.GlobalPos(), strings.Join(msgs, "\n"))
}

// refreshTheme recolours the editor after a theme change: the highlighter
// rules, the current-line band and the gutter all read the active palette.
func (c *codeEditor) refreshTheme() {
	c.hl.setLanguage(c.lang) // rebuilds rules with the new palette, then rehighlights
	c.highlightCurrentLine()
	c.updateIssueBar() // its link colour comes from the palette
	c.gutter.Update()
}

func (c *codeEditor) gutterWidth() int {
	digits := 1
	for n := c.edit.BlockCount(); n >= 10; n /= 10 {
		digits++
	}
	return 12 + int(charWidth(c.edit))*digits
}

func (c *codeEditor) updateGutterWidth() {
	w := c.gutterWidth()
	c.gutter.SetFixedWidth(w)
	c.edit.SetViewportMargins(0, 0, 0, 0) // gutter is a sibling, not an overlay
}

func (c *codeEditor) paintGutter() {
	p := qt.NewQPainter2(c.gutter.QPaintDevice)
	defer p.End()
	p.FillRect5(0, 0, c.gutter.Width(), c.gutter.Height(), color(colorGutterBg))
	normal, problem := color(colorGutterText), color(colorServerError)

	block := c.edit.FirstVisibleBlock()
	offset := c.edit.ContentOffset()
	geo := c.edit.BlockBoundingGeometry(&block)
	top := geo.Translated(offset.X(), offset.Y()).Top()
	fm := qt.NewQFontMetrics(c.edit.Font())
	h := fm.Height()

	for block.IsValid() && int(top) <= c.gutter.Height() {
		if block.IsVisible() {
			if c.issueLines[block.BlockNumber()] {
				p.SetPen(problem)
			} else {
				p.SetPen(normal)
			}
			p.DrawText7(0, int(top), c.gutter.Width()-6, h,
				int(qt.AlignRight|qt.AlignVCenter), fmt.Sprint(block.BlockNumber()+1))
		}
		rect := c.edit.BlockBoundingRect(&block)
		top += rect.Height()
		block = *block.Next()
	}
}

func (c *codeEditor) highlightCurrentLine() {
	sel := qt.NewQTextEdit__ExtraSelection2()
	fmtt := qt.NewQTextCharFormat()
	fmtt.SetBackground(qt.NewQBrush3(color(colorCurrentLine)))
	fmtt.SetProperty(int(qt.QTextFormat__FullWidthSelection), qt.NewQVariant8(true))
	sel.SetFormat(*fmtt)
	cur := c.edit.TextCursor()
	cur.ClearSelection()
	sel.SetCursor(*cur)
	sels := []qt.QTextEdit__ExtraSelection{*sel}

	// Problem underlines go on top of the current-line band.
	if len(c.issues) > 0 {
		under := qt.NewQTextCharFormat()
		under.SetUnderlineStyle(qt.QTextCharFormat__WaveUnderline)
		under.SetUnderlineColor(color(colorServerError))
		doc := c.edit.Document()
		for _, is := range c.issues {
			u := qt.NewQTextEdit__ExtraSelection2()
			u.SetFormat(*under)
			ic := qt.NewQTextCursor2(doc)
			ic.SetPosition(is.start)
			ic.SetPosition2(is.end, qt.QTextCursor__KeepAnchor)
			u.SetCursor(*ic)
			sels = append(sels, *u)
		}
	}
	c.edit.SetExtraSelections(sels)
}
