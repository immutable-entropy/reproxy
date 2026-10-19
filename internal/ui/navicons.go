package ui

import (
	"math"

	qt "github.com/mappu/miqt/qt6"
)

// Icons for the navigation bar, painted rather than loaded from files: they
// follow the active theme's colours (see theme.go) and stay crisp at any
// scale factor, with no assets to ship.
//
// Each icon is drawn on a square iconGrid×iconGrid grid, stroked with
// iconStroke-wide round-capped lines, so the set looks consistent.
type iconKind int

const (
	iconTargets iconKind = iota
	iconIntercept
	iconTraffic
	iconReplay
	iconMap
	iconAnalyze
	iconFindings
	iconFiles
	iconSettings
	iconEdit
	iconTune
	iconPlus
	iconClose
	iconUp
	iconDown
	iconSearch
	iconList
	iconMinus
	iconFit
	iconTrash
	iconGlobe
	iconPlugins
	iconEye
	iconEyeOff
	iconCrawler
	iconFilter
	iconKebab
	iconRowsTight
	iconRowsLoose
	iconImport
	// The Sessions panel's dock-position glyphs, in the style of a browser's
	// dev-tools dock menu: a window frame with the panel shaded on one edge,
	// and a pop-out window for floating it free.
	iconDockBottom
	iconDockTop
	iconDockLeft
	iconDockRight
	iconDockWindow
)

const (
	iconGrid   = 24.0
	iconStroke = 1.7
)

// drawIcon paints kind in col, filling a size×size box whose top-left corner
// is at (x, y). The painter must already have antialiasing on.
func drawIcon(p *qt.QPainter, kind iconKind, x, y, size float64, col *qt.QColor) {
	p.Save()
	defer p.Restore()
	p.Translate2(x, y)
	p.Scale(size/iconGrid, size/iconGrid)

	pen := qt.NewQPen3(col)
	pen.SetWidthF(iconStroke)
	pen.SetCapStyle(qt.RoundCap)
	pen.SetJoinStyle(qt.RoundJoin)
	p.SetPenWithPen(pen)
	p.SetBrushWithStyle(qt.NoBrush)

	switch kind {
	case iconTargets:
		drawCrosshair(p)
	case iconIntercept:
		drawIntercept(p, col)
	case iconTraffic:
		drawTraffic(p, col)
	case iconReplay:
		drawReplay(p, col)
	case iconMap:
		drawSitemap(p)
	case iconAnalyze:
		drawChart(p)
	case iconFindings:
		drawFlag(p)
	case iconFiles:
		drawFiles(p)
	case iconSettings:
		drawCog(p)
	case iconEdit:
		drawPencil(p)
	case iconTune:
		drawSliders(p)
	case iconPlus:
		drawPlus(p)
	case iconClose:
		drawCross(p)
	case iconUp:
		drawChevron(p, true)
	case iconDown:
		drawChevron(p, false)
	case iconSearch:
		drawSearch(p)
	case iconFilter:
		drawFunnel(p)
	case iconList:
		drawList(p)
	case iconMinus:
		p.DrawLine4(qt.NewQPointF3(6, 12), qt.NewQPointF3(18, 12))
	case iconFit:
		drawFit(p)
	case iconTrash:
		drawTrash(p)
	case iconImport:
		drawImport(p)
	case iconGlobe:
		drawGlobe(p)
	case iconPlugins:
		drawPuzzle(p)
	case iconEye:
		drawEye(p, false)
	case iconEyeOff:
		drawEye(p, true)
	case iconCrawler:
		drawCrawler(p, col)
	case iconKebab:
		drawKebab(p, col)
	case iconRowsTight:
		drawRows(p, true)
	case iconRowsLoose:
		drawRows(p, false)
	case iconDockBottom:
		drawDock(p, col, dockBottom)
	case iconDockTop:
		drawDock(p, col, dockTop)
	case iconDockLeft:
		drawDock(p, col, dockLeft)
	case iconDockRight:
		drawDock(p, col, dockRight)
	case iconDockWindow:
		drawDockWindow(p)
	}
}

// dockEdge names which edge of the window frame the panel is shaded on, for
// drawDock.
type dockEdge int

const (
	dockBottom dockEdge = iota
	dockTop
	dockLeft
	dockRight
)

