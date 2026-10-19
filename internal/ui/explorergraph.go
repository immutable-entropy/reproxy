package ui

import (
	"fmt"
	"math"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/entities"
	"reproxy/internal/sitemap"
)

// explorerGraph is the Explorer's graphical view: the same tree, drawn as
// boxes joined by lines and growing left to right — a domain at the left
// edge, its hosts beside it, their paths beside those — on a canvas the
// user drags to move and scrolls to zoom (also Ctrl+scroll, the corner
// controls and +/-/0; Shift+scroll moves sideways).
//
// Left to right because a node's name is a line of text: boxes are wider
// than they are tall, so siblings stack far more tightly down the page
// than they ever would across it, and a deep path stays readable as it
// marches to the right.
//
// It shares the pane's state rather than keeping its own: which nodes are
// open (so collapsing in one view collapses in the other), the selected
// node, and the sidebar a click opens.
//
// Each box carries its type's colour, and an entity only discovered is
// drawn faded, as in the tree.
//
// It paints itself, like the navigation bar: only the nodes in view are
// drawn, and below a legible zoom the boxes are drawn without text, so a
// large tree stays quick to move around.
type explorerGraph struct {
	w    *qt.QWidget
	pane *explorerPane
	m    *sitemap.Map

	nodes []*gnode
	byKey map[string]*gnode
	// width and height of the laid-out tree, in scene units.
	sceneW, sceneH float64

	// The view: screen = scene*zoom + (offX, offY).
	zoom       float64
	offX, offY float64
	fitted     bool // the view has been fitted once; after that it is the user's

	pressed   bool
	dragging  bool
	pressX    float64
	pressY    float64
	lastX     float64
	lastY     float64
	hovered   string // node key under the pointer
	hoverOnly bool   // the pointer is on the hovered node's expand handle

	zoomLabel *qt.QLabel
	controls  *qt.QWidget
	hint      *qt.QLabel
}

// gnode is one node as laid out: its box's top-left corner in scene units.
type gnode struct {
	n    *sitemap.Node
	x, y float64
	kids []*gnode
	open bool
}

// Scene metrics. The tree grows to the right, so a generation is a column:
// the depth gap is horizontal and siblings stack down the page.
const (
	gNodeW   = 160.0
	gNodeH   = 50.0
	gGapY    = 14.0 // between sibling boxes, down the page
	gGapX    = 64.0 // between a parent's column and its children's
	gRootGap = 34.0 // between roots
	gMargin  = 40.0 // around the tree when fitting it
	gHandleR = 8.0  // expand/collapse handle radius
	gMinZoom = 0.12
	gMaxZoom = 2.5
	// gTextZoom is the smallest zoom at which labels are drawn.
	gTextZoom = 0.38
)

func newExplorerGraph(pane *explorerPane) *explorerGraph {
	g := &explorerGraph{w: qt.NewQWidget2(), pane: pane, byKey: map[string]*gnode{}, zoom: 1}
	setName(g.w.QObject, "ExplorerCanvas")
	g.w.SetAttribute(qt.WA_StyledBackground)
	g.w.SetMouseTracking(true)
	g.w.SetFocusPolicy(qt.StrongFocus)
	g.w.SetCursor(qt.NewQCursor2(qt.OpenHandCursor))

	g.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { g.paint() })
	g.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { g.press(e) })
	g.w.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { g.move(e) })
	g.w.OnMouseReleaseEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { g.release(e) })
	g.w.OnMouseDoubleClickEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		pos := e.Position()
		if n, _ := g.hit(pos.X(), pos.Y()); n != nil && len(n.n.Children) > 0 {
			g.toggle(n.n.Key)
		}
	})
	g.w.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) {
		g.hovered, g.hoverOnly = "", false
		g.w.Update()
	})
	g.w.OnWheelEvent(func(_ func(*qt.QWheelEvent), e *qt.QWheelEvent) { g.wheel(e) })
	// The same menu as the tree's rows, so Expand All and the rest are
	// where a right-click puts them in either view.
	g.w.SetContextMenuPolicy(qt.CustomContextMenu)
	g.w.OnCustomContextMenuRequested(func(pos *qt.QPoint) {
		if n, _ := g.hit(float64(pos.X()), float64(pos.Y())); n != nil {
			g.pane.showMenu(n.n.Key, g.w.MapToGlobalWithQPoint(pos))
		}
	})
	g.w.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if !g.key(e) {
			super(e)
		}
	})
	g.w.OnResizeEvent(func(_ func(*qt.QResizeEvent), _ *qt.QResizeEvent) { g.placeOverlays() })
	g.buildOverlays()
	return g
}

