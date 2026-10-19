package ui

import qt "github.com/mappu/miqt/qt6"

// sessionsPanel is the Sessions panel, which was the bottom panel: the
// Agents tab hosts the LLM chat and its Sessions column (CLAUDE.md, goal 3),
// and the Terminal tab runs the system shell, laid out the same way, and is
// where reproxy's own command line will be driven from.
//
// The panel owns its tabs, which of them are shown, and the kebab in its
// top right corner. Where it sits is the main window's business: it is
// pinned to the bottom of the window by default, and View → Sessions →
// Position (or the kebab, which offers the same actions) pins it to any of
// the four edges instead, or puts it in a window of its own. See
// mainWindow.applySessionsSide.

// sessionsSide is where the panel is pinned. The value is what is stored in
// the saved layout (config.UIState.SessionsSide).
type sessionsSide string

const (
	sessionsBottom sessionsSide = "bottom"
	sessionsTop    sessionsSide = "top"
	sessionsLeft   sessionsSide = "left"
	sessionsRight  sessionsSide = "right"
	sessionsWindow sessionsSide = "window"
)

// sessionsSideOptions is the order shown in View → Sessions → Position and
// in the panel's kebab.
var sessionsSideOptions = []option[sessionsSide]{
	{"Pinned to Bottom", sessionsBottom},
	{"Pinned to Top", sessionsTop},
	{"Pinned to Left", sessionsLeft},
	{"Pinned to Right", sessionsRight},
	{"Own Window", sessionsWindow},
}

// icon is the dock-position glyph for a side, in the style of a browser's
// dev-tools dock picker (see navicons.go).
func (s sessionsSide) icon() iconKind {
	switch s {
	case sessionsTop:
		return iconDockTop
	case sessionsLeft:
		return iconDockLeft
	case sessionsRight:
		return iconDockRight
	case sessionsWindow:
		return iconDockWindow
	default:
		return iconDockBottom
	}
}

// sessionsSideFor maps a stored value to a side, defaulting to the bottom.
func sessionsSideFor(stored string) sessionsSide {
	for _, o := range sessionsSideOptions {
		if o.value == sessionsSide(stored) {
			return o.value
		}
	}
	return sessionsBottom
}

// docked reports whether the panel sits in the main window rather than in a
// window of its own.
func (s sessionsSide) docked() bool { return s != sessionsWindow }

// beside reports whether the panel runs down a side of the window, as
// opposed to across its top or bottom: the splitter it shares with the
// sections is turned on its side for those two.
func (s sessionsSide) beside() bool { return s == sessionsLeft || s == sessionsRight }

// first reports whether the panel comes before the sections in that
// splitter, which is what puts it at the top or the left.
func (s sessionsSide) first() bool { return s == sessionsTop || s == sessionsLeft }

// Tab IDs. Keep them stable: they name the hidden tabs in the saved layout.
const (
	sessionsTerminal = "terminal"
	sessionsAgents   = "agents"
)

// sessionsTab is one tab of the panel, in the order they appear.
type sessionsTab struct {
	id, label string
}

var sessionsTabs = []sessionsTab{
	{sessionsTerminal, "Terminal"},
	{sessionsAgents, "Agents"},
}

type sessionsPanel struct {
	root *qt.QWidget
	tabs *qt.QTabWidget

	// pages is each tab's content by ID; order is the IDs of the tabs
	// currently in the bar, so the one on screen can be kept across a
	// rebuild. hidden is what View → Sessions → Contents has unticked.
	pages  map[string]*qt.QWidget
	order  []string
	hidden map[string]bool

	// The kebab in the top right corner and the menu it drops. The menu is a
	// dev-tools-style row of dock-position icons (buildSideRow); onSide is
	// what the main window wires in to act on a pick, and sideButtons lets the
	// row show which side is current.
	kebab       *iconButton
	menu        *qt.QMenu
	sideButtons map[sessionsSide]*iconButton
	onSide      func(sessionsSide)

	// narrow is set while the panel is too thin for a tab's two columns to
	// sit side by side, and onNarrow is what lays them out instead.
	narrow   bool
	onNarrow func(stacked bool)
}

