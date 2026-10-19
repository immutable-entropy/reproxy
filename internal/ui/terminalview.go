package ui

import (
	"math"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/terminal"
)

// terminalView draws one terminal session and sends the keyboard to it. Like
// the navigation bar's icons it paints itself: a grid of character cells in
// the monospace font, in the colours the shell asked for.
//
// It holds no state of the shell's own — the screen it draws is a snapshot
// taken from the session each repaint (see terminal.Session.Snapshot), so
// the shell goes on writing while the paint runs.
type terminalView struct {
	w       *qt.QWidget
	session *terminal.Session
	screen  terminal.Screen

	font         *qt.QFont
	cellW, cellH float64
	ascent       float64

	// offset is how many lines back from the bottom the view is scrolled.
	offset int
	// focused draws the cursor filled rather than outlined, as a terminal
	// that isn't taking keys should look.
	focused bool

	// Selection, in screen cells. active is set while the mouse is down.
	selecting          bool
	hasSel             bool
	selFromX, selFromY int
	selToX, selToY     int

	// onSize is called when the grid's size in cells changes.
	onSize func(cols, rows int)
	// onScroll is called when the view scrolls itself (the wheel), so the
	// scrollbar beside it can follow.
	onScroll func()
}

const (
	// terminalPadX and terminalPadY are the margin around the grid.
	terminalPadX = 6
	terminalPadY = 4
	// wheelLines is how far one notch of the wheel scrolls.
	wheelLines = 3
)

func newTerminalView() *terminalView {
	v := &terminalView{w: qt.NewQWidget2()}
	setName(v.w.QObject, "TerminalView")
	v.w.SetAttribute(qt.WA_StyledBackground)
	v.w.SetFocusPolicy(qt.StrongFocus)
	v.w.SetCursor(qt.NewQCursor2(qt.IBeamCursor))
	v.setFont(monospaceFont())

	v.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { v.paint() })
	v.w.OnResizeEvent(func(_ func(*qt.QResizeEvent), _ *qt.QResizeEvent) { v.applySize() })
	v.w.OnKeyPressEvent(func(_ func(*qt.QKeyEvent), e *qt.QKeyEvent) { v.onKey(e) })
	v.w.OnWheelEvent(func(_ func(*qt.QWheelEvent), e *qt.QWheelEvent) { v.onWheel(e) })
	// Tab belongs to the shell, which completes with it. Qt would otherwise
	// take Tab and Shift+Tab to move the focus on before the key press
	// reaches the widget at all; refusing the traversal is what lets it
	// through, so the keyboard leaves the terminal by a click or a shortcut.
	v.w.OnFocusNextPrevChild(func(_ func(bool) bool, _ bool) bool { return false })
	v.w.OnFocusInEvent(func(_ func(*qt.QFocusEvent), _ *qt.QFocusEvent) { v.focused = true; v.w.Update() })
	v.w.OnFocusOutEvent(func(_ func(*qt.QFocusEvent), _ *qt.QFocusEvent) { v.focused = false; v.w.Update() })
	v.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.onMousePress(e) })
	v.w.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.onMouseMove(e) })
	v.w.OnMouseReleaseEvent(func(_ func(*qt.QMouseEvent), _ *qt.QMouseEvent) { v.selecting = false })
	v.w.SetMouseTracking(false)
	v.w.SetContextMenuPolicy(qt.CustomContextMenu)
	v.w.OnCustomContextMenuRequested(func(*qt.QPoint) { v.showMenu() })
	// While the terminal has the keyboard, the shell gets the key
	// combinations the window would otherwise take for itself: Ctrl+C is an
	// interrupt here, not Edit → Copy.
	v.w.OnEvent(func(super func(*qt.QEvent) bool, e *qt.QEvent) bool {
		if e.Type() == qt.QEvent__ShortcutOverride {
			ke := qt.UnsafeNewQKeyEvent(e.UnsafePointer())
			if terminalTakesKey(qt.Key(ke.Key()), ke.Modifiers()) {
				e.Accept() // delivered as an ordinary key press instead
				return true
			}
		}
		return super(e)
	})
	return v
}

