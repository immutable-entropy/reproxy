package ui

import (
	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/findings"
)

// A pill is a finding type as the Findings list draws it: a small squared
// badge in the type's own colour, with the type's name in whichever of black
// or white reads against it.
//
// Pills are painted rather than styled, because their colour comes from the
// user's type catalog (Settings → Findings) and not from the theme — the
// stylesheet has no way to know it. The same widget serves two jobs: a
// filled pill labels a finding, and a clickable outlined one filters the
// list by that type.

const (
	pillHeight  = 18
	pillPadX    = 7
	pillRadius  = 3 // squared, not a lozenge
	pillSpacing = 5
)

type pill struct {
	w     *qt.QWidget
	label string
	// hex is the type's colour, or "" for a type that is no longer
	// configured, which is drawn in the theme's neutral chip colours.
	hex string

	clickable bool
	on        bool
	hovered   bool
	onClick   func()
}

// newPill returns a filled pill labelling a finding.
func newPill(label, hex string) *pill {
	p := &pill{w: qt.NewQWidget2(), label: label, hex: hex, on: true}
	p.w.SetFixedHeight(pillHeight)
	p.w.OnSizeHint(func(func() *qt.QSize) *qt.QSize { return qt.NewQSize2(p.width(), pillHeight) })
	p.w.SetSizePolicy2(qt.QSizePolicy__Fixed, qt.QSizePolicy__Fixed)
	p.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { p.paint() })
	return p
}

// newFilterPill returns a pill the user can click to switch a type filter on
// and off. It is outlined while off and filled while on.
func newFilterPill(label, hex string, onClick func()) *pill {
	p := newPill(label, hex)
	p.clickable, p.on, p.onClick = true, false, onClick
	p.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	p.w.SetToolTip("Show only findings tagged " + label)
	p.w.OnEnterEvent(func(_ func(*qt.QEnterEvent), _ *qt.QEnterEvent) {
		p.hovered = true
		p.w.Update()
	})
	p.w.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) {
		p.hovered = false
		p.w.Update()
	})
	p.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if e.Button() == qt.LeftButton && p.onClick != nil {
			p.onClick()
		}
	})
	return p
}

func (p *pill) setOn(on bool) {
	if p.on != on {
		p.on = on
		p.w.Update()
	}
}

func (p *pill) width() int {
	return qt.NewQFontMetrics(p.pillFont()).HorizontalAdvance(p.label) + 2*pillPadX
}

// pillFont is the label's font: a copy of the widget's, a little smaller and
// bold, so a row of pills reads as badges rather than as text.
func (p *pill) pillFont() *qt.QFont {
	font := qt.NewQFont5(p.w.Font())
	font.SetPointSizeF(font.PointSizeF() - 1)
	font.SetBold(true)
	return font
}

func (p *pill) paint() {
	painter := qt.NewQPainter2(p.w.QPaintDevice)
	defer painter.End()
	painter.SetRenderHint(qt.QPainter__Antialiasing)
	painter.SetRenderHint(qt.QPainter__TextAntialiasing)
	painter.SetFont(p.pillFont())

	rect := qt.NewQRectF4(0.5, 0.5, float64(p.w.Width())-1, float64(p.w.Height())-1)
	fill, ink := p.colors()

	if p.on {
		painter.SetPenWithStyle(qt.NoPen)
		painter.SetBrush(qt.NewQBrush3(fill))
	} else {
		// Off: the colour as an outline, so the row still reads as that type.
		painter.SetPen(fill)
		bg := color("surface")
		if p.hovered {
			bg = color(colorHover)
		}
		painter.SetBrush(qt.NewQBrush3(bg))
	}
	painter.DrawRoundedRect(rect, pillRadius, pillRadius)

	if !p.on {
		ink = fill
	}
	painter.SetPen(ink)
	painter.DrawText5(rect, int(qt.AlignCenter), p.label)
}

// colors returns the pill's background and the ink that reads on it. A type
// that is no longer configured has no colour of its own, so it borrows the
// theme's neutral chip.
func (p *pill) colors() (fill, ink *qt.QColor) {
	if p.hex == "" {
		return color("chipBg"), color("textSubtle")
	}
	fill = qt.NewQColor6(p.hex)
	if !fill.IsValid() {
		return color("chipBg"), color("textSubtle")
	}
	return fill, inkOn(fill)
}

// inkOn picks black or white for text on a background, by the background's
// perceived brightness. The type colours are the user's to choose, so the
// label has to cope with any of them.
func inkOn(bg *qt.QColor) *qt.QColor {
	// Rec. 601 luma, which tracks perceived brightness well enough to
	// decide between two inks.
	luma := 0.299*float64(bg.Red()) + 0.587*float64(bg.Green()) + 0.114*float64(bg.Blue())
	if luma > 150 {
		return qt.NewQColor6("#1f2328")
	}
	return qt.NewQColor6("#ffffff")
}

// pillRow lays out one finding's type pills, left to right, ending with a
// stretch so they pack together. An untyped finding gets one neutral pill:
// the row never comes out blank, since a finding with no pills would look
// like a rendering fault rather than a choice.
func pillRow(types []string, catalog func(string) (label, hex string)) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(pillSpacing)
	if len(types) == 0 {
		h.AddWidget(newPill("Untyped", "").w)
	}
	for _, id := range types {
		label, hex := catalog(id)
		h.AddWidget(newPill(label, hex).w)
	}
	h.AddStretch()
	return row
}

// typeLabel resolves a type ID against a catalog for display: its name and
// colour, or the bare ID in neutral colours when the type has been removed
// from Settings → Findings. A finding keeps its type IDs, so deleting a type
// must never make its findings unreadable.
func typeLabel(types []findings.Type, id string) (label, hex string) {
	for _, t := range types {
		if t.ID == id {
			return t.Name, t.Color
		}
	}
	return id, ""
}