// drawDock paints a browser-style dock glyph: an outlined window frame with a
// solid band on one edge standing for the docked panel. This mirrors the
// dock-side picker in Chrome's dev tools, where the filled region shows where
// the panel would sit.
func drawDock(p *qt.QPainter, col *qt.QColor, edge dockEdge) {
	p.Save()
	defer p.Restore()
	// The window frame, stroked with the pen drawIcon set up.
	p.DrawRoundedRect(qt.NewQRectF4(4, 5, 16, 14), 2.5, 2.5)
	// The docked panel: a solid band just inside one edge.
	var band *qt.QRectF
	switch edge {
	case dockBottom:
		band = qt.NewQRectF4(6, 13, 12, 4)
	case dockTop:
		band = qt.NewQRectF4(6, 7, 12, 4)
	case dockLeft:
		band = qt.NewQRectF4(6, 7, 4, 10)
	case dockRight:
		band = qt.NewQRectF4(14, 7, 4, 10)
	}
	p.SetPenWithStyle(qt.NoPen)
	p.SetBrush(qt.NewQBrush3(col))
	p.DrawRoundedRect(band, 1, 1)
}

// drawDockWindow paints the "float in its own window" glyph: a window frame
// with an arrow lifting out of its top-right corner, the familiar pop-out
// mark, matching the undock option in a browser's dev tools.
func drawDockWindow(p *qt.QPainter) {
	// The window, sitting toward the lower left.
	p.DrawRoundedRect(qt.NewQRectF4(4, 10, 10, 10), 2.5, 2.5)
	// The pop-out arrow, out of the top-right corner.
	p.DrawLine4(qt.NewQPointF3(12.5, 11.5), qt.NewQPointF3(19.5, 4.5))
	p.DrawLine4(qt.NewQPointF3(14.5, 4.5), qt.NewQPointF3(19.5, 4.5))
	p.DrawLine4(qt.NewQPointF3(19.5, 4.5), qt.NewQPointF3(19.5, 9.5))
}

// paintedIcon renders an icon kind into a QIcon for the few places that need
// one — a menu action, a list row — rather than a self-painting button. col
// is the ink and size the pixmap's side in device-independent pixels.
func paintedIcon(kind iconKind, col *qt.QColor, size int) *qt.QIcon {
	pix := qt.NewQPixmap2(size, size)
	pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
	pt := qt.NewQPainter2(pix.QPaintDevice)
	pt.SetRenderHint(qt.QPainter__Antialiasing)
	drawIcon(pt, kind, 0, 0, float64(size), col)
	pt.End()
	return qt.NewQIcon2(pix)
}

// drawKebab is the three vertical dots of a "more" button: the Sessions
// panel's own menu, in its top right corner.
func drawKebab(p *qt.QPainter, col *qt.QColor) {
	p.Save()
	defer p.Restore()
	p.SetPenWithStyle(qt.NoPen)
	p.SetBrush(qt.NewQBrush3(col))
	for _, y := range []float64{6.6, 12, 17.4} {
		p.DrawEllipse3(qt.NewQPointF3(12, y), 1.5, 1.5)
	}
}

// drawRows is the request list's density button: lines packed tight for
// compact rows, spaced out for comfortable ones. The button shows the
// density it would switch to, so the two are only ever seen one at a time.
func drawRows(p *qt.QPainter, tight bool) {
	ys := []float64{6, 12, 18}
	if tight {
		ys = []float64{5, 8.5, 12, 15.5, 19}
	}
	for _, y := range ys {
		p.DrawLine4(qt.NewQPointF3(4.5, y), qt.NewQPointF3(19.5, y))
	}
}

// drawCrawler is a web crawler: a bug in a browser window, for Autocrawl.
// The window's frame and title bar, and under them a bug with a filled
// head, an oval body and three legs a side.
func drawCrawler(p *qt.QPainter, col *qt.QColor) {
	p.DrawRoundedRect(qt.NewQRectF4(2.5, 3.5, 19, 17), 2.5, 2.5)
	p.DrawLine4(qt.NewQPointF3(2.5, 7.8), qt.NewQPointF3(21.5, 7.8))
	p.DrawEllipse3(qt.NewQPointF3(12, 15.4), 2.4, 3.1)
	p.Save()
	p.SetBrush(qt.NewQBrush3(col))
	p.DrawEllipse3(qt.NewQPointF3(12, 11), 1.1, 1.1)
	p.Restore()
	for _, leg := range [][4]float64{
		{9.8, 13.8, 7, 12.4},
		{9.6, 15.6, 6.4, 15.6},
		{9.8, 17.4, 7, 18.8},
	} {
		p.DrawLine4(qt.NewQPointF3(leg[0], leg[1]), qt.NewQPointF3(leg[2], leg[3]))
		p.DrawLine4(qt.NewQPointF3(24-leg[0], leg[1]), qt.NewQPointF3(24-leg[2], leg[3]))
	}
}