// setFont measures the cell the font gives us: terminals are grids, so the
// width of one character decides everything else.
func (v *terminalView) setFont(f *qt.QFont) {
	v.font = f
	fm := qt.NewQFontMetricsF(f)
	v.cellW = fm.HorizontalAdvance("M")
	v.cellH = math.Ceil(fm.Height())
	v.ascent = fm.Ascent()
	if v.cellW < 1 {
		v.cellW = 8
	}
	if v.cellH < 1 {
		v.cellH = 14
	}
}

// setSession points the view at a session (nil for none) and resizes it to
// whatever the view currently is.
func (v *terminalView) setSession(s *terminal.Session) {
	v.session = s
	v.offset = 0
	v.clearSelection()
	v.applySize()
	v.w.Update()
}

// cols and rows are the grid the view has room for.
func (v *terminalView) cols() int {
	return max(int((float64(v.w.Width())-2*terminalPadX)/v.cellW), 1)
}

func (v *terminalView) rows() int {
	return max(int((float64(v.w.Height())-2*terminalPadY)/v.cellH), 1)
}

// applySize tells the session how big its screen is now.
func (v *terminalView) applySize() {
	cols, rows := v.cols(), v.rows()
	if v.session != nil {
		v.session.Resize(cols, rows)
	}
	if v.onSize != nil {
		v.onSize(cols, rows)
	}
}

// scrollTo moves the view to offset lines back from the bottom.
func (v *terminalView) scrollTo(offset int) {
	back := 0
	if v.session != nil {
		back = v.session.ScrollbackLen()
	}
	offset = min(max(offset, 0), back)
	if offset == v.offset {
		return
	}
	v.offset = offset
	v.w.Update()
}

func (v *terminalView) onWheel(e *qt.QWheelEvent) {
	if v.session == nil {
		return
	}
	if v.session.OnAltScreen() {
		// A full-screen program owns the screen: the wheel is its business,
		// and a terminal sends it the arrow keys for it.
		up := e.AngleDelta().Y() > 0
		for range wheelLines {
			if up {
				v.session.Key(terminal.KeyUp, 0)
			} else {
				v.session.Key(terminal.KeyDown, 0)
			}
		}
		return
	}
	notches := float64(e.AngleDelta().Y()) / 120
	v.scrollTo(v.offset + int(notches*wheelLines))
	if v.onScroll != nil {
		v.onScroll()
	}
}

// ---- Keyboard --------------------------------------------------------------

// terminalTakesKey decides who owns a key combination while the terminal
// has the keyboard. The shell has first claim — Ctrl+C, Ctrl+D, Ctrl+L,
// Ctrl+R and the rest are how a terminal is driven — so it takes every
// Ctrl or Alt combination, with two exceptions kept for the window: the
// digits that switch panels, and Ctrl+Shift+<key> (New Engagement and the
// like), out of which only Copy and Paste are the terminal's own.
func terminalTakesKey(k qt.Key, mods qt.KeyboardModifier) bool {
	ctrl := mods&qt.ControlModifier != 0
	alt := mods&qt.AltModifier != 0
	shift := mods&qt.ShiftModifier != 0
	if !ctrl && !alt {
		return false // plain keys are delivered to the widget anyway
	}
	if ctrl && k >= qt.Key_0 && k <= qt.Key_9 {
		return false
	}
	if ctrl && shift {
		return k == qt.Key_C || k == qt.Key_V
	}
	return true
}

