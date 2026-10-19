package ui

import qt "github.com/mappu/miqt/qt6"

// iconButton is a flat button showing one painted icon (see navicons.go).
// Like the navigation bar's items it paints itself, so it follows the active
// theme without any icon files, and it stays legible at any scale factor.
//
// Use it where a row needs an action that shouldn't shout: editing an
// target, reordering a rule, removing a row.
type iconButton struct {
	w    *qt.QWidget
	icon iconKind
	// size is the icon's extent; the button is a little larger.
	size int
	// danger tints the icon red on hover, for actions that remove something.
	danger bool
	// active tints the icon in the accent colour, for a button that reports
	// a state as well as offering one: a filter that is hiding something.
	active  bool
	hovered bool
	enabled bool

	onClick func()
}

const (
	iconButtonSize = 18 // icon extent
	iconButtonPad  = 5  // around the icon
)

// newIconButton returns a button of the standard size. tip is its tooltip,
// which is the only label it has.
func newIconButton(icon iconKind, tip string, onClick func()) *iconButton {
	b := &iconButton{w: qt.NewQWidget2(), icon: icon, size: iconButtonSize, enabled: true, onClick: onClick}
	b.w.SetToolTip(tip)
	b.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	b.w.SetFixedSize2(b.size+2*iconButtonPad, b.size+2*iconButtonPad)
	b.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { b.paint() })
	b.w.OnEnterEvent(func(_ func(*qt.QEnterEvent), _ *qt.QEnterEvent) {
		b.hovered = true
		b.w.Update()
	})
	b.w.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) {
		b.hovered = false
		b.w.Update()
	})
	b.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if e.Button() == qt.LeftButton && b.enabled && b.onClick != nil {
			b.onClick()
		}
	})
	return b
}

// asDanger makes the icon turn red on hover (removing something).
func (b *iconButton) asDanger() *iconButton {
	b.danger = true
	return b
}

// setEnabled greys the button out and stops it responding.
func (b *iconButton) setEnabled(on bool) {
	b.enabled = on
	b.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	if !on {
		b.w.SetCursor(qt.NewQCursor2(qt.ArrowCursor))
	}
	b.w.Update()
}

// setActive tints the icon, for a button whose state matters as much as its
// action (a filter narrowing the list under it).
func (b *iconButton) setActive(on bool) {
	if b.active != on {
		b.active = on
		b.w.Update()
	}
}

// setHovered re-states whether the pointer is on the button, for after
// something took the mouse away from it (a menu dropping from it swallows
// the leave event).
func (b *iconButton) setHovered(on bool) {
	if b.hovered != on {
		b.hovered = on
		b.w.Update()
	}
}

// setIcon swaps the painted icon, for a button that reports a state as well
// as offering an action.
func (b *iconButton) setIcon(icon iconKind) {
	if b.icon != icon {
		b.icon = icon
		b.w.Update()
	}
}

func (b *iconButton) paint() {
	p := qt.NewQPainter2(b.w.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)

	w, h := float64(b.w.Width()), float64(b.w.Height())
	ink := colorMuted
	switch {
	case !b.enabled:
		ink = "textDisabled"
	case b.hovered && b.danger:
		ink = colorServerError
	case b.active:
		ink = colorAccent // what it reports outweighs the pointer being on it
	case b.hovered:
		ink = colorText
	}
	if b.hovered && b.enabled {
		p.SetPenWithStyle(qt.NoPen)
		bg := colorHover
		if b.danger {
			bg = "dangerBg"
		}
		p.SetBrush(qt.NewQBrush3(color(bg)))
		p.DrawRoundedRect(qt.NewQRectF4(0, 0, w, h), 6, 6)
	}
	drawIcon(p, b.icon, (w-float64(b.size))/2, (h-float64(b.size))/2, float64(b.size), color(ink))
}