// sessionsNarrow is the width below which a tab's two columns stack. A tab
// is a main column and a Sessions column abreast, which wants most of a
// window's width: pinned to the left or the right there is nowhere near
// that, so they go one above the other instead. It follows the panel's own
// width rather than where it is pinned, so dragging the divider is enough
// to change it back.
const sessionsNarrow = 640

// newSessionsPanel builds the panel around the pages its tabs show, keyed by
// tab ID.
func newSessionsPanel(pages map[string]*qt.QWidget) *sessionsPanel {
	p := &sessionsPanel{pages: pages, hidden: map[string]bool{}}

	p.tabs = qt.NewQTabWidget2()
	p.tabs.SetDocumentMode(true)

	p.menu = qt.NewQMenu2()
	p.buildSideRow()
	p.kebab = newIconButton(iconKebab, "Where this panel sits", nil)
	p.kebab.onClick = p.popup
	// The menu grabs the mouse while it is open, so the button never hears
	// the pointer leave: it is asked again once the menu is gone.
	p.menu.OnAboutToHide(func() { p.kebab.setHovered(p.kebab.w.UnderMouse()) })
	p.rebuild()
	p.showTabByID(sessionsAgents) // the Agents tab is the one that opens

	// The kebab sits in the strip the tabs leave empty, over the tab bar's
	// right end. It is laid over the tabs rather than given to the tab
	// widget as a corner widget, which the stylesheet's QTabWidget rules
	// leave unplaced: a one-cell grid holding both puts it in the corner
	// with no geometry of our own to keep up to date. Its container is
	// what holds it off the frame's edge, and is no wider than the button,
	// so the tabs below stay clickable.
	corner := qt.NewQWidget2()
	ch := qt.NewQHBoxLayout(corner)
	ch.SetContentsMargins(0, 3, 8, 0)
	ch.AddWidget(p.kebab.w)

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	g := qt.NewQGridLayout(panel.QWidget)
	g.SetContentsMargins(1, 4, 1, 1)
	g.AddWidget2(p.tabs.QWidget, 0, 0)
	g.AddWidget4(corner, 0, 0, qt.AlignRight|qt.AlignTop)
	panel.SetMinimumHeight(80)
	panel.SetMinimumWidth(320) // it can be pinned to a side as well
	panel.OnResizeEvent(func(super func(*qt.QResizeEvent), e *qt.QResizeEvent) {
		super(e)
		p.setNarrow(e.Size().Width() < sessionsNarrow)
	})
	p.root = panel.QWidget
	return p
}

// setStacker registers how a tab lays its two columns out when the panel is
// too narrow for them side by side, and applies the width it has now.
func (p *sessionsPanel) setStacker(fn func(stacked bool)) {
	p.onNarrow = fn
	fn(p.narrow)
}

func (p *sessionsPanel) setNarrow(on bool) {
	if p.narrow == on {
		return
	}
	p.narrow = on
	if p.onNarrow != nil {
		p.onNarrow(on)
	}
}

// stackColumns turns a tab's splitter upright so its main column sits above
// its Sessions column, or lays the two side by side again.
func stackColumns(split *qt.QSplitter, stacked bool) {
	if stacked {
		split.SetOrientation(qt.Vertical)
		split.SetStretchFactor(0, 65)
		split.SetStretchFactor(1, 35)
		split.SetSizes([]int{650, 350})
		return
	}
	split.SetOrientation(qt.Horizontal)
	split.SetStretchFactor(0, 75)
	split.SetStretchFactor(1, 25)
	split.SetSizes([]int{750, 250})
}

