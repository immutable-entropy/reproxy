package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"
)

// navBar is the main window's top-level navigation: one item per section of
// the app, plus Plugins and Settings on their own at the far end. Picking an item decides
// what the rest of the window shows (see mainWindow.selectSection), so the
// bar owns no content of its own.
//
// Items are icon-only by default, with the name beside the icon when
// View → Navigation → Show Names is on. Each can carry a notification badge
// (a count) so a section can call attention to itself while the user is
// looking at another one.
//
// The bar is anchored left (the default), top, right or bottom, or hidden
// altogether, from Settings → General.

// navSide is where the bar is anchored. The value is what is stored in the
// config (config.Config.NavPosition).
type navSide string

const (
	navLeft   navSide = "left"
	navTop    navSide = "top"
	navRight  navSide = "right"
	navBottom navSide = "bottom"
	navHidden navSide = "hidden"
)

// navSideOptions is the order shown in Settings → General.
var navSideOptions = []option[navSide]{
	{"Left", navLeft},
	{"Top", navTop},
	{"Right", navRight},
	{"Bottom", navBottom},
	{"Hidden", navHidden},
}

// navSideFor maps a stored value to a side, defaulting to the left.
func navSideFor(stored string) navSide {
	for _, o := range navSideOptions {
		if o.value == navSide(stored) {
			return o.value
		}
	}
	return navLeft
}

// vertical reports whether the bar runs down a side of the window, as
// opposed to across the top or bottom.
func (s navSide) vertical() bool { return s == navLeft || s == navRight }

// Section IDs. Keep them stable: they identify the selected section in the
// saved layout. (Traffic was "intercept" before it was renamed; a layout
// saved then reopens on the default section, which is Traffic anyway. The
// Explorer was the Map, and keeps its ID.)
const (
	sectionTargets  = "targets"
	sectionTraffic  = "traffic"
	sectionReplay   = "replay"
	sectionExplorer = "map"
	sectionAnalyze  = "analyze"
	sectionFindings = "findings"
	sectionFiles    = "files"
)

// navSection describes one item of the bar, in the order they appear.
type navSection struct {
	id    string
	label string
	icon  iconKind
	// hidden keeps a section out of the bar while it is being built: the
	// page is still made and still registered, so the work in progress
	// stays wired up and nothing else has to know it isn't on the bar.
	// Clear it to put the section back.
	hidden bool
}

var navSections = []navSection{
	{id: sectionTargets, label: "Targets", icon: iconTargets},
	{id: sectionTraffic, label: "Traffic", icon: iconTraffic},
	{id: sectionReplay, label: "Replay", icon: iconReplay},
	{id: sectionExplorer, label: "Explorer", icon: iconMap},
	{id: sectionFiles, label: "Files", icon: iconFiles},
	{id: sectionFindings, label: "Findings", icon: iconFindings},
	{id: sectionAnalyze, label: "Analyze", icon: iconAnalyze, hidden: true},
}

// shown reports whether a section has an item on the bar.
func shown(id string) bool {
	for _, s := range navSections {
		if s.id == id {
			return !s.hidden
		}
	}
	return false
}

// Bar metrics. The rail is wider when names are shown; items keep a constant
// extent along the bar either way.
const (
	navRailWidth      = 58
	navRailLabelWidth = 168
	navBarHeight      = 48
	navItemExtent     = 46 // item height on a vertical bar
	navItemWidth      = 58 // item width on a horizontal bar, icons only
	navIconSize       = 22
	navItemPad        = 10 // gap between an item's edge and its icon
	navLabelGap       = 10 // gap between icon and name
	navItemInset      = 4  // margin of the hover/selected background
	navBadgeHeight    = 15.0
	navBadgeReserve   = 22 // room kept for a notification count beside a name
)

type navBar struct {
	root     *qt.QWidget
	layout   *qt.QBoxLayout
	items    []*navButton
	plugins  *navButton
	settings *navButton

	side    navSide
	labels  bool
	current string

	onSelect func(id string)
}

// newNavBar builds the bar, anchored left with icons only. onSelect is called
// with a section ID when the user picks one; onPlugins and onSettings when
// they click the puzzle piece or the cog, which open windows of their own.
func newNavBar(onSelect func(id string), onPlugins, onSettings func()) *navBar {
	n := &navBar{root: qt.NewQWidget2(), side: navLeft, onSelect: onSelect}
	setName(n.root.QObject, "NavBar")
	n.root.SetAttribute(qt.WA_StyledBackground)

	n.layout = qt.NewQBoxLayout2(qt.QBoxLayout__TopToBottom, n.root)
	n.layout.SetContentsMargins(0, 6, 0, 6)
	n.layout.SetSpacing(2)

	for _, s := range navSections {
		if s.hidden {
			continue
		}
		item := newNavButton(s, nil)
		item.onClick = func() { n.selectItem(item.section.id) }
		n.items = append(n.items, item)
		n.layout.AddWidget(item.w)
	}
	n.layout.AddStretch()
	n.plugins = newNavButton(navSection{id: "plugins", label: "Plugins", icon: iconPlugins}, onPlugins)
	n.layout.AddWidget(n.plugins.w)
	n.settings = newNavButton(navSection{id: "settings", label: "Settings", icon: iconSettings}, onSettings)
	n.layout.AddWidget(n.settings.w)

	n.applyMode()
	return n
}