// colorSwatch is a small rounded square filled with a target's colour,
// used in its list row and beside its name. It is a plain painted widget so
// it picks up a colour change without rebuilding anything around it.
type colorSwatch struct {
	w   *qt.QWidget
	hex string
}

func newColorSwatch(hexColor string, size int) *colorSwatch {
	s := &colorSwatch{w: qt.NewQWidget2(), hex: hexColor}
	s.w.SetFixedSize2(size, size)
	s.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) {
		p := qt.NewQPainter2(s.w.QPaintDevice)
		defer p.End()
		p.SetRenderHint(qt.QPainter__Antialiasing)
		p.SetPenWithStyle(qt.NoPen)
		p.SetBrush(qt.NewQBrush3(qt.NewQColor6(s.hex)))
		p.DrawRoundedRect(qt.NewQRectF4(0, 0, float64(s.w.Width()), float64(s.w.Height())), 4, 4)
	})
	return s
}

func (s *colorSwatch) setColor(hexColor string) {
	s.hex = hexColor
	s.w.Update()
}

// segmentedIcons is a row of icon buttons of which exactly one is on, such
// as the Explorer's choice between its tree and graph views. Like iconButton it
// paints itself; tips are the segments' tooltips, their only labels.
type segmentedIcons struct {
	w       *qt.QWidget
	icons   []iconKind
	current int
	hovered int

	onChange func(int)
}

const segmentWidth = 30

func newSegmentedIcons(icons []iconKind, tips []string, onChange func(int)) *segmentedIcons {
	s := &segmentedIcons{w: qt.NewQWidget2(), icons: icons, hovered: -1, onChange: onChange}
	s.w.SetFixedSize2(segmentWidth*len(icons), iconButtonSize+2*iconButtonPad)
	s.w.SetMouseTracking(true)
	s.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	s.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { s.paint() })
	s.w.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if i := s.at(e.Position().X()); i != s.hovered {
			s.hovered = i
			s.w.SetToolTip(tips[i])
			s.w.Update()
		}
	})
	s.w.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) {
		s.hovered = -1
		s.w.Update()
	})
	s.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if i := s.at(e.Position().X()); e.Button() == qt.LeftButton && i != s.current {
			s.setCurrent(i)
			if s.onChange != nil {
				s.onChange(i)
			}
		}
	})
	return s
}

func (s *segmentedIcons) at(x float64) int {
	i := int(x) / segmentWidth
	return max(0, min(i, len(s.icons)-1))
}

// setCurrent turns segment i on without calling onChange.
func (s *segmentedIcons) setCurrent(i int) {
	s.current = i
	s.w.Update()
}

func (s *segmentedIcons) paint() {
	p := qt.NewQPainter2(s.w.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)

	h := float64(s.w.Height())
	frame := qt.NewQRectF4(0.5, 0.5, float64(s.w.Width())-1, h-1)
	clip := qt.NewQPainterPath()
	clip.AddRoundedRect(frame, 6, 6)
	p.SetClipPath(clip)
	for i, icon := range s.icons {
		x := float64(i * segmentWidth)
		ink := colorMuted
		switch {
		case i == s.current:
			p.FillRect4(qt.NewQRectF4(x, 0, segmentWidth, h), color(colorSelected))
			ink = colorAccent
		case i == s.hovered:
			p.FillRect4(qt.NewQRectF4(x, 0, segmentWidth, h), color(colorHover))
			ink = colorText
		}
		const size = 16.0
		drawIcon(p, icon, x+(segmentWidth-size)/2, (h-size)/2, size, color(ink))
	}
	p.SetClipping(false)
	pen := qt.NewQPen3(color("inputBorder"))
	p.SetPenWithPen(pen)
	p.SetBrushWithStyle(qt.NoBrush)
	p.DrawRoundedRect(frame, 6, 6)
	for i := 1; i < len(s.icons); i++ {
		x := float64(i*segmentWidth) + 0.5
		p.DrawLine4(qt.NewQPointF3(x, 0), qt.NewQPointF3(x, h))
	}
}