// onKey turns a Qt key press into what the shell expects. Plain typing goes
// as the text it produced; anything with Ctrl or Alt, and every named key,
// is sent as a key so the emulator can encode it for the mode the program
// running has set.
func (v *terminalView) onKey(e *qt.QKeyEvent) {
	if v.session == nil {
		return
	}
	mods := e.Modifiers()
	ctrl := mods&qt.ControlModifier != 0
	shift := mods&qt.ShiftModifier != 0
	alt := mods&qt.AltModifier != 0

	// The terminal's own shortcuts, as every terminal has them: the shell
	// gets Ctrl+C, so copying is Ctrl+Shift+C.
	if ctrl && shift {
		switch qt.Key(e.Key()) {
		case qt.Key_C:
			v.copySelection()
			return
		case qt.Key_V:
			v.paste()
			return
		}
	}
	// Reading back through the output: Shift+PgUp/PgDn, as in a console.
	if shift && !ctrl {
		switch qt.Key(e.Key()) {
		case qt.Key_PageUp:
			v.scrollTo(v.offset + v.rows() - 1)
			v.notifyScroll()
			return
		case qt.Key_PageDown:
			v.scrollTo(v.offset - v.rows() + 1)
			v.notifyScroll()
			return
		}
	}

	var mod terminal.Mod
	if ctrl {
		mod |= terminal.ModCtrl
	}
	if alt {
		mod |= terminal.ModAlt
	}
	if shift {
		mod |= terminal.ModShift
	}

	if code, ok := namedKey(qt.Key(e.Key())); ok {
		v.sendKey(code, mod)
		return
	}
	if ctrl || alt {
		// Ctrl/Alt with a character: the emulator knows what byte that is.
		if text := strings.ToLower(e.Text()); text != "" && !ctrl {
			v.sendKey(terminal.Code([]rune(text)[0]), mod)
			return
		}
		if key := qt.Key(e.Key()); key >= qt.Key_Space && key <= qt.Key_AsciiTilde {
			v.sendKey(terminal.Code(strings.ToLower(string(rune(key)))[0]), mod)
			return
		}
	}
	if text := e.Text(); text != "" {
		v.scrollTo(0) // typing returns to the prompt, as every terminal does
		v.session.Text(text)
	}
}

// sendKey sends a named or modified key and returns to the live screen.
func (v *terminalView) sendKey(code terminal.Code, mod terminal.Mod) {
	v.scrollTo(0)
	v.session.Key(code, mod)
	v.notifyScroll()
}

func (v *terminalView) notifyScroll() {
	if v.onScroll != nil {
		v.onScroll()
	}
}

// namedKey maps the keys that aren't text to the emulator's own.
func namedKey(k qt.Key) (terminal.Code, bool) {
	switch k {
	case qt.Key_Up:
		return terminal.KeyUp, true
	case qt.Key_Down:
		return terminal.KeyDown, true
	case qt.Key_Left:
		return terminal.KeyLeft, true
	case qt.Key_Right:
		return terminal.KeyRight, true
	case qt.Key_Home:
		return terminal.KeyHome, true
	case qt.Key_End:
		return terminal.KeyEnd, true
	case qt.Key_PageUp:
		return terminal.KeyPgUp, true
	case qt.Key_PageDown:
		return terminal.KeyPgDown, true
	case qt.Key_Insert:
		return terminal.KeyInsert, true
	case qt.Key_Delete:
		return terminal.KeyDelete, true
	case qt.Key_Backspace:
		return terminal.KeyBackspc, true
	case qt.Key_Return, qt.Key_Enter:
		return terminal.KeyEnter, true
	case qt.Key_Tab, qt.Key_Backtab:
		// Shift+Tab arrives as Backtab; the modifier goes with it, and the
		// emulator encodes that pair as CSI Z.
		return terminal.KeyTab, true
	case qt.Key_Escape:
		return terminal.KeyEscape, true
	case qt.Key_F1:
		return terminal.KeyF1, true
	case qt.Key_F2:
		return terminal.KeyF2, true
	case qt.Key_F3:
		return terminal.KeyF3, true
	case qt.Key_F4:
		return terminal.KeyF4, true
	case qt.Key_F5:
		return terminal.KeyF5, true
	case qt.Key_F6:
		return terminal.KeyF6, true
	case qt.Key_F7:
		return terminal.KeyF7, true
	case qt.Key_F8:
		return terminal.KeyF8, true
	case qt.Key_F9:
		return terminal.KeyF9, true
	case qt.Key_F10:
		return terminal.KeyF10, true
	case qt.Key_F11:
		return terminal.KeyF11, true
	case qt.Key_F12:
		return terminal.KeyF12, true
	}
	return 0, false
}

// ---- Selection and the clipboard -------------------------------------------