// drawEye is the reveal toggle beside a hidden value: an open eye to show
// it, the same eye struck through once it is showing.
func drawEye(p *qt.QPainter, struck bool) {
	lid := qt.NewQPainterPath()
	lid.MoveTo2(3, 12)
	lid.QuadTo2(12, 4.5, 21, 12)
	lid.QuadTo2(12, 19.5, 3, 12)
	p.DrawPath(lid)
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 2.6, 2.6)
	if struck {
		p.DrawLine4(qt.NewQPointF3(4.5, 19.5), qt.NewQPointF3(19.5, 4.5))
	}
}

// drawPuzzle is a puzzle piece: plugins, the pieces added to the app. A
// square with a knob on its top and right edges.
func drawPuzzle(p *qt.QPainter) {
	piece := qt.NewQPainterPath()
	piece.MoveTo2(4, 8)
	piece.LineTo2(8.2, 8)
	piece.ArcTo2(7, 2.6, 6, 6, 233, -286)
	piece.LineTo2(16, 8)
	piece.LineTo2(16, 11.6)
	piece.ArcTo2(15, 10.6, 6.8, 6.8, 135, -270)
	piece.LineTo2(16, 20)
	piece.LineTo2(4, 20)
	piece.CloseSubpath()
	p.DrawPath(piece)
}

// drawGlobe is a globe: opening a browser onto the web. A circle crossed by
// an equator, two latitudes and a meridian.
func drawGlobe(p *qt.QPainter) {
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 8.5, 8.5)
	// Equator, with a shorter latitude line above and below it.
	p.DrawLine4(qt.NewQPointF3(3.5, 12), qt.NewQPointF3(20.5, 12))
	p.DrawLine4(qt.NewQPointF3(5.6, 7.5), qt.NewQPointF3(18.4, 7.5))
	p.DrawLine4(qt.NewQPointF3(5.6, 16.5), qt.NewQPointF3(18.4, 16.5))
	// A meridian: a thin ellipse narrowing toward the poles.
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 3.6, 8.5)
}

// drawTrash is a bin: clearing a whole view (the Explorer).
// drawImport is an arrow coming down into a tray: reading a file in.
func drawImport(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(12, 3.5), qt.NewQPointF3(12, 13.5))
	head := qt.NewQPainterPath()
	head.MoveTo2(8.5, 10)
	head.LineTo2(12, 13.5)
	head.LineTo2(15.5, 10)
	p.DrawPath(head)
	tray := qt.NewQPainterPath()
	tray.MoveTo2(5, 15.5)
	tray.LineTo2(5, 20)
	tray.LineTo2(19, 20)
	tray.LineTo2(19, 15.5)
	p.DrawPath(tray)
}

func drawTrash(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(5, 7), qt.NewQPointF3(19, 7))
	p.DrawLine4(qt.NewQPointF3(10, 7), qt.NewQPointF3(10, 4.5))
	p.DrawLine4(qt.NewQPointF3(10, 4.5), qt.NewQPointF3(14, 4.5))
	p.DrawLine4(qt.NewQPointF3(14, 4.5), qt.NewQPointF3(14, 7))
	p.DrawLine4(qt.NewQPointF3(7, 7), qt.NewQPointF3(8, 19.5))
	p.DrawLine4(qt.NewQPointF3(8, 19.5), qt.NewQPointF3(16, 19.5))
	p.DrawLine4(qt.NewQPointF3(16, 19.5), qt.NewQPointF3(17, 7))
	p.DrawLine4(qt.NewQPointF3(10.5, 10.5), qt.NewQPointF3(10.5, 16))
	p.DrawLine4(qt.NewQPointF3(13.5, 10.5), qt.NewQPointF3(13.5, 16))
}

// drawSearch is a magnifying glass: the request-list filter.
// drawList is an indented list: the Explorer's tree view.
func drawList(p *qt.QPainter) {
	for _, row := range [][2]float64{{4, 6}, {8, 12}, {8, 18}} {
		p.DrawPoint(qt.NewQPointF3(row[0], row[1]))
		p.DrawLine4(qt.NewQPointF3(row[0]+4, row[1]), qt.NewQPointF3(20, row[1]))
	}
}

// drawFit is four corners: fit the content to the view.
func drawFit(p *qt.QPainter) {
	for _, c := range [][4]float64{{4, 4, 1, 1}, {20, 4, -1, 1}, {4, 20, 1, -1}, {20, 20, -1, -1}} {
		x, y, dx, dy := c[0], c[1], c[2], c[3]
		p.DrawLine4(qt.NewQPointF3(x, y), qt.NewQPointF3(x+5*dx, y))
		p.DrawLine4(qt.NewQPointF3(x, y), qt.NewQPointF3(x, y+5*dy))
	}
}