// buildSideRow fills the kebab's menu with a single row of dock-position
// icons, the way a browser's dev tools present the same choice: each edge of
// the window, plus floating it in its own window, as a button with the
// position's name for its tooltip. The current position is tinted.
func (p *sessionsPanel) buildSideRow() {
	p.sideButtons = make(map[sessionsSide]*iconButton, len(sessionsSideOptions))
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(6, 4, 6, 4)
	h.SetSpacing(2)
	for _, o := range sessionsSideOptions {
		side := o.value
		b := newIconButton(side.icon(), o.label, func() { p.chooseSide(side) })
		p.sideButtons[side] = b
		h.AddWidget(b.w)
	}
	wa := qt.NewQWidgetAction(p.menu.QObject)
	wa.SetDefaultWidget(row)
	p.menu.AddAction(wa.QAction)
}

// chooseSide acts on a pick from the kebab's icon row and closes the menu.
func (p *sessionsPanel) chooseSide(side sessionsSide) {
	p.menu.Hide()
	if p.onSide != nil {
		p.onSide(side)
	}
}

// markSide tints the icon of the position the panel now sits in, so the kebab
// row shows the current choice however it was made (the row, the View menu or
// a restored layout).
func (p *sessionsPanel) markSide(side sessionsSide) {
	for s, b := range p.sideButtons {
		b.setActive(s == side)
	}
}

// popup drops the kebab's menu under it, right edges aligned: the button
// sits in the panel's own corner, so a menu hanging off to the left is the
// only one that stays on screen.
func (p *sessionsPanel) popup() {
	b := p.kebab.w
	x := b.Width() - p.menu.SizeHint().Width()
	p.menu.Popup(b.MapToGlobalWithQPoint(qt.NewQPoint2(x, b.Height())))
}

// setHidden replaces the set of tabs left out of the panel, for restoring a
// saved layout.
func (p *sessionsPanel) setHidden(ids []string) {
	p.hidden = make(map[string]bool, len(ids))
	for _, id := range ids {
		p.hidden[id] = true
	}
	p.rebuild()
}

// hiddenIDs lists the tabs left out, in the order they would appear.
func (p *sessionsPanel) hiddenIDs() []string {
	var ids []string
	for _, t := range sessionsTabs {
		if p.hidden[t.id] {
			ids = append(ids, t.id)
		}
	}
	return ids
}

// shows reports whether a tab is in the panel.
func (p *sessionsPanel) shows(id string) bool { return !p.hidden[id] }

// setShown puts a tab in the panel or takes it out. With every tab out the
// panel holds nothing, which is how Contents turns it off altogether (see
// mainWindow.refreshSessions).
func (p *sessionsPanel) setShown(id string, on bool) {
	if p.hidden[id] == !on {
		return
	}
	p.hidden[id] = !on
	p.rebuild()
}

// empty reports whether every tab has been taken out.
func (p *sessionsPanel) empty() bool { return len(p.order) == 0 }

// rebuild puts the shown tabs in the bar, in their fixed order. Qt has no
// hidden tab, so a tab is taken out of the bar and put back; the page
// widgets themselves outlive it, so a shell or a chat goes on running while
// its tab is away.
func (p *sessionsPanel) rebuild() {
	was := ""
	if i := p.tabs.CurrentIndex(); i >= 0 && i < len(p.order) {
		was = p.order[i]
	}
	for p.tabs.Count() > 0 {
		p.tabs.RemoveTab(0)
	}
	p.order = p.order[:0]
	for _, t := range sessionsTabs {
		if p.hidden[t.id] {
			continue
		}
		p.tabs.AddTab(p.pages[t.id], t.label)
		p.order = append(p.order, t.id)
	}
	p.showTabByID(was)
}

// showTabByID brings a tab to the front if it is in the bar.
func (p *sessionsPanel) showTabByID(id string) {
	for i, have := range p.order {
		if have == id {
			p.tabs.SetCurrentIndex(i)
			return
		}
	}
}