// buildOverlays adds the zoom controls (bottom right) and a usage hint
// (bottom left) floating over the canvas.
func (g *explorerGraph) buildOverlays() {
	g.controls = qt.NewQWidget(g.w)
	setName(g.controls.QObject, "ExplorerControls")
	g.controls.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(g.controls)
	h.SetContentsMargins(4, 2, 4, 2)
	h.SetSpacing(0)
	g.zoomLabel = qt.NewQLabel2()
	setName(g.zoomLabel.QObject, "Muted")
	g.zoomLabel.SetAlignment(qt.AlignCenter)
	g.zoomLabel.SetMinimumWidth(44)
	h.AddWidget(newIconButton(iconMinus, "Zoom out (-)", func() { g.zoomBy(1 / 1.25) }).w)
	h.AddWidget(g.zoomLabel.QWidget)
	h.AddWidget(newIconButton(iconPlus, "Zoom in (+)", func() { g.zoomBy(1.25) }).w)
	h.AddWidget(newIconButton(iconFit, "Fit the Explorer to the view (0)", g.fit).w)

	g.hint = qt.NewQLabel3("Drag to move · Scroll to zoom · Click a node for details, its handle to open it · Right-click for more")
	g.hint.SetParent(g.w)
	setName(g.hint.QObject, "ExplorerHint")
	g.hint.SetAttribute(qt.WA_TransparentForMouseEvents)
	g.updateZoomLabel()
}

func (g *explorerGraph) placeOverlays() {
	const margin = 12
	g.controls.AdjustSize()
	cs := g.controls.SizeHint()
	g.controls.Move(g.w.Width()-cs.Width()-margin, g.w.Height()-cs.Height()-margin)
	g.hint.AdjustSize()
	hs := g.hint.SizeHint()
	g.hint.Move(margin, g.w.Height()-hs.Height()-margin)
	// Hide the hint rather than let it run under the controls.
	g.hint.SetVisible(margin+hs.Width()+margin < g.w.Width()-cs.Width()-margin)
}

// setMap shows a newly built tree, keeping the view where it was.
func (g *explorerGraph) setMap(m *sitemap.Map) {
	g.m = m
	g.relayout("")
}

// relayout lays the tree out again after a change in what is open. When
// anchor names a node, the view moves so that node stays where it was on
// screen (opening a node recentres it over its children).
func (g *explorerGraph) relayout(anchor string) {
	var ax, ay float64
	before, hadAnchor := g.byKey[anchor]
	if hadAnchor {
		ax, ay = g.toScreen(before.x, before.y)
	}

	g.nodes = g.nodes[:0]
	g.byKey = make(map[string]*gnode)
	g.sceneW, g.sceneH = 0, 0
	if g.m != nil {
		cursor := 0.0
		for i, r := range g.m.Roots {
			if i > 0 {
				cursor += gRootGap - gGapY
			}
			g.place(r, 0, &cursor)
		}
		g.sceneH = math.Max(0, cursor-gGapY)
	}

	if after, ok := g.byKey[anchor]; ok && hadAnchor {
		sx, sy := g.toScreen(after.x, after.y)
		g.offX += ax - sx
		g.offY += ay - sy
	}
	g.w.Update()
}

// place lays out n's subtree with its topmost box at *cursor, which it
// advances past the subtree. Depth decides the column; a parent sits
// centred beside its open children, and boxes are all one height, so a
// parent never sticks out past them.
func (g *explorerGraph) place(n *sitemap.Node, depth int, cursor *float64) *gnode {
	gn := &gnode{n: n, x: float64(depth) * (gNodeW + gGapX), open: g.pane.isExpanded(n) && len(n.Children) > 0}
	g.nodes = append(g.nodes, gn)
	g.byKey[n.Key] = gn
	g.sceneW = math.Max(g.sceneW, gn.x+gNodeW+gHandleR)

	if !gn.open {
		gn.y = *cursor
		*cursor += gNodeH + gGapY
		return gn
	}
	for _, c := range n.Children {
		gn.kids = append(gn.kids, g.place(c, depth+1, cursor))
	}
	first, last := gn.kids[0], gn.kids[len(gn.kids)-1]
	gn.y = (first.y + last.y) / 2
	return gn
}

// ---- View ----------------------------------------------------------------

func (g *explorerGraph) toScreen(x, y float64) (float64, float64) {
	return x*g.zoom + g.offX, y*g.zoom + g.offY
}

