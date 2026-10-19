package ui

import (
	"strconv"

	qt "github.com/mappu/miqt/qt6"
)

// gauge is a small radial dial for a 0–100 score on the finding detail pane: a
// track arc with a coloured value arc over it and the number in the middle,
// with a caption beneath. An unset score (nil) shows an empty track and "—".
// The colour of the value arc and the number comes from colorFn, so confidence
// and concern read in their own band colours and the whole thing follows the
// active theme on a repaint.
type gauge struct {
	root    *qt.QWidget
	dial    *qt.QWidget
	value   *int
	colorFn func(int) string // value → hex for the arc and number
}

const gaugeSize = 78

func newGauge(caption string, colorFn func(int) string) *gauge {
	g := &gauge{colorFn: colorFn}
	g.root = qt.NewQWidget2()
	v := qt.NewQVBoxLayout(g.root)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(2)

	g.dial = qt.NewQWidget2()
	g.dial.SetFixedSize2(gaugeSize, gaugeSize)
	g.dial.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { g.paint() })
	v.AddWidget3(g.dial, 0, qt.AlignHCenter)

	capLbl := qt.NewQLabel3(caption)
	setName(capLbl.QObject, "Muted")
	capLbl.SetAlignment(qt.AlignHCenter)
	v.AddWidget(capLbl.QWidget)
	return g
}

// set updates the score shown and repaints.
func (g *gauge) set(v *int) {
	g.value = v
	g.dial.Update()
}

func (g *gauge) paint() {
	p := qt.NewQPainter2(g.dial.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)
	p.SetRenderHint(qt.QPainter__TextAntialiasing)

	const stroke = 8.0
	w, h := float64(g.dial.Width()), float64(g.dial.Height())
	m := stroke/2 + 2
	rect := qt.NewQRectF4(m, m, w-2*m, h-2*m)

	// Angles are in sixteenths of a degree, counter-clockwise from 3 o'clock.
	// Start at the upper left and sweep 270° clockwise, leaving a gap at the
	// bottom, so the dial reads like a speedometer.
	const start = 225 * 16
	const full = -270 * 16

	track := qt.NewQPen3(color("divider"))
	track.SetWidthF(stroke)
	track.SetCapStyle(qt.RoundCap)
	p.SetPenWithPen(track)
	p.DrawArc(rect, start, full)

	num := "—"
	ink := color(colorMuted)
	if g.value != nil {
		v := clampGauge(*g.value)
		arc := qt.NewQPen3(qt.NewQColor6(g.colorFn(v)))
		arc.SetWidthF(stroke)
		arc.SetCapStyle(qt.RoundCap)
		p.SetPenWithPen(arc)
		p.DrawArc(rect, start, int(float64(full)*float64(v)/100.0))
		num = strconv.Itoa(v)
		ink = qt.NewQColor6(g.colorFn(v))
	}

	f := p.Font()
	f.SetPointSizeF(16)
	f.SetBold(true)
	p.SetFont(f)
	p.SetPen(ink)
	p.DrawText5(rect, int(qt.AlignCenter), num)
}

func clampGauge(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}