func drawSearch(p *qt.QPainter) {
	p.DrawEllipse3(qt.NewQPointF3(10.5, 10.5), 5.5, 5.5)
	p.DrawLine4(qt.NewQPointF3(14.6, 14.6), qt.NewQPointF3(19.5, 19.5))
}

// drawFunnel is a filter: a list poured through a funnel, keeping some of it.
func drawFunnel(p *qt.QPainter) {
	path := qt.NewQPainterPath()
	path.MoveTo2(3.5, 4.5)
	path.LineTo2(20.5, 4.5)
	path.LineTo2(13.8, 12.5)
	path.LineTo2(13.8, 19.5)
	path.LineTo2(10.2, 17.2)
	path.LineTo2(10.2, 12.5)
	path.CloseSubpath()
	p.DrawPath(path)
}

// drawCrosshair is Targets: the hosts in scope, sighted in.
func drawCrosshair(p *qt.QPainter) {
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 6.5, 6.5)
	for _, tick := range [][4]float64{
		{12, 2.5, 12, 5}, {12, 19, 12, 21.5},
		{2.5, 12, 5, 12}, {19, 12, 21.5, 12},
	} {
		p.DrawLine4(qt.NewQPointF3(tick[0], tick[1]), qt.NewQPointF3(tick[2], tick[3]))
	}
	p.DrawPoint(qt.NewQPointF3(12, 12))
}

// drawIntercept is a request arriving at a barrier: traffic held, not passed.
func drawIntercept(p *qt.QPainter, col *qt.QColor) {
	p.DrawLine4(qt.NewQPointF3(18.5, 4), qt.NewQPointF3(18.5, 20))
	p.DrawLine4(qt.NewQPointF3(3, 12), qt.NewQPointF3(9.5, 12))
	fillArrowHead(p, 15, 12, 1, 0, 6, col)
}

// drawTraffic is Traffic: requests going out and responses coming back,
// as two arrows passing in opposite directions.
func drawTraffic(p *qt.QPainter, col *qt.QColor) {
	p.DrawLine4(qt.NewQPointF3(3.5, 8), qt.NewQPointF3(15, 8))
	fillArrowHead(p, 21, 8, 1, 0, 6.5, col)
	p.DrawLine4(qt.NewQPointF3(20.5, 16), qt.NewQPointF3(9, 16))
	fillArrowHead(p, 3, 16, -1, 0, 6.5, col)
}

// drawReplay is a circular arrow: send the same request again.
func drawReplay(p *qt.QPainter, col *qt.QColor) {
	// A ring with a gap at the top, where the arrow head sits.
	const r = 8.0
	p.DrawArc3(int(12-r), int(12-r), int(2*r), int(2*r), 105*16, 310*16)
	// End of the arc, with the head along the tangent there.
	a := (105 + 310) * math.Pi / 180
	fillArrowHead(p, 12+r*math.Cos(a), 12-r*math.Sin(a), -math.Sin(a), -math.Cos(a), 7, col)
}

// drawChart is Analyze: bars on a baseline.
// drawSitemap is the Explorer: a page branching into the pages below it.
func drawSitemap(p *qt.QPainter) {
	p.DrawRoundedRect(qt.NewQRectF4(8.5, 3.5, 7, 5), 1.5, 1.5)
	p.DrawLine4(qt.NewQPointF3(12, 8.5), qt.NewQPointF3(12, 15.5))
	p.DrawLine4(qt.NewQPointF3(5.5, 12), qt.NewQPointF3(18.5, 12))
	p.DrawLine4(qt.NewQPointF3(5.5, 12), qt.NewQPointF3(5.5, 15.5))
	p.DrawLine4(qt.NewQPointF3(18.5, 12), qt.NewQPointF3(18.5, 15.5))
	for _, x := range []float64{2.5, 9, 15.5} {
		p.DrawRoundedRect(qt.NewQRectF4(x, 15.5, 6, 5), 1.5, 1.5)
	}
}

func drawChart(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(3.5, 20), qt.NewQPointF3(20.5, 20))
	for _, bar := range [][2]float64{{7, 14}, {12, 6.5}, {17, 10.5}} {
		p.DrawLine4(qt.NewQPointF3(bar[0], 18.5), qt.NewQPointF3(bar[0], bar[1]))
	}
}