func (g *explorerGraph) toScene(x, y float64) (float64, float64) {
	return (x - g.offX) / g.zoom, (y - g.offY) / g.zoom
}

// fit zooms and centres so the whole tree is in view, never past 100%.
func (g *explorerGraph) fit() {
	g.fitted = true
	w, h := float64(g.w.Width()), float64(g.w.Height())
	if g.sceneW == 0 || w == 0 || h == 0 {
		return
	}
	z := math.Min(w/(g.sceneW+2*gMargin), h/(g.sceneH+2*gMargin))
	g.zoom = clamp(z, gMinZoom, 1)
	g.offX = math.Max(gMargin*g.zoom, (w-g.sceneW*g.zoom)/2)
	g.offY = (h - g.sceneH*g.zoom) / 2
	g.updateZoomLabel()
	g.w.Update()
}

// reveal fits the tree the first time it is shown, and otherwise brings the
// selected node into view if it is off screen.
func (g *explorerGraph) reveal() {
	if !g.fitted {
		g.fit()
		return
	}
	n, ok := g.byKey[g.pane.selected]
	if !ok {
		return
	}
	sx, sy := g.toScreen(n.x, n.y)
	w, h := float64(g.w.Width()), float64(g.w.Height())
	if sx < 0 || sy < 0 || sx+gNodeW*g.zoom > w || sy+gNodeH*g.zoom > h {
		g.offX += w/2 - (sx + gNodeW*g.zoom/2)
		g.offY += h/2 - (sy + gNodeH*g.zoom/2)
		g.w.Update()
	}
}

// zoomAt zooms by factor keeping the scene point under (sx, sy) in place.
func (g *explorerGraph) zoomAt(factor, sx, sy float64) {
	z := clamp(g.zoom*factor, gMinZoom, gMaxZoom)
	if z == g.zoom {
		return
	}
	x, y := g.toScene(sx, sy)
	g.zoom = z
	g.offX = sx - x*z
	g.offY = sy - y*z
	g.updateZoomLabel()
	g.w.Update()
}

// zoomBy zooms about the centre of the view.
func (g *explorerGraph) zoomBy(factor float64) {
	g.zoomAt(factor, float64(g.w.Width())/2, float64(g.w.Height())/2)
}

func (g *explorerGraph) pan(dx, dy float64) {
	g.offX += dx
	g.offY += dy
	g.w.Update()
}

