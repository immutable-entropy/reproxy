package terminal

import (
	"image/color"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Attr is a cell's styling, as the shell asked for it.
type Attr uint8

// The attributes a cell can carry. Blinking is deliberately not one of
// them: a blinking cell is drawn like any other.
const (
	AttrBold Attr = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrReverse
	AttrStrike
	AttrConceal
)

// ColorKind says where a cell's colour came from, which decides who gets to
// choose it. The sixteen ANSI colours are names, not values — a shell that
// asks for "blue" means the terminal's blue — so they are passed on as names
// and the view resolves them against the theme. The 256-colour palette and
// true colour are values the program picked itself and are passed through.
type ColorKind uint8

const (
	// ColorDefault is the terminal's own foreground or background.
	ColorDefault ColorKind = iota
	// ColorANSI is one of the sixteen named colours; Index is 0–15, the
	// first eight normal and the rest bright.
	ColorANSI
	// ColorIndexed is a colour from the 256-colour palette (Index 16–255).
	ColorIndexed
	// ColorRGB is a colour the program gave in full.
	ColorRGB
)

// Color is a cell's colour. R, G and B carry the emulator's own idea of it
// whatever the kind, so a caller that doesn't theme the named colours can
// use them as they are.
type Color struct {
	Kind    ColorKind
	Index   uint8
	R, G, B uint8
}

// Set reports whether the cell asked for a colour at all.
func (c Color) Set() bool { return c.Kind != ColorDefault }

// Cell is one character cell of the screen.
type Cell struct {
	// Text is the grapheme in the cell: usually one rune, empty for a
	// blank, and empty too for the second half of a double-width character
	// (the first half carries Width 2 and is drawn across both).
	Text   string
	FG, BG Color
	Attrs  Attr
	Width  int
}

// Blank reports whether the cell has nothing to draw but its background.
func (c Cell) Blank() bool { return c.Text == "" || c.Text == " " }

// Screen is a copy of what a session's terminal shows, taken for one repaint.
// Rows run top to bottom; Cells is row-major and Cols*Rows long.
type Screen struct {
	Cols, Rows       int
	Cells            []Cell
	CursorX, CursorY int
	CursorVisible    bool
	// Offset is how many lines back from the bottom this snapshot was
	// taken, and Scrollback how many lines are kept in all, so a view can
	// size its scrollbar. While Offset is non-zero the cursor is not shown:
	// the user is reading, not typing at this spot.
	Offset, Scrollback int
	// AltScreen is set while a full-screen program is running.
	AltScreen bool
	// Exited is set once the shell has ended.
	Exited bool
}

// At returns the cell at (x, y), or a blank one outside the screen.
func (s *Screen) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= s.Cols || y >= s.Rows {
		return Cell{}
	}
	return s.Cells[y*s.Cols+x]
}

// Snapshot fills dst with the session's screen, scrolled back by offset
// lines (0 is the live screen at the bottom). dst's storage is reused, so a
// view can keep one Screen and snapshot into it every repaint.
//
// It is the one place the emulator's types cross into ours, and it holds the
// session's lock for the copy alone: the shell goes on writing into the
// emulator the moment it returns.
func (s *Session) Snapshot(offset int, dst *Screen) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cols, rows := s.cols, s.rows
	back := s.emu.ScrollbackLen()
	if s.emu.IsAltScreen() {
		back = 0 // a full-screen program owns the whole view
	}
	offset = min(max(offset, 0), back)

	dst.Cols, dst.Rows = cols, rows
	dst.Offset, dst.Scrollback = offset, back
	dst.AltScreen = s.emu.IsAltScreen()
	dst.Exited = s.exited
	if n := cols * rows; cap(dst.Cells) < n {
		dst.Cells = make([]Cell, n)
	} else {
		dst.Cells = dst.Cells[:n]
		clear(dst.Cells)
	}

	for y := range rows {
		// The rows above the live screen come from the scrollback, newest
		// last: at offset n, the top row is the nth line from its end.
		from := y - offset
		for x := range cols {
			var c *uv.Cell
			if from < 0 {
				c = s.emu.ScrollbackCellAt(x, back+from)
			} else {
				c = s.emu.CellAt(x, from)
			}
			dst.Cells[y*cols+x] = convert(c)
		}
	}

	dst.CursorVisible = !s.emu.CursorHidden() && offset == 0 && !s.exited
	pos := s.emu.CursorPosition()
	dst.CursorX, dst.CursorY = pos.X, pos.Y
}