// setSide anchors the bar, or hides it. The caller moves the bar to the
// matching edge of the window (mainWindow.applyNavSide).
func (n *navBar) setSide(s navSide) {
	n.side = s
	n.root.SetVisible(s != navHidden)
	if s == navHidden {
		return
	}
	if s.vertical() {
		n.layout.SetDirection(qt.QBoxLayout__TopToBottom)
		n.layout.SetContentsMargins(0, 6, 0, 6)
	} else {
		n.layout.SetDirection(qt.QBoxLayout__LeftToRight)
		n.layout.SetContentsMargins(6, 0, 6, 0)
	}
	// The stylesheet draws the divider on the edge that faces the content.
	n.root.SetProperty("side", qt.NewQVariant14(string(s)))
	n.root.Style().Unpolish(n.root)
	n.root.Style().Polish(n.root)
	n.applyMode()
}

// setLabels shows or hides the item names.
func (n *navBar) setLabels(on bool) {
	n.labels = on
	n.applyMode()
}

// applyMode resizes the bar and its items for the current side and label
// setting.
func (n *navBar) applyMode() {
	for _, item := range n.all() {
		item.setMode(n.side.vertical(), n.labels)
	}
	if n.side.vertical() {
		width := navRailWidth
		if n.labels {
			width = navRailLabelWidth
		}
		n.root.SetMinimumHeight(0)
		n.root.SetMaximumHeight(qtWidgetMax)
		n.root.SetFixedWidth(width)
	} else {
		n.root.SetMinimumWidth(0)
		n.root.SetMaximumWidth(qtWidgetMax)
		n.root.SetFixedHeight(navBarHeight)
	}
	n.root.UpdateGeometry()
	n.root.Update()
}

// qtWidgetMax is Qt's QWIDGETSIZE_MAX: it lifts a fixed-size constraint set
// for the other orientation.
const qtWidgetMax = 16777215

// all returns the section items plus Plugins and Settings.
func (n *navBar) all() []*navButton {
	return append(append([]*navButton{}, n.items...), n.plugins, n.settings)
}

// selectItem reports the user's choice. Marking the item current is left to
// the owner (through setCurrent), which switches sections the same way
// whether the bar or the app itself asked for it — and can still see which
// section is being left.
func (n *navBar) selectItem(id string) {
	if n.onSelect == nil {
		n.setCurrent(id)
		return
	}
	n.onSelect(id)
}

// setCurrent marks a section as current without reporting a choice.
func (n *navBar) setCurrent(id string) {
	n.current = id
	for _, item := range n.items {
		item.setSelected(item.section.id == id)
	}
}

// setBadge sets a section's notification count; 0 clears it.
func (n *navBar) setBadge(id string, count int) {
	for _, item := range n.items {
		if item.section.id == id {
			item.setBadge(count)
			return
		}
	}
}

// refreshTheme repaints the bar after a theme change, since the icons are
// painted in the active palette's colours.
func (n *navBar) refreshTheme() {
	for _, item := range n.all() {
		item.w.Update()
	}
}

// navButton is one item of the bar: a painted icon, an optional name, a
// hover/selected background, and a notification badge.
type navButton struct {
	w       *qt.QWidget
	section navSection

	selected bool
	hovered  bool
	badge    int

	vertical bool // the bar runs down a side, so items stack
	labels   bool

	onClick func()
}

func newNavButton(s navSection, onClick func()) *navButton {
	b := &navButton{w: qt.NewQWidget2(), section: s, onClick: onClick, vertical: true}
	b.w.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	b.w.SetToolTip(s.label)
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
		if e.Button() == qt.LeftButton && b.onClick != nil {
			b.onClick()
		}
	})
	b.w.OnSizeHint(func(func() *qt.QSize) *qt.QSize { return b.sizeHint() })
	b.applySizePolicy()
	return b
}

func (b *navButton) setMode(vertical, labels bool) {
	b.vertical, b.labels = vertical, labels
	b.applySizePolicy()
	b.w.UpdateGeometry()
	b.w.Update()
}

// applySizePolicy makes items stretch across a vertical bar's width, and
// keep their own width on a horizontal one.
func (b *navButton) applySizePolicy() {
	if b.vertical {
		b.w.SetSizePolicy2(qt.QSizePolicy__Expanding, qt.QSizePolicy__Fixed)
	} else {
		b.w.SetSizePolicy2(qt.QSizePolicy__Fixed, qt.QSizePolicy__Expanding)
	}
}