func (g *explorerGraph) updateZoomLabel() {
	g.zoomLabel.SetText(fmt.Sprintf("%d%%", int(math.Round(g.zoom*100))))
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// ---- Input ---------------------------------------------------------------

// hit returns the node at screen point (x, y), and whether the point is on
// its expand/collapse handle.
func (g *explorerGraph) hit(x, y float64) (*gnode, bool) {
	sx, sy := g.toScene(x, y)
	for _, n := range g.nodes {
		if len(n.n.Children) > 0 {
			dx, dy := sx-(n.x+gNodeW), sy-(n.y+gNodeH/2)
			if dx*dx+dy*dy <= (gHandleR+3)*(gHandleR+3) {
				return n, true
			}
		}
		if sx >= n.x && sx <= n.x+gNodeW && sy >= n.y && sy <= n.y+gNodeH {
			return n, false
		}
	}
	return nil, false
}

func (g *explorerGraph) press(e *qt.QMouseEvent) {
	if e.Button() != qt.LeftButton {
		return
	}
	pos := e.Position()
	g.pressed, g.dragging = true, false
	g.pressX, g.pressY = pos.X(), pos.Y()
	g.lastX, g.lastY = g.pressX, g.pressY
}

func (g *explorerGraph) move(e *qt.QMouseEvent) {
	x, y := e.Position().X(), e.Position().Y()
	if g.pressed {
		if !g.dragging && math.Abs(x-g.pressX)+math.Abs(y-g.pressY) > 4 {
			g.dragging = true
			g.w.SetCursor(qt.NewQCursor2(qt.ClosedHandCursor))
		}
		if g.dragging {
			g.pan(x-g.lastX, y-g.lastY)
			g.lastX, g.lastY = x, y
		}
		return
	}

	n, onHandle := g.hit(x, y)
	key := ""
	if n != nil {
		key = n.n.Key
	}
	if key == g.hovered && onHandle == g.hoverOnly {
		return
	}
	g.hovered, g.hoverOnly = key, onHandle
	switch {
	case n == nil:
		g.w.SetCursor(qt.NewQCursor2(qt.OpenHandCursor))
		g.w.SetToolTip("")
	case onHandle:
		g.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
		verb := "Open"
		if n.open {
			verb = "Close"
		}
		g.w.SetToolTip(fmt.Sprintf("%s (%s)", verb, countOf(len(n.n.Children), "item", "items")))
	default:
		g.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
		g.w.SetToolTip(nodeTip(n.n))
	}
	g.w.Update()
}

func (g *explorerGraph) release(e *qt.QMouseEvent) {
	if e.Button() != qt.LeftButton || !g.pressed {
		return
	}
	wasDrag := g.dragging
	g.pressed, g.dragging = false, false
	if wasDrag {
		g.w.SetCursor(qt.NewQCursor2(qt.OpenHandCursor))
		return
	}
	pos := e.Position()
	n, onHandle := g.hit(pos.X(), pos.Y())
	switch {
	case n == nil:
	case onHandle:
		g.toggle(n.n.Key)
	default:
		g.pane.selectNode(n.n.Key)
		g.w.Update()
	}
}

// wheel zooms: scrolling is how the canvas is zoomed, whether the scroll
// came from a wheel's notches (angleDelta, 120 to a notch), a trackpad's
// two fingers (pixelDelta) or a pinch (which arrives as Ctrl+scroll). The
// view is moved by dragging it; Shift+scroll nudges it sideways, and so
// does a sideways scroll of its own, rather than zooming from a swipe that
// was never up or down.
func (g *explorerGraph) wheel(e *qt.QWheelEvent) {
	pos := e.Position()
	angle, pixel := e.AngleDelta(), e.PixelDelta()
	dx, dy := float64(angle.X()), float64(angle.Y())
	if !pixel.IsNull() {
		dx, dy = float64(pixel.X()), float64(pixel.Y())
	}
	switch {
	case e.Modifiers()&qt.ShiftModifier != 0:
		g.pan((dx+dy)/2, 0)
	case dy != 0:
		g.zoomAt(math.Pow(1.0015, dy), pos.X(), pos.Y())
	default:
		g.pan(dx/2, 0)
	}
	e.Accept()
}

// key handles the canvas shortcuts, reporting whether it used the key.
func (g *explorerGraph) key(e *qt.QKeyEvent) bool {
	const step = 60
	switch qt.Key(e.Key()) {
	case qt.Key_Plus, qt.Key_Equal:
		g.zoomBy(1.25)
	case qt.Key_Minus:
		g.zoomBy(1 / 1.25)
	case qt.Key_0:
		g.fit()
	case qt.Key_Left:
		g.pan(step, 0)
	case qt.Key_Right:
		g.pan(-step, 0)
	case qt.Key_Up:
		g.pan(0, step)
	case qt.Key_Down:
		g.pan(0, -step)
	default:
		return false
	}
	return true
}

// toggle opens or closes a node, in both views.
func (g *explorerGraph) toggle(key string) {
	g.pane.setExpanded(key, !g.byKey[key].open)
	g.relayout(key)
}

// ---- Painting ------------------------------------------------------------

func (g *explorerGraph) paint() {
	p := qt.NewQPainter2(g.w.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)

	w, h := float64(g.w.Width()), float64(g.w.Height())
	g.paintGrid(p, w, h)

	p.Translate2(g.offX, g.offY)
	p.Scale(g.zoom, g.zoom)

	// What's in view, in scene units, with room for a node's handle.
	vx, vy := g.toScene(0, 0)
	vw, vh := w/g.zoom, h/g.zoom
	inView := func(n *gnode) bool {
		return n.x+gNodeW+gHandleR >= vx && n.x <= vx+vw && n.y+gNodeH >= vy && n.y <= vy+vh
	}

	// Edges first, so the boxes sit on top: out of the parent's right
	// edge, down the gutter between the two columns, and into each child's
	// left edge.
	edge := qt.NewQPen3(color("inputBorder"))
	edge.SetWidthF(1.3)
	edge.SetCosmetic(true)
	p.SetPenWithPen(edge)
	for _, n := range g.nodes {
		if !n.open || len(n.kids) == 0 {
			continue
		}
		first, last := n.kids[0], n.kids[len(n.kids)-1]
		midX := n.x + gNodeW + gGapX/2
		if last.y+gNodeH < vy || first.y > vy+vh || n.x > vx+vw || first.x+gNodeW < vx {
			continue // entirely out of view
		}
		cy := n.y + gNodeH/2
		p.DrawLine4(qt.NewQPointF3(n.x+gNodeW, cy), qt.NewQPointF3(midX, cy))
		p.DrawLine4(qt.NewQPointF3(midX, first.y+gNodeH/2), qt.NewQPointF3(midX, last.y+gNodeH/2))
		for _, k := range n.kids {
			ky := k.y + gNodeH/2
			p.DrawLine4(qt.NewQPointF3(midX, ky), qt.NewQPointF3(k.x, ky))
		}
	}

	fonts := g.fonts()
	for _, n := range g.nodes {
		if inView(n) {
			g.paintNode(p, n, fonts)
		}
	}
}

// paintGrid dots the canvas so panning reads as movement even over empty
// space. The spacing grows when zoomed out, so the dots never crowd.
func (g *explorerGraph) paintGrid(p *qt.QPainter, w, h float64) {
	step := 24 * g.zoom
	for step < 14 {
		step *= 2
	}
	dot := color("borderSubtle")
	if g.zoom < 0.5 {
		dot = color("divider")
	}
	p.SetPenWithStyle(qt.NoPen)
	p.SetBrush(qt.NewQBrush3(dot))
	x0 := math.Mod(g.offX, step)
	y0 := math.Mod(g.offY, step)
	for x := x0; x < w; x += step {
		for y := y0; y < h; y += step {
			p.DrawEllipse3(qt.NewQPointF3(x, y), 1.1, 1.1)
		}
	}
}

type graphFonts struct {
	name, root, chip *qt.QFont
	nameFM, chipFM   *qt.QFontMetrics
}

func (g *explorerGraph) fonts() graphFonts {
	base := g.w.Font()
	f := graphFonts{name: qt.NewQFont5(base), root: qt.NewQFont5(base), chip: qt.NewQFont5(base)}
	f.root.SetWeight(qt.QFont__DemiBold)
	f.chip.SetWeight(qt.QFont__DemiBold)
	f.chip.SetPointSizeF(base.PointSizeF() * 0.72)
	f.nameFM = qt.NewQFontMetrics(f.root)
	f.chipFM = qt.NewQFontMetrics(f.chip)
	return f
}

func (g *explorerGraph) paintNode(p *qt.QPainter, n *gnode, f graphFonts) {
	node := n.n
	selected := node.Key == g.pane.selected
	hovered := node.Key == g.hovered && !g.hoverOnly
	discovered := node.Discovered()
	rect := qt.NewQRectF4(n.x, n.y, gNodeW, gNodeH)
	ink := hex(typeColor(node.Type))

	p.Save()
	if discovered && !selected {
		p.SetOpacity(discoveredAlpha)
	}
	fill := color("surface")
	switch {
	case selected:
		fill = color(colorSelected)
	case node.Entity == nil:
		fill = color("chipBg")
	case hovered:
		fill = color(colorHover)
	}
	border := qt.NewQPen3(color("inputBorder"))
	border.SetWidthF(1.2)
	switch {
	case selected:
		border = qt.NewQPen3(color(colorAccent))
		border.SetWidthF(2)
	case node.Entity != nil:
		border = qt.NewQPen3(tintColor(ink, 170))
		border.SetWidthF(1.2)
		if discovered {
			border.SetStyle(qt.DashLine)
		}
	case hovered:
		border = qt.NewQPen3(color(colorMuted))
		border.SetWidthF(1.2)
	}
	p.SetPenWithPen(border)
	p.SetBrush(qt.NewQBrush3(fill))
	p.DrawRoundedRect(rect, 8, 8)
	// The type's colour runs along the top of an entity's box.
	if node.Entity != nil {
		p.SetPenWithStyle(qt.NoPen)
		p.SetBrush(qt.NewQBrush3(qt.NewQColor6(ink)))
		path := qt.NewQPainterPath()
		path.AddRoundedRect(rect, 8, 8)
		p.SetClipPath(path)
		p.DrawRect(qt.NewQRectF4(n.x, n.y, gNodeW, 4))
		p.SetClipping(false)
	}
	p.Restore()

	if len(node.Children) > 0 {
		g.paintHandle(p, n)
	}
	if g.zoom < gTextZoom {
		return
	}

	p.Save()
	defer p.Restore()
	if discovered {
		p.SetOpacity(discoveredAlpha)
	}
	// Name, after the type's chip (with its target's colour for a domain
	// or host).
	left, width := n.x+10, gNodeW-20
	chip := qt.NewQRectF4(left, n.y+9, 14, 14)
	paintEntityChip(p, chip, node, g.pane.targetColor(node))
	left, width = left+19, width-19
	font, text := f.name, colorText
	if !node.IsURL() {
		font = f.root
	}
	if node.Entity == nil {
		text = colorMuted
	}
	if discovered {
		font = qt.NewQFont5(font)
		font.SetItalic(true)
	}
	p.SetFont(font)
	p.SetPen(color(text))
	name := qt.NewQFontMetrics(font).ElidedText(node.Name, qt.ElideRight, int(width))
	p.DrawText5(qt.NewQRectF4(left, n.y+6, width, 20), int(qt.AlignLeft|qt.AlignVCenter), name)

	// Second line: a chip per method, or what the node is when it has none.
	lineY := n.y + 28
	if node.IsURL() && node.Visited() {
		g.paintChips(p, node.Entity.Operations(), n.x+gNodeW/2, lineY, f)
		return
	}
	note := ""
	switch {
	case discovered && node.Entity.Origin == entities.OriginTarget:
		note = "target"
	case discovered:
		note = "discovered"
	case node.Total > 0:
		note = countOf(node.Total, "request", "requests")
		if node.Entity == nil {
			note += " below"
		}
	}
	p.SetFont(f.chip)
	p.SetPen(color(colorMuted))
	p.DrawText5(qt.NewQRectF4(n.x+6, lineY, gNodeW-12, 16), int(qt.AlignCenter), note)
}

// paintChips draws method chips centred on cx, as many as fit.
func (g *explorerGraph) paintChips(p *qt.QPainter, ops []*sitemap.Operation, cx, y float64, f graphFonts) {
	const gap, pad, maxW = 4.0, 5.0, gNodeW - 16
	bh := float64(f.chipFM.Height()) + 2
	var widths []float64
	total := 0.0
	for i, op := range ops {
		bw := float64(f.chipFM.HorizontalAdvance(op.Method)) + 2*pad
		more := float64(f.chipFM.HorizontalAdvance(fmt.Sprintf("+%d", len(ops)-i))) + gap
		if total+bw > maxW-more && i < len(ops)-1 {
			break
		}
		widths = append(widths, bw)
		total += bw + gap
	}
	rest := len(ops) - len(widths)
	restLabel := ""
	if rest > 0 {
		restLabel = fmt.Sprintf("+%d", rest)
		total += float64(f.chipFM.HorizontalAdvance(restLabel))
	} else {
		total -= gap
	}

	p.SetFont(f.chip)
	x := cx - total/2
	for i, bw := range widths {
		fg := hex(methodColor(ops[i].Method))
		r := qt.NewQRectF4(x, y, bw, bh)
		p.SetPen(tintColor(fg, 150))
		p.SetBrush(qt.NewQBrush3(tintColor(fg, 16)))
		p.DrawRoundedRect(r, 4, 4)
		p.SetPen(qt.NewQColor6(fg))
		p.DrawText5(r, int(qt.AlignCenter), ops[i].Method)
		x += bw + gap
	}
	if restLabel != "" {
		p.SetPen(color(colorMuted))
		p.DrawText5(qt.NewQRectF4(x, y, 40, bh), int(qt.AlignLeft|qt.AlignVCenter), restLabel)
	}
}

// paintHandle draws the open/close button on the right edge of a node that
// has children — the side they come out of: − when open, + when closed.
func (g *explorerGraph) paintHandle(p *qt.QPainter, n *gnode) {
	cx, cy := n.x+gNodeW, n.y+gNodeH/2
	hot := n.n.Key == g.hovered && g.hoverOnly
	ring := qt.NewQPen3(color("inputBorder"))
	if hot {
		ring = qt.NewQPen3(color(colorAccent))
	}
	ring.SetWidthF(1.2)
	p.SetPenWithPen(ring)
	p.SetBrush(qt.NewQBrush3(color("surface")))
	p.DrawEllipse3(qt.NewQPointF3(cx, cy), gHandleR, gHandleR)

	ink := color(colorMuted)
	if hot {
		ink = color(colorAccent)
	}
	mark := qt.NewQPen3(ink)
	mark.SetWidthF(1.5)
	mark.SetCapStyle(qt.RoundCap)
	p.SetPenWithPen(mark)
	p.DrawLine4(qt.NewQPointF3(cx-3.5, cy), qt.NewQPointF3(cx+3.5, cy))
	if !n.open {
		p.DrawLine4(qt.NewQPointF3(cx, cy-3.5), qt.NewQPointF3(cx, cy+3.5))
	}
}