// convert turns an emulator cell into ours. A nil cell is a blank one: the
// emulator returns nil outside its buffer and for lines the scrollback
// trimmed to their last written character.
func convert(c *uv.Cell) Cell {
	if c == nil {
		return Cell{Width: 1}
	}
	out := Cell{Text: c.Content, Width: c.Width}
	if out.Width == 0 && out.Text != "" {
		out.Width = 1
	}
	out.FG, out.BG = toColor(c.Style.Fg), toColor(c.Style.Bg)
	if c.Style.Underline != uv.UnderlineNone {
		out.Attrs |= AttrUnderline
	}
	for _, m := range []struct {
		flag uint8
		attr Attr
	}{
		{uv.AttrBold, AttrBold},
		{uv.AttrFaint, AttrFaint},
		{uv.AttrItalic, AttrItalic},
		{uv.AttrReverse, AttrReverse},
		{uv.AttrStrikethrough, AttrStrike},
		{uv.AttrConceal, AttrConceal},
	} {
		if c.Style.Attrs&m.flag != 0 {
			out.Attrs |= m.attr
		}
	}
	return out
}

// toColor records which of the three kinds of colour a cell carries, and
// its RGB whatever the kind: every colour the emulator holds knows its own
// RGBA. A named colour keeps its index so the view can draw the theme's
// blue rather than the palette's — the difference between a readable prompt
// and a dark blue path on a dark background. A program that redefines one
// of the sixteen with OSC 4 is therefore not followed; on those the theme
// has the last word.
func toColor(c color.Color) Color {
	if c == nil {
		return Color{}
	}
	out := Color{Kind: ColorRGB}
	switch v := c.(type) {
	case ansi.BasicColor: // SGR 30–37, 40–47, 90–97, 100–107
		out.Kind, out.Index = ColorANSI, uint8(v)
	case ansi.IndexedColor: // SGR 38/48;5;n (ansi.ExtendedColor is the same type)
		out.Index = uint8(v)
		out.Kind = ColorIndexed
		if v < 16 {
			out.Kind = ColorANSI
		}
	}
	r, g, b, _ := c.RGBA()
	out.R, out.G, out.B = uint8(r>>8), uint8(g>>8), uint8(b>>8)
	return out
}

// Text returns the text of the given rows of the snapshot, with trailing
// blanks on each line dropped, for copying a selection.
func (s *Screen) Text(fromX, fromY, toX, toY int) string {
	if fromY > toY || (fromY == toY && fromX > toX) {
		fromX, fromY, toX, toY = toX, toY, fromX, fromY
	}
	var out []byte
	for y := max(fromY, 0); y <= min(toY, s.Rows-1); y++ {
		x0, x1 := 0, s.Cols-1
		if y == fromY {
			x0 = max(fromX, 0)
		}
		if y == toY {
			x1 = min(toX, s.Cols-1)
		}
		line := make([]byte, 0, (x1-x0+1)*2)
		for x := x0; x <= x1; x++ {
			c := s.At(x, y)
			if c.Text == "" {
				if c.Width == 0 {
					continue // the far half of a double-width character
				}
				line = append(line, ' ')
				continue
			}
			line = append(line, c.Text...)
		}
		for len(line) > 0 && line[len(line)-1] == ' ' {
			line = line[:len(line)-1]
		}
		if y > fromY {
			out = append(out, '\n')
		}
		out = append(out, line...)
	}
	return string(out)
}