// drawFlag is Findings: something worth flagging was found.
func drawFlag(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(6, 3.5), qt.NewQPointF3(6, 20.5))
	banner := qt.NewQPainterPath()
	banner.MoveTo2(6, 5)
	banner.LineTo2(19, 5)
	banner.LineTo2(16, 9.5)
	banner.LineTo2(19, 14)
	banner.LineTo2(6, 14)
	p.DrawPath(banner)
}

// drawCog is Settings.
func drawCog(p *qt.QPainter) {
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 6.3, 6.3)
	p.DrawEllipse3(qt.NewQPointF3(12, 12), 2.5, 2.5)
	for i := 0; i < 8; i++ {
		a := float64(i) * math.Pi / 4
		cos, sin := math.Cos(a), math.Sin(a)
		p.DrawLine4(qt.NewQPointF3(12+6.3*cos, 12+6.3*sin), qt.NewQPointF3(12+9*cos, 12+9*sin))
	}
}

// drawPencil is Edit.
func drawPencil(p *qt.QPainter) {
	nib := qt.NewQPainterPath()
	nib.MoveTo2(4, 20)
	nib.LineTo2(4.8, 16.2)
	nib.LineTo2(16, 5)
	nib.LineTo2(19, 8)
	nib.LineTo2(7.8, 19.2)
	nib.CloseSubpath()
	p.DrawPath(nib)
	p.DrawLine4(qt.NewQPointF3(14, 7), qt.NewQPointF3(17, 10))
}

// drawSliders is the per-target settings button: two labelled controls,
// which reads as "settings for this one" without repeating the nav's cog.
func drawSliders(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(4, 8.5), qt.NewQPointF3(20, 8.5))
	p.DrawLine4(qt.NewQPointF3(4, 15.5), qt.NewQPointF3(20, 15.5))
	p.DrawEllipse3(qt.NewQPointF3(9, 8.5), 2.4, 2.4)
	p.DrawEllipse3(qt.NewQPointF3(15, 15.5), 2.4, 2.4)
}

func drawPlus(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(12, 5), qt.NewQPointF3(12, 19))
	p.DrawLine4(qt.NewQPointF3(5, 12), qt.NewQPointF3(19, 12))
}

func drawCross(p *qt.QPainter) {
	p.DrawLine4(qt.NewQPointF3(6, 6), qt.NewQPointF3(18, 18))
	p.DrawLine4(qt.NewQPointF3(18, 6), qt.NewQPointF3(6, 18))
}

// drawChevron points up or down, for reordering a list.
func drawChevron(p *qt.QPainter, up bool) {
	y1, y2 := 9.0, 15.0
	if !up {
		y1, y2 = y2, y1
	}
	v := qt.NewQPainterPath()
	v.MoveTo2(6, y2)
	v.LineTo2(12, y1)
	v.LineTo2(18, y2)
	p.DrawPath(v)
}

// fillArrowHead fills a triangle pointing at (tipX, tipY) along the direction
// (dx, dy), size long and 0.84×size wide.
func fillArrowHead(p *qt.QPainter, tipX, tipY, dx, dy, size float64, col *qt.QColor) {
	n := math.Hypot(dx, dy)
	if n == 0 {
		return
	}
	dx, dy = dx/n, dy/n
	baseX, baseY := tipX-dx*size, tipY-dy*size
	halfW := size * 0.42
	head := qt.NewQPainterPath()
	head.MoveTo2(tipX, tipY)
	head.LineTo2(baseX-dy*halfW, baseY+dx*halfW)
	head.LineTo2(baseX+dy*halfW, baseY-dx*halfW)
	head.CloseSubpath()
	p.FillPath(head, qt.NewQBrush3(col))
}

// drawFiles is the Files section: two stacked sheets of paper, the front
// one with a folded corner — the files the work recovered, which are
// documents on disk rather than anything about the traffic itself.
func drawFiles(p *qt.QPainter) {
	// The sheet behind, offset up and to the right.
	back := qt.NewQPainterPath()
	back.MoveTo2(8.5, 4.5)
	back.LineTo2(16.2, 4.5)
	back.LineTo2(20, 8.3)
	back.LineTo2(20, 15.5)
	p.DrawPath(back)

	// The sheet in front, with its corner folded over.
	front := qt.NewQPainterPath()
	front.MoveTo2(12.2, 8)
	front.LineTo2(6, 8)
	front.LineTo2(6, 19.5)
	front.LineTo2(16, 19.5)
	front.LineTo2(16, 11.8)
	front.CloseSubpath()
	p.DrawPath(front)
	fold := qt.NewQPainterPath()
	fold.MoveTo2(12.2, 8)
	fold.LineTo2(12.2, 11.8)
	fold.LineTo2(16, 11.8)
	p.DrawPath(fold)
}