func (v *terminalView) cellAtPos(x, y float64) (int, int) {
	cx := int((x - terminalPadX) / v.cellW)
	cy := int((y - terminalPadY) / v.cellH)
	return min(max(cx, 0), v.cols()-1), min(max(cy, 0), v.rows()-1)
}

func (v *terminalView) onMousePress(e *qt.QMouseEvent) {
	v.w.SetFocus()
	if e.Button() != qt.LeftButton {
		return
	}
	pos := e.Position()
	x, y := v.cellAtPos(pos.X(), pos.Y())
	v.selecting, v.hasSel = true, false
	v.selFromX, v.selFromY, v.selToX, v.selToY = x, y, x, y
	v.w.SetMouseTracking(true)
	v.w.Update()
}

func (v *terminalView) onMouseMove(e *qt.QMouseEvent) {
	if !v.selecting {
		return
	}
	pos := e.Position()
	v.selToX, v.selToY = v.cellAtPos(pos.X(), pos.Y())
	v.hasSel = v.selToX != v.selFromX || v.selToY != v.selFromY
	v.w.Update()
}

func (v *terminalView) clearSelection() {
	v.hasSel, v.selecting = false, false
	v.w.SetMouseTracking(false)
	v.w.Update()
}

// selectedText is what the selection covers, as the screen shows it.
func (v *terminalView) selectedText() string {
	if !v.hasSel {
		return ""
	}
	return v.screen.Text(v.selFromX, v.selFromY, v.selToX, v.selToY)
}

func (v *terminalView) copySelection() {
	if text := v.selectedText(); text != "" {
		qt.QGuiApplication_Clipboard().SetText(text)
	}
}

func (v *terminalView) paste() {
	if v.session == nil {
		return
	}
	if text := qt.QGuiApplication_Clipboard().Text(); text != "" {
		v.scrollTo(0)
		v.session.Paste(text)
	}
}

// showMenu is the right-click menu: what a terminal needs and nothing else.
func (v *terminalView) showMenu() {
	menu := qt.NewQMenu2()
	copyAct := menu.AddActionWithText("Copy")
	copyAct.SetEnabled(v.hasSel)
	copyAct.OnTriggered(v.copySelection)
	menu.AddActionWithText("Paste").OnTriggered(v.paste)
	menu.AddSeparator()
	menu.AddActionWithText("Select All").OnTriggered(func() {
		v.selFromX, v.selFromY = 0, 0
		v.selToX, v.selToY = v.cols()-1, v.rows()-1
		v.hasSel = true
		v.w.Update()
	})
	menu.Exec3(qt.QCursor_Pos(), nil)
}

// ---- Painting --------------------------------------------------------------

// selectionBounds returns the selection in top-left to bottom-right order.
func (v *terminalView) selectionBounds() (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = v.selFromX, v.selFromY, v.selToX, v.selToY
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	return
}

// selected reports whether the cell at (x, y) is inside the selection.
func (v *terminalView) selected(x, y int) bool {
	if !v.hasSel {
		return false
	}
	x0, y0, x1, y1 := v.selectionBounds()
	switch {
	case y < y0 || y > y1:
		return false
	case y0 == y1:
		return x >= x0 && x <= x1
	case y == y0:
		return x >= x0
	case y == y1:
		return x <= x1
	default:
		return true
	}
}

