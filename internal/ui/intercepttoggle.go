package ui

import qt "github.com/mappu/miqt/qt6"

// interceptToggle is the button at the right-hand end of the menu bar that
// switches interception on and off. It is a solid block of colour, green
// while interception is on and red while it is off, so the state reads at a
// glance, with the intercept icon painted over it in white.
//
// It doesn't switch anything itself: a click calls onClick (which toggles
// the interception action), and the window calls setOn once the
// interceptor has changed, so the button, the menu tick and Ctrl+I always
// agree.
type interceptToggle struct {
	w       *qt.QWidget
	on      bool
	hovered bool
	onClick func()
}

func newInterceptToggle(bar *qt.QMenuBar, on bool, onClick func()) *interceptToggle {
	t := &interceptToggle{w: qt.NewQWidget2(), on: on, onClick: onClick}
	t.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	// As tall as a menu bar item (font height plus the 5px top and bottom
	// padding in theme.qss), so it fills the bar just as the menus do.
	h := bar.FontMetrics().Height() + 10
	t.w.SetFixedSize2(h+10, h)
	// QMenuBar places its corner widget by size hint, which a plain QWidget
	// doesn't have.
	t.w.OnSizeHint(func(_ func() *qt.QSize) *qt.QSize { return qt.NewQSize2(h+10, h) })
	t.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { t.paint() })
	t.w.OnEnterEvent(func(_ func(*qt.QEnterEvent), _ *qt.QEnterEvent) {
		t.hovered = true
		t.w.Update()
	})
	t.w.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) {
		t.hovered = false
		t.w.Update()
	})
	t.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if e.Button() == qt.LeftButton && t.onClick != nil {
			t.onClick()
		}
	})
	t.setOn(on)
	return t
}

// setOn shows interception as on or off.
func (t *interceptToggle) setOn(on bool) {
	t.on = on
	if on {
		t.w.SetToolTip("Interception on — click to turn off (Ctrl+I)")
	} else {
		t.w.SetToolTip("Interception off — click to turn on (Ctrl+I)")
	}
	t.w.Update()
}

func (t *interceptToggle) paint() {
	p := qt.NewQPainter2(t.w.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)

	w, h := float64(t.w.Width()), float64(t.w.Height())
	bg := color(colorServerError)
	if t.on {
		bg = color(colorSuccess)
	}
	if t.hovered {
		bg = bg.LighterWithInt(115)
	}
	p.SetPenWithStyle(qt.NoPen)
	p.SetBrush(qt.NewQBrush3(bg))
	p.DrawRoundedRect(qt.NewQRectF4(0, 0, w, h), 6, 6)

	size := h * 0.72
	drawIcon(p, iconIntercept, (w-size)/2, (h-size)/2, size, qt.NewQColor6("#ffffff"))
}