func (b *navButton) sizeHint() *qt.QSize {
	if b.vertical {
		width := navRailWidth
		if b.labels {
			width = navRailLabelWidth
		}
		return qt.NewQSize2(width, navItemExtent)
	}
	width := navItemWidth
	if b.labels {
		width = navItemPad*2 + navIconSize + navLabelGap + b.labelWidth() + navBadgeReserve
	}
	return qt.NewQSize2(width, navBarHeight-12)
}

func (b *navButton) labelWidth() int {
	return qt.NewQFontMetrics(b.w.Font()).HorizontalAdvance(b.section.label)
}

func (b *navButton) setSelected(on bool) {
	if b.selected != on {
		b.selected = on
		b.w.Update()
	}
}

func (b *navButton) setBadge(count int) {
	if b.badge != count {
		b.badge = count
		b.w.Update()
	}
}

func (b *navButton) paint() {
	p := qt.NewQPainter2(b.w.QPaintDevice)
	defer p.End()
	p.SetRenderHint(qt.QPainter__Antialiasing)
	p.SetRenderHint(qt.QPainter__TextAntialiasing)

	w, h := float64(b.w.Width()), float64(b.w.Height())
	inset := float64(navItemInset)

	switch {
	case b.selected:
		p.SetPenWithStyle(qt.NoPen)
		p.SetBrush(qt.NewQBrush3(color(colorSelected)))
		p.DrawRoundedRect(qt.NewQRectF4(inset, inset, w-2*inset, h-2*inset), 7, 7)
	case b.hovered:
		p.SetPenWithStyle(qt.NoPen)
		p.SetBrush(qt.NewQBrush3(color(colorHover)))
		p.DrawRoundedRect(qt.NewQRectF4(inset, inset, w-2*inset, h-2*inset), 7, 7)
	}

	// A bar on the edge facing the content marks the current section.
	if b.selected {
		p.SetBrush(qt.NewQBrush3(color(colorAccent)))
		if b.vertical {
			p.DrawRoundedRect(qt.NewQRectF4(0, h*0.22, 3, h*0.56), 1.5, 1.5)
		} else {
			p.DrawRoundedRect(qt.NewQRectF4(w*0.22, h-3, w*0.56, 3), 1.5, 1.5)
		}
	}

	ink := colorMuted
	switch {
	case b.selected:
		ink = colorAccent
	case b.hovered:
		ink = colorText
	}

	iconX, iconY := (w-navIconSize)/2, (h-navIconSize)/2
	if b.labels {
		iconX = navItemPad
		if b.vertical {
			iconX = navItemPad + 2
		}
		drawIcon(p, b.section.icon, iconX, iconY, navIconSize, color(ink))
		p.SetPen(color(ink))
		textX := iconX + navIconSize + navLabelGap
		p.DrawText5(qt.NewQRectF4(textX, 0, w-textX-navItemPad-navBadgeReserve, h),
			int(qt.AlignLeft|qt.AlignVCenter), b.section.label)
		// With a name to collide with, the count goes at the far end of the
		// item rather than over the icon.
		if b.badge > 0 {
			b.paintBadge(p, w-navItemPad, h/2, qt.AlignRight)
		}
		return
	}
	drawIcon(p, b.section.icon, iconX, iconY, navIconSize, color(ink))
	if b.badge > 0 {
		b.paintBadge(p, iconX+navIconSize, iconY+navBadgeHeight*0.35, qt.AlignHCenter)
	}
}

// paintBadge draws the notification count as a pill centred vertically on y,
// placed at x either centred on it or ending at it.
func (b *navButton) paintBadge(p *qt.QPainter, x, y float64, align qt.AlignmentFlag) {
	text := fmt.Sprint(b.badge)
	if b.badge > 99 {
		text = "99+"
	}
	// A copy: QWidget.Font hands back the widget's own font, and shrinking
	// that one would shrink the item's name too.
	font := qt.NewQFont5(b.w.Font())
	font.SetPointSizeF(font.PointSizeF() - 1.5)
	font.SetBold(true)
	p.SetFont(font)
	fm := qt.NewQFontMetrics(font)

	const padX = 5.0
	width := float64(fm.HorizontalAdvance(text)) + 2*padX
	if width < navBadgeHeight {
		width = navBadgeHeight
	}
	left := x - width/2
	if align == qt.AlignRight {
		left = x - width
	}
	rect := qt.NewQRectF4(left, y-navBadgeHeight/2, width, navBadgeHeight)

	p.SetPenWithStyle(qt.NoPen)
	p.SetBrush(qt.NewQBrush3(color(colorHeld)))
	p.DrawRoundedRect(rect, navBadgeHeight/2, navBadgeHeight/2)
	p.SetPen(color("onAccent"))
	p.DrawText5(rect, int(qt.AlignCenter), text)
}