func (v *terminalView) paint() {
	p := qt.NewQPainter2(v.w.QPaintDevice)
	defer p.End()
	p.SetFont(v.font)

	bg, fg := color("surface"), color(colorText)
	p.FillRect5(0, 0, v.w.Width(), v.w.Height(), bg)
	if v.session == nil {
		return
	}
	v.session.Snapshot(v.offset, &v.screen)

	for y := range v.screen.Rows {
		top := terminalPadY + float64(y)*v.cellH
		// Backgrounds first, in runs, so a line of colour is one rectangle
		// rather than one per cell.
		for x := 0; x < v.screen.Cols; {
			c := v.screen.At(x, y)
			cellBG, _ := cellColors(c, bg, fg)
			sel := v.selected(x, y)
			run := 1
			for x+run < v.screen.Cols {
				n := v.screen.At(x+run, y)
				nbg, _ := cellColors(n, bg, fg)
				if nbg.Name() != cellBG.Name() || v.selected(x+run, y) != sel {
					break
				}
				run++
			}
			if sel {
				cellBG = color("selectionBg")
			}
			if sel || cellBG.Name() != bg.Name() {
				p.FillRect4(qt.NewQRectF4(terminalPadX+float64(x)*v.cellW, top,
					float64(run)*v.cellW, v.cellH), cellBG)
			}
			x += run
		}
		// Then the characters.
		for x := range v.screen.Cols {
			c := v.screen.At(x, y)
			if c.Blank() || c.Attrs&terminal.AttrConceal != 0 {
				continue
			}
			_, cellFG := cellColors(c, bg, fg)
			if c.Attrs&terminal.AttrFaint != 0 {
				cellFG = fadeColor(cellFG, bg)
			}
			f := v.font
			if c.Attrs&(terminal.AttrBold|terminal.AttrItalic|terminal.AttrUnderline|terminal.AttrStrike) != 0 {
				f = qt.NewQFont5(v.font)
				f.SetBold(c.Attrs&terminal.AttrBold != 0)
				f.SetItalic(c.Attrs&terminal.AttrItalic != 0)
				f.SetUnderline(c.Attrs&terminal.AttrUnderline != 0)
				f.SetStrikeOut(c.Attrs&terminal.AttrStrike != 0)
			}
			p.SetFont(f)
			p.SetPen(cellFG)
			p.DrawText(qt.NewQPointF3(terminalPadX+float64(x)*v.cellW, top+v.ascent), c.Text)
		}
	}
	v.paintCursor(p, fg)
}

// paintCursor draws the block cursor: filled while the view has the
// keyboard, outlined when it doesn't, as terminals have always shown it.
func (v *terminalView) paintCursor(p *qt.QPainter, fg *qt.QColor) {
	if !v.screen.CursorVisible || v.screen.CursorY >= v.screen.Rows {
		return
	}
	r := qt.NewQRectF4(terminalPadX+float64(v.screen.CursorX)*v.cellW,
		terminalPadY+float64(v.screen.CursorY)*v.cellH, v.cellW, v.cellH)
	if !v.focused {
		p.SetPen(fg)
		p.SetBrushWithStyle(qt.NoBrush)
		p.DrawRect(r)
		return
	}
	p.FillRect4(r, fg)
	c := v.screen.At(v.screen.CursorX, v.screen.CursorY)
	if !c.Blank() {
		p.SetPen(color("surface"))
		p.SetFont(v.font)
		p.DrawText(qt.NewQPointF3(r.X(), r.Y()+v.ascent), c.Text)
	}
}

// cellColors resolves a cell's colours against the view's own, reverse video
// included.
func cellColors(c terminal.Cell, defBG, defFG *qt.QColor) (bg, fg *qt.QColor) {
	bg, fg = resolveColor(c.BG, defBG), resolveColor(c.FG, defFG)
	if c.Attrs&terminal.AttrReverse != 0 {
		bg, fg = fg, bg
	}
	return bg, fg
}

// resolveColor turns one of the emulator's colours into one to paint with.
// The sixteen a shell names — the blue of a path, the green of a prompt —
// are the theme's to choose, so they come from the palette and follow the
// theme the rest of the window is in; a colour the program gave as an index
// into the 256-colour palette, or in full, is its own and is used as it is.
func resolveColor(c terminal.Color, def *qt.QColor) *qt.QColor {
	switch c.Kind {
	case terminal.ColorANSI:
		return color(terminalANSI(c.Index))
	case terminal.ColorIndexed, terminal.ColorRGB:
		return qt.NewQColor3(int(c.R), int(c.G), int(c.B))
	default:
		return def
	}
}

// fadeColor is a dim (SGR 2) colour: halfway to the background.
func fadeColor(c, bg *qt.QColor) *qt.QColor {
	return qt.NewQColor3((c.Red()+bg.Red())/2, (c.Green()+bg.Green())/2, (c.Blue()+bg.Blue())/2)
}
