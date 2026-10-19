package ui

import (
	"fmt"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/capture"
	"reproxy/internal/entities"
	"reproxy/internal/sitemap"
	"reproxy/internal/target"
)

// explorerPane is the Entity Explorer section: every entity in the
// engagement's store (see packages entities and sitemap) — each domain,
// host and URL visited or discovered, on the hosts the targets cover or
// that were visited — shown either as a tree (the default) or as a graph
// to pan and zoom (explorerGraph), switched from the header. A host no
// target covers that the targets' traffic led to is Related: it belongs to
// the Targets section until it is promoted, so the Explorer hides it until
// the header's eye asks for it (see entities.Store.Drawn). Both views share what is open and what is selected. Each type
// has its colour, and an entity only discovered (no traffic reached it) is
// drawn faded. Clicking a node slides a sidebar out from the right
// describing the entity — a URL the way an OpenAPI document would — with
// its key/value store, a button listing the requests made to it or under
// it, and one removing it.
//
//	┌──────────────────────────────────┬──────────────────┐
//	│ Explorer [scope▾][types▾][kinds▾]│ /users/{id}   🗑 × │
//	│ ▾ D example.com                  │ [View Requests]  │
//	│   ▾ H https://app.example.com    │  GET  /users/{id}│
//	│     ▾ U users                    │  Parameters ...  │
//	│       • U {id}   GET DELETE      │  Details ...     │
//	└──────────────────────────────────┴──────────────────┘
//
// The tree is rebuilt from the entities store, in the background, only
// while the section is on screen: entities arriving while the user is
// elsewhere just mark it stale, so drawing the Explorer costs nothing until
// it is looked at. (The store itself is kept up to date by the indexer,
// off the proxy's goroutine, whether or not anyone is looking.)
type explorerPane struct {
	root     *qt.QWidget
	entities *entities.Store
	// store resolves a URL's sightings to the flows still listed.
	store   *capture.Store
	targets *target.Store
	// onReplay opens a captured request in the Replay section.
	onReplay func(capture.Flow)

	scope *qt.QComboBox
	// types filters the tree by entity type, content by type of content
	// (HTML, CSS, images...): drop-downs of checkboxes.
	types   *checkFilter
	content *checkFilter
	// related is the header's eye: whether the hosts the targets led to
	// but cover are drawn. Off by default — they are candidates for scope
	// rather than part of it, and the Targets section is where they are
	// dealt with.
	related     *iconButton
	showRelated bool
	views       *segmentedIcons
	tree        *qt.QTreeWidget
	graph       *explorerGraph
	pages       *qt.QStackedWidget // the empty state, the tree or the graph
	graphMode   bool
	hiddenType  map[sitemap.Type]bool
	hiddenKind  map[sitemap.Kind]bool
	typeEntries []filterEntry
	kindEntries []filterEntry

	sidebar      *qt.QWidget // animated container; its maximumWidth slides it
	sidebarAnim  *qt.QPropertyAnimation
	sidebarOpen  bool
	detailTitle  *qt.QLabel
	deleteButton *iconButton
	viewRequests *qt.QPushButton
	editDetails  *qt.QPushButton
	doc          *qt.QTextBrowser
	// hostOpen says whether a URL's Host section is open in the sidebar.
	// It starts closed, and stays as the user leaves it.
	hostOpen bool

	// full is the tree as built; m is what is shown, after the filters.
	// Details come from full, so a node whose own content is hidden (kept
	// only because it leads to shown ones) still describes itself.
	full *sitemap.Map
	m    *sitemap.Map
	// items maps tree items back to node keys, and itemByKey the other way;
	// both are rebuilt with the tree.
	items     map[uintptr]string
	itemByKey map[string]*qt.QTreeWidgetItem
	// selected is the node shown in the sidebar, by key.
	selected string
	// expanded remembers what the user opened or closed, by key, so a
	// rebuild keeps the tree as they left it. Absent: the default (domains
	// and their hosts open, the rest closed).
	expanded map[string]bool
	// populating is set while the tree is rebuilt, so the item signals that
	// fires aren't taken for the user's.
	populating bool

	active   bool // the section is on screen
	stale    bool // the store changed since the last build
	building bool
	again    bool // another build was asked for during this one
	debounce *qt.QTimer
}

const (
	explorerSidebarWidth = 460
	explorerRebuildDelay = 400 * time.Millisecond
	explorerColMethods   = 1
	explorerColCount     = 2

	explorerPageEmpty = 0
	explorerPageTree  = 1
	explorerPageGraph = 2
	// explorerPageFiltered explains an empty tree when the filters hid
	// everything.
	explorerPageFiltered = 3

	// discoveredAlpha is how opaque an entity only discovered is drawn.
	discoveredAlpha = 0.45
)

// kindLabels names each sitemap.Kind in the content filter.
var kindLabels = map[sitemap.Kind]string{
	sitemap.KindHTML:       "HTML",
	sitemap.KindJavaScript: "JavaScript",
	sitemap.KindCSS:        "CSS",
	sitemap.KindJSON:       "JSON",
	sitemap.KindXML:        "XML",
	sitemap.KindImage:      "Images",
	sitemap.KindFont:       "Fonts",
	sitemap.KindMedia:      "Audio and video",
	sitemap.KindFile:       "Files and downloads",
	sitemap.KindText:       "Plain text",
	sitemap.KindOther:      "Other and no response",
}

// typeLabels names each entity type, in the type filter and the sidebar.
var typeLabels = map[sitemap.Type]string{
	sitemap.TypeDomain: "Domains",
	sitemap.TypeHost:   "Hosts",
	sitemap.TypeURL:    "URLs",
}

// typeColor is the palette key of an entity type's colour.
func typeColor(t sitemap.Type) string {
	switch t {
	case sitemap.TypeDomain:
		return colorEntityDomain
	case sitemap.TypeHost:
		return colorEntityHost
	}
	return colorEntityURL
}

// typeLetter is what an entity type's chip says.
func typeLetter(t sitemap.Type) string {
	switch t {
	case sitemap.TypeDomain:
		return "D"
	case sitemap.TypeHost:
		return "H"
	}
	return "U"
}

func newExplorerPane(ents *entities.Store, store *capture.Store, targets *target.Store, onReplay func(capture.Flow)) *explorerPane {
	p := &explorerPane{
		root:       qt.NewQWidget2(),
		entities:   ents,
		store:      store,
		targets:    targets,
		onReplay:   onReplay,
		items:      make(map[uintptr]string),
		itemByKey:  make(map[string]*qt.QTreeWidgetItem),
		hiddenType: make(map[sitemap.Type]bool),
		hiddenKind: make(map[sitemap.Kind]bool),
		expanded:   make(map[string]bool),
		stale:      true,
	}
	for _, t := range sitemap.Types {
		p.typeEntries = append(p.typeEntries, filterEntry{id: string(t), label: typeLabels[t], color: typeColor(t)})
	}
	for _, k := range sitemap.Kinds {
		p.kindEntries = append(p.kindEntries, filterEntry{id: string(k), label: kindLabels[k]})
	}

	layout := qt.NewQHBoxLayout(p.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.SetSpacing(0)
	layout.AddWidget2(p.buildTreePanel(), 1)
	layout.AddWidget(p.buildSidebar())

	// Bursts of changes are folded into one rebuild.
	p.debounce = qt.NewQTimer()
	p.debounce.SetSingleShot(true)
	p.debounce.SetInterval(int(explorerRebuildDelay / time.Millisecond))
	p.debounce.OnTimeout(p.rebuild)

	p.reloadScopes()
	return p
}

func (p *explorerPane) buildTreePanel() *qt.QWidget {
	p.scope = qt.NewQComboBox2()
	p.scope.SetToolTip("Show one target's entities, or all of them")
	p.scope.OnCurrentIndexChanged(func(int) { p.rebuild() })

	p.tree = qt.NewQTreeWidget2()
	setName(p.tree.QObject, "ExplorerTree")
	p.tree.SetColumnCount(3)
	p.tree.SetHeaderHidden(true)
	p.tree.SetUniformRowHeights(true)
	p.tree.SetAnimated(true)
	p.tree.SetIndentation(18)
	p.tree.SetFrameShape(qt.QFrame__NoFrame)
	p.tree.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	p.tree.SetIconSize(qt.NewQSize2(entityIconSize, entityIconSize))
	header := p.tree.Header()
	header.SetStretchLastSection(false)
	header.SetSectionResizeMode2(0, qt.QHeaderView__Stretch)
	header.SetSectionResizeMode2(explorerColMethods, qt.QHeaderView__ResizeToContents)
	header.SetSectionResizeMode2(explorerColCount, qt.QHeaderView__ResizeToContents)
	p.tree.SetItemDelegateForColumn(explorerColMethods, newMethodChipsDelegate(p.tree).QAbstractItemDelegate)

	p.tree.OnCurrentItemChanged(func(current, _ *qt.QTreeWidgetItem) {
		if !p.populating && current != nil {
			p.selectNode(p.items[uintptr(current.UnsafePointer())])
		}
	})
	// Clicking the selected node again reopens a closed sidebar.
	p.tree.OnItemClicked(func(item *qt.QTreeWidgetItem, _ int) {
		p.selectNode(p.items[uintptr(item.UnsafePointer())])
	})
	p.tree.OnItemExpanded(func(item *qt.QTreeWidgetItem) { p.rememberExpanded(item, true) })
	p.tree.OnItemCollapsed(func(item *qt.QTreeWidgetItem) { p.rememberExpanded(item, false) })
	p.tree.SetContextMenuPolicy(qt.CustomContextMenu)
	p.tree.OnCustomContextMenuRequested(func(pos *qt.QPoint) {
		if item := p.tree.ItemAt(pos); item != nil {
			p.showMenu(p.items[uintptr(item.UnsafePointer())], p.tree.Viewport().MapToGlobalWithQPoint(pos))
		}
	})

	p.graph = newExplorerGraph(p)

	p.pages = qt.NewQStackedWidget2()
	p.pages.AddWidget(newEmptyState("Nothing explored yet",
		"Add a target, or browse the app through the proxy. Every domain, host and URL you visit, "+
			"and every link found in the responses, appears here."))
	p.pages.AddWidget(p.tree.QWidget)
	p.pages.AddWidget(p.graph.w)
	p.pages.AddWidget(newEmptyState("Nothing to show",
		"The filters at the top right hide everything in the Explorer. Choose more types "+
			"of entity or content to show."))

	p.related = newIconButton(iconEyeOff, "", nil)
	p.related.onClick = func() { p.setShowRelated(!p.showRelated) }
	p.syncRelated()

	p.views = newSegmentedIcons([]iconKind{iconList, iconMap},
		[]string{"Show the Explorer as a tree", "Show the Explorer as a graph you can pan and zoom"},
		func(i int) { p.setGraphMode(i == 1) })

	p.types = newCheckFilter("Choose which types of entity the Explorer shows", p.typeEntries,
		func(id string, shown bool) {
			p.hiddenType[sitemap.Type(id)] = !shown
			p.filterChanged()
		},
		func(hidden int) string {
			switch hidden {
			case 0:
				return "All types"
			case len(sitemap.Types):
				return "No types"
			}
			return fmt.Sprintf("%d of %d types", len(sitemap.Types)-hidden, len(sitemap.Types))
		})
	p.content = newCheckFilter("Choose which types of content the Explorer's URLs show", p.kindEntries,
		func(id string, shown bool) {
			p.hiddenKind[sitemap.Kind(id)] = !shown
			p.filterChanged()
		},
		func(hidden int) string {
			switch hidden {
			case 0:
				return "All content"
			case len(sitemap.Kinds):
				return "No content"
			}
			return fmt.Sprintf("%d of %d kinds", len(sitemap.Kinds)-hidden, len(sitemap.Kinds))
		})

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)

	head := qt.NewQWidget2()
	setName(head.QObject, "PanelHeader")
	head.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(head)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)
	title := qt.NewQLabel3("Explorer")
	setName(title.QObject, "PanelTitle")
	h.AddWidget(title.QWidget)
	h.AddStretch()
	h.AddWidget(p.scope.QWidget)
	h.AddWidget(p.types.button.QWidget)
	h.AddWidget(p.content.button.QWidget)
	h.AddWidget(p.related.w)
	h.AddWidget(p.views.w)
	h.AddWidget(newIconButton(iconTrash, "Clear the Explorer (the captured requests are kept)", p.clearAll).asDanger().w)

	v.AddWidget(head)
	v.AddWidget(p.pages.QWidget)
	return panel.QWidget
}

// setShowRelated draws the related hosts, or hides them again. Unlike the
// type and content filters this one changes what is asked of the store, so
// the tree is built afresh rather than filtered.
func (p *explorerPane) setShowRelated(on bool) {
	if p.showRelated == on {
		return
	}
	p.showRelated = on
	p.syncRelated()
	p.rebuild()
}

// syncRelated puts the state on the eye: struck through while the related
// hosts are hidden, tinted while they are shown.
func (p *explorerPane) syncRelated() {
	if p.showRelated {
		p.related.setIcon(iconEye)
		p.related.w.SetToolTip("Hide the hosts the targets led to but don't cover (they stay on the Targets section)")
	} else {
		p.related.setIcon(iconEyeOff)
		p.related.w.SetToolTip("Show the hosts the targets led to but don't cover, as well as the targets' own")
	}
	p.related.setActive(p.showRelated)
}

// filterChanged re-applies the filters after a checkbox changed.
func (p *explorerPane) filterChanged() {
	if p.full != nil {
		p.render()
	}
}

// hiddenKinds lists the content types the filter hides, for saving.
func (p *explorerPane) hiddenKinds() []string { return p.content.hiddenIDs() }

// setHiddenKinds hides exactly the given content types and shows the rest.
func (p *explorerPane) setHiddenKinds(kinds []string) { p.content.setHidden(kinds, true) }

// hiddenTypes lists the entity types the filter hides, for saving.
func (p *explorerPane) hiddenTypes() []string { return p.types.hiddenIDs() }

// setHiddenTypes hides exactly the given entity types and shows the rest.
func (p *explorerPane) setHiddenTypes(types []string) { p.types.setHidden(types, true) }

// updateCounts labels the filters' checkboxes with what the tree holds.
func (p *explorerPane) updateCounts() {
	kinds, types := p.full.KindCounts(), p.full.TypeCounts()
	p.content.setCounts(func(id string) int { return kinds[sitemap.Kind(id)] })
	p.types.setCounts(func(id string) int { return types[sitemap.Type(id)] })
}

func (p *explorerPane) buildSidebar() *qt.QWidget {
	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	panel.SetFixedWidth(explorerSidebarWidth)
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)

	head := qt.NewQWidget2()
	setName(head.QObject, "PanelHeader")
	head.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(head)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)
	p.detailTitle = qt.NewQLabel2()
	setName(p.detailTitle.QObject, "ExplorerDetailTitle")
	p.detailTitle.SetTextInteractionFlags(qt.TextSelectableByMouse)
	p.detailTitle.SetMinimumWidth(0)
	h.AddWidget2(p.detailTitle.QWidget, 1)
	p.deleteButton = newIconButton(iconTrash, "Remove from the Explorer", func() { p.deleteEntity(p.selected) }).asDanger()
	h.AddWidget(p.deleteButton.w)
	h.AddWidget(newIconButton(iconClose, "Close", func() { p.setSidebarOpen(false) }).w)

	actions := qt.NewQWidget2()
	a := qt.NewQHBoxLayout(actions)
	a.SetContentsMargins(rowPadX, 10, rowPadX, 0)
	a.SetSpacing(8)
	p.viewRequests = qt.NewQPushButton3("View Requests")
	setName(p.viewRequests.QObject, "PrimaryButton")
	p.viewRequests.OnClicked(p.openRequests)
	p.editDetails = qt.NewQPushButton3("Edit Details…")
	p.editDetails.SetToolTip("Edit the entity's key/value store")
	p.editDetails.OnClicked(func() { p.editKV(p.selected) })
	a.AddWidget(p.viewRequests.QWidget)
	a.AddWidget(p.editDetails.QWidget)
	a.AddStretch()

	p.doc = qt.NewQTextBrowser2()
	setName(p.doc.QObject, "ExplorerDoc")
	p.doc.SetFrameShape(qt.QFrame__NoFrame)
	p.doc.SetOpenLinks(false)
	p.doc.OnAnchorClicked(func(u *qt.QUrl) {
		if u.ToString() == hostSectionLink {
			p.toggleHostSection()
		}
	})
	// Tables are sized to the width; never scroll sideways.
	p.doc.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)

	v.AddWidget(head)
	v.AddWidget(actions)
	v.AddWidget2(p.doc.QWidget, 1)

	// The container keeps the gap to the tree, and is what slides: its
	// maximumWidth runs from 0 (closed) to the panel plus the gap.
	p.sidebar = qt.NewQWidget2()
	s := qt.NewQHBoxLayout(p.sidebar)
	s.SetContentsMargins(windowMargin, 0, 0, 0)
	s.AddWidget(panel.QWidget)
	p.sidebar.SetMinimumWidth(0)
	p.sidebar.SetMaximumWidth(0)
	p.sidebarAnim = qt.NewQPropertyAnimation2(p.sidebar.QObject, []byte("maximumWidth"))
	p.sidebarAnim.SetDuration(180)
	p.sidebarAnim.SetEasingCurve(qt.NewQEasingCurve3(qt.QEasingCurve__OutCubic))
	return p.sidebar
}

// setActive tells the pane whether its section is on screen. It catches up
// on changes that arrived while it was hidden.
func (p *explorerPane) setActive(on bool) {
	p.active = on
	if on && p.stale {
		p.rebuild()
	}
}

// storeChanged is called for store events. It only schedules work.
func (p *explorerPane) storeChanged() {
	p.stale = true
	if p.active && !p.debounce.IsActive() {
		p.debounce.Start2()
	}
}

// rebuild draws the entities in scope on a background goroutine and shows
// the result. A request made while one is running is queued behind it.
func (p *explorerPane) rebuild() {
	if p.building {
		p.again = true
		return
	}
	p.building, p.stale = true, false

	scope := ""
	if p.scope.Count() > 0 {
		scope = p.scope.CurrentData().ToString()
	}
	list := p.entities.Drawn(scope, p.showRelated)
	go func() {
		m := sitemap.Build(list)
		mainthread.Start(func() {
			p.building = false
			p.show(m)
			if p.again {
				p.again = false
				p.rebuild()
			}
		})
	}()
}

// show takes a newly built tree.
func (p *explorerPane) show(full *sitemap.Map) {
	p.full = full
	p.updateCounts()
	p.render()
}

// filtered reports whether either filter hides anything.
func (p *explorerPane) filtered() bool {
	return len(p.hiddenKinds()) > 0 || len(p.hiddenTypes()) > 0
}

// render shows the tree through the filters, in both views, keeping
// expansion and selection.
func (p *explorerPane) render() {
	m := p.full
	if p.filtered() {
		m = p.full.Filter(sitemap.Filter{
			Kind: func(k sitemap.Kind) bool { return !p.hiddenKind[k] },
			Type: func(t sitemap.Type) bool { return !p.hiddenType[t] },
		})
	}
	p.m = m
	p.populating = true
	scroll := p.tree.VerticalScrollBar().Value()
	p.tree.Clear()
	p.items = make(map[uintptr]string)
	p.itemByKey = make(map[string]*qt.QTreeWidgetItem)
	var current *qt.QTreeWidgetItem
	for _, n := range m.Roots {
		item := p.addItem(nil, n, &current)
		p.tree.AddTopLevelItem(item)
		p.applyExpansion(item, n)
	}
	if current != nil {
		p.tree.SetCurrentItem(current)
	}
	p.tree.VerticalScrollBar().SetValue(scroll)
	p.populating = false

	p.graph.setMap(m)
	p.showPage()

	// The selected node's details may have changed; it may also be gone,
	// or filtered out.
	if p.selected != "" {
		if m.Find(p.selected) != nil {
			p.renderDetails(p.full.Find(p.selected))
		} else {
			p.selected = ""
			p.setSidebarOpen(false)
		}
	}
}

// targetColor is the colour of the target n is drawn under, for a domain
// or host; "" otherwise.
func (p *explorerPane) targetColor(n *sitemap.Node) string {
	if n.IsURL() {
		return ""
	}
	if t, ok := p.targets.Get(n.TargetID); ok {
		return t.Color
	}
	return ""
}

// addItem builds the item for n and its subtree. current is set to the
// item of the selected node, if it is in the subtree.
func (p *explorerPane) addItem(parent *qt.QTreeWidgetItem, n *sitemap.Node, current **qt.QTreeWidgetItem) *qt.QTreeWidgetItem {
	var item *qt.QTreeWidgetItem
	if parent == nil {
		item = qt.NewQTreeWidgetItem()
	} else {
		item = qt.NewQTreeWidgetItem6(parent)
	}
	p.items[uintptr(item.UnsafePointer())] = n.Key
	p.itemByKey[n.Key] = item
	if n.Key == p.selected {
		*current = item
	}

	discovered := n.Discovered()
	item.SetIcon(0, entityIcon(n, p.targetColor(n)))
	f := qt.NewQFont5(p.tree.Font()) // a copy: Font() is the tree's own
	if !n.IsURL() {
		f.SetWeight(qt.QFont__DemiBold)
	}
	f.SetItalic(discovered)
	item.SetFont(0, f)
	switch {
	case discovered:
		ink := color(colorText)
		ink.SetAlphaF(discoveredAlpha)
		item.SetForeground(0, qt.NewQBrush3(ink))
	case n.Entity == nil:
		item.SetForeground(0, qt.NewQBrush3(color(colorMuted)))
	}
	item.SetText(0, n.Name)
	item.SetToolTip(0, nodeTip(n))

	if n.IsURL() && n.Visited() {
		var methods []string
		for _, op := range n.Entity.Operations() {
			methods = append(methods, op.Method)
		}
		item.SetData(explorerColMethods, int(qt.UserRole), qt.NewQVariant14(strings.Join(methods, " ")))
	}
	if n.Total > 0 {
		item.SetText(explorerColCount, fmt.Sprint(n.Total))
		item.SetTextAlignment(explorerColCount, int(qt.AlignRight|qt.AlignVCenter))
		item.SetForeground(explorerColCount, qt.NewQBrush3(color(colorMuted)))
		item.SetToolTip(explorerColCount, countOf(n.Total, "request", "requests")+" here and below")
	}

	for _, c := range n.Children {
		p.addItem(item, c, current)
	}
	return item
}

// nodeTip describes a node for a tooltip: what it is, and whether traffic
// reached it.
func nodeTip(n *sitemap.Node) string {
	tip := n.Label()
	if n.Entity != nil {
		tip = strings.TrimSuffix(typeLabels[n.Type], "s") + " · " + tip
	}
	switch {
	case n.Discovered() && n.Entity.Origin == entities.OriginTarget:
		tip += "\nNamed by a target; no traffic yet"
	case n.Discovered():
		tip += "\nDiscovered; no traffic reached it yet"
	case n.Total > 0:
		tip += "\n" + countOf(n.Total, "request", "requests") + " here and below"
	}
	return tip
}

// isExpanded reports whether n is open: as the user left it, or closed by
// default. The Explorer opens on its domains alone, with everything under
// each of them folded away until the user asks for it — a morning's
// browsing is thousands of URLs, and all of them at once is no use to
// anyone. "Expand All" on a node opens a whole branch at a time.
func (p *explorerPane) isExpanded(n *sitemap.Node) bool {
	if open, ok := p.expanded[n.Key]; ok {
		return open
	}
	return false
}

// expandAll opens key and everything under it, in both views. It works
// from the filtered tree, so it opens what is actually drawn.
func (p *explorerPane) expandAll(key string) {
	n := p.m.Find(key)
	if n == nil {
		return
	}
	var walk func(*sitemap.Node)
	walk = func(c *sitemap.Node) {
		if len(c.Children) == 0 {
			return
		}
		p.expanded[c.Key] = true
		for _, k := range c.Children {
			walk(k)
		}
	}
	walk(n)
	if item, ok := p.itemByKey[key]; ok {
		p.populating = true
		p.applyExpansion(item, n)
		p.populating = false
		p.tree.ScrollToItem(item)
	}
	p.graph.relayout(key)
}

// setExpanded opens or closes a node from the graph, keeping the tree in
// step.
func (p *explorerPane) setExpanded(key string, open bool) {
	p.expanded[key] = open
	if item, ok := p.itemByKey[key]; ok {
		p.populating = true
		item.SetExpanded(open)
		p.populating = false
	}
}

// applyExpansion opens the items the user had open (or the default ones),
// after the item is in the tree (expanding before that has no effect).
func (p *explorerPane) applyExpansion(item *qt.QTreeWidgetItem, n *sitemap.Node) {
	item.SetExpanded(p.isExpanded(n))
	for i, c := range n.Children {
		p.applyExpansion(item.Child(i), c)
	}
}

func (p *explorerPane) rememberExpanded(item *qt.QTreeWidgetItem, open bool) {
	if p.populating {
		return
	}
	if key, ok := p.items[uintptr(item.UnsafePointer())]; ok {
		p.expanded[key] = open
		p.graph.relayout(key)
	}
}

// setGraphMode switches between the tree and the graph.
func (p *explorerPane) setGraphMode(on bool) {
	p.graphMode = on
	if on {
		p.views.setCurrent(1)
	} else {
		p.views.setCurrent(0)
	}
	p.showPage()
}

// showPage shows the view chosen, or the empty state when there is
// nothing to show yet.
func (p *explorerPane) showPage() {
	switch {
	case p.full == nil || len(p.full.Roots) == 0:
		p.pages.SetCurrentIndex(explorerPageEmpty)
	case len(p.m.Roots) == 0:
		p.pages.SetCurrentIndex(explorerPageFiltered)
	case p.graphMode:
		p.pages.SetCurrentIndex(explorerPageGraph)
		p.graph.reveal()
	default:
		p.pages.SetCurrentIndex(explorerPageTree)
		if item, ok := p.itemByKey[p.selected]; ok {
			p.tree.ScrollToItem(item)
		}
	}
}

// selectNode shows a node in the sidebar, sliding it out if it is closed.
func (p *explorerPane) selectNode(key string) {
	if p.m.Find(key) == nil {
		return
	}
	n := p.full.Find(key)
	p.selected = key
	// Keep the tree's current item in step when the graph selected it.
	if item, ok := p.itemByKey[key]; ok && p.tree.CurrentItem() != item {
		p.populating = true
		p.tree.SetCurrentItem(item)
		p.populating = false
	}
	p.renderDetails(n)
	p.setSidebarOpen(true)
	p.graph.w.Update()
}

func (p *explorerPane) renderDetails(n *sitemap.Node) {
	title := n.Name
	if n.IsURL() {
		title = n.Path
	}
	p.detailTitle.SetText(title)
	p.detailTitle.SetToolTip(n.Label())
	p.deleteButton.w.SetVisible(n.Entity != nil)
	p.editDetails.SetVisible(n.Entity != nil)

	requests := len(p.flowIDs(n))
	p.viewRequests.SetText(fmt.Sprintf("View Requests (%d)", requests))
	p.viewRequests.SetEnabled(requests > 0)
	switch {
	case requests > 0 && n.IsURL():
		p.viewRequests.SetToolTip("List the requests made to this URL")
	case requests > 0:
		p.viewRequests.SetToolTip("List the requests made to the URLs under it")
	case n.IsURL():
		p.viewRequests.SetToolTip("Nothing has been requested at exactly this path")
	default:
		p.viewRequests.SetToolTip("Nothing has been requested under it")
	}

	scroll := p.doc.VerticalScrollBar().Value()
	p.doc.SetHtml(entityDocHTML(n, p.targets, p.hostDetails(n)))
	p.doc.VerticalScrollBar().SetValue(scroll)
}

// hostDetails looks up the hosts n's sidebar describes besides n itself
// (see hostDetails): from the store, since an address out of scope isn't
// drawn.
func (p *explorerPane) hostDetails(n *sitemap.Node) hostDetails {
	h := hostDetails{open: p.hostOpen}
	e := n.Entity
	switch {
	case e == nil:
	case e.Type == sitemap.TypeURL:
		origin, ok := p.entities.Get(e.Parent)
		if !ok {
			break
		}
		h.origin = &origin
		if name, ok := p.entities.Get(origin.Parent); ok && name.IsName() {
			h.name = &name
			for _, a := range name.HostAttrs.Addresses {
				if addr, ok := p.entities.Get(entities.HostKey("", a)); ok {
					h.addresses = append(h.addresses, addr)
				}
			}
		}
	case e.IsName() && e.HostAttrs.IP:
		h.resolvedFrom = p.entities.ResolvingTo(e.Host)
	}
	return h
}

// toggleHostSection opens or closes the URLs' Host section; it stays as
// the user left it as they move from URL to URL.
func (p *explorerPane) toggleHostSection() {
	p.hostOpen = !p.hostOpen
	if n := p.full.Find(p.selected); n != nil {
		p.renderDetails(n)
	}
}

// flowIDs is the requests a node stands for: a URL's own, or every URL's
// under a domain or host.
func (p *explorerPane) flowIDs(n *sitemap.Node) []uint64 {
	switch {
	case n.Entity == nil:
		return nil
	case n.IsURL():
		return n.Entity.FlowIDs()
	}
	return p.entities.FlowIDs(n.Key)
}

func (p *explorerPane) setSidebarOpen(open bool) {
	if open == p.sidebarOpen {
		return
	}
	p.sidebarOpen = open
	to := 0
	if open {
		to = explorerSidebarWidth + windowMargin
	} else {
		p.selected = ""
		p.tree.ClearSelection()
		p.graph.w.Update()
	}
	p.sidebarAnim.Stop()
	p.sidebarAnim.SetStartValue(qt.NewQVariant4(p.sidebar.MaximumWidth()))
	p.sidebarAnim.SetEndValue(qt.NewQVariant4(to))
	p.sidebarAnim.Start()
}

// showMenu is a node's context menu.
func (p *explorerPane) showMenu(key string, at *qt.QPoint) {
	n := p.full.Find(key)
	if n == nil {
		return
	}
	menu := qt.NewQMenu2()
	// The drawn node, not the full one: what Expand All would open.
	drawn := p.m.Find(key)
	expand := menu.AddActionWithText("Expand All Children")
	expand.SetEnabled(drawn != nil && len(drawn.Children) > 0)
	expand.OnTriggered(func() { p.expandAll(key) })
	menu.AddSeparator()
	copyKey := menu.AddActionWithText("Copy " + map[bool]string{true: "URL", false: "Name"}[n.IsURL()])
	copyKey.OnTriggered(func() { qt.QGuiApplication_Clipboard().SetText(n.Label()) })
	if n.Entity != nil {
		menu.AddActionWithText("Edit Details…").OnTriggered(func() { p.editKV(key) })
		menu.AddSeparator()
		menu.AddActionWithText("Remove from the Explorer…").OnTriggered(func() { p.deleteEntity(key) })
	}
	menu.Exec3(at, nil)
	menu.DeleteLater()
}

// editKV edits an entity's key/value store in a dialog.
func (p *explorerPane) editKV(key string) {
	e, ok := p.entities.Get(key)
	if !ok {
		return
	}
	pairs, ok := runKVDialog(p.root, e)
	if !ok {
		return
	}
	if _, err := p.entities.ReplaceKV(key, pairs); err != nil {
		qt.QMessageBox_Warning(p.root, "Edit details", sentence(err.Error()))
	}
}

// deleteEntity removes an entity and everything under it, after asking.
func (p *explorerPane) deleteEntity(key string) {
	n := p.full.Find(key)
	if n == nil || n.Entity == nil {
		return
	}
	below := 0
	var count func(*sitemap.Node)
	count = func(c *sitemap.Node) {
		for _, k := range c.Children {
			if k.Entity != nil {
				below++
			}
			count(k)
		}
	}
	count(n)
	text := fmt.Sprintf("Remove %s from the Explorer?", n.Label())
	if below > 0 {
		text = fmt.Sprintf("Remove %s and the %s under it from the Explorer?", n.Label(), countOf(below, "entity", "entities"))
	}
	text += "\n\nThe captured requests are kept, and new traffic to it puts it back. This can't be undone."
	answer := qt.QMessageBox_Question6(p.root, "Remove from the Explorer", text,
		qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
	if answer != qt.QMessageBox__Yes {
		return
	}
	if err := p.entities.Delete(key); err != nil {
		qt.QMessageBox_Warning(p.root, "Remove from the Explorer", sentence(err.Error()))
	}
}

// clearAll empties the engagement's Explorer, after asking. The traffic
// stays: the Explorer fills again as new requests are made.
func (p *explorerPane) clearAll() {
	n := p.entities.Len()
	if n == 0 {
		return
	}
	answer := qt.QMessageBox_Question6(p.root, "Clear the Explorer",
		fmt.Sprintf("Remove the %s from the Explorer?\n\nThe captured requests are kept, the targets' "+
			"domains and addresses come straight back, and new traffic is explored as it arrives. "+
			"This can't be undone.", countOf(n, "entity", "entities")),
		qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
	if answer == qt.QMessageBox__Yes {
		p.entities.Clear()
	}
}

// openRequests lists the requests the selected node stands for in a window
// of their own.
func (p *explorerPane) openRequests() {
	n := p.full.Find(p.selected)
	if n == nil {
		return
	}
	var flows []capture.Flow
	for _, id := range p.flowIDs(n) {
		if f, ok := p.store.Get(id); ok {
			flows = append(flows, f)
		}
	}
	openEntityRequests(p.root, n.Label(), flows, p.store, p.onReplay)
}

// reloadScopes refills the target selector, keeping the choice when the
// target still exists.
func (p *explorerPane) reloadScopes() {
	current := ""
	if p.scope.Count() > 0 {
		current = p.scope.CurrentData().ToString()
	}
	p.scope.BlockSignals(true)
	p.scope.Clear()
	p.scope.AddItem3("All traffic", qt.NewQVariant14(""))
	for _, t := range p.targets.List() {
		p.scope.AddItem4(swatchIcon(t.Color), t.Label(), qt.NewQVariant14(t.ID))
	}
	if i := p.scope.FindData(qt.NewQVariant14(current)); i >= 0 {
		p.scope.SetCurrentIndex(i)
	}
	p.scope.BlockSignals(false)
}

// targetsChanged follows edits to targets: labels and colours in the
// selector and on the nodes, and which hosts are drawn at all — a related
// host promoted to a target brings its entities into the Explorer.
func (p *explorerPane) targetsChanged() {
	p.reloadScopes()
	p.storeChanged()
}

// refreshTheme re-renders what has the theme's colours baked in.
func (p *explorerPane) refreshTheme() {
	p.types.refreshTheme()
	if p.full != nil {
		p.render()
	}
	p.graph.w.Update()
}

// entityIconSize is the side of a tree row's type chip.
const entityIconSize = 16

// entityIcon is a tree row's type chip: the type's letter on its colour,
// faded for an entity only discovered, with a dot of the target's colour
// in the corner when targetColor is set. A path prefix (no entity) gets an
// empty ring, so the names still line up.
func entityIcon(n *sitemap.Node, targetColor string) *qt.QIcon {
	const size = entityIconSize
	pix := qt.NewQPixmap2(size, size)
	pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
	pt := qt.NewQPainter2(pix.QPaintDevice)
	pt.SetRenderHint(qt.QPainter__Antialiasing)
	paintEntityChip(pt, qt.NewQRectF4(1, 1, size-2, size-2), n, targetColor)
	pt.End()
	return qt.NewQIcon2(pix)
}

// paintEntityChip draws a type chip in rect (see entityIcon).
func paintEntityChip(pt *qt.QPainter, rect *qt.QRectF, n *sitemap.Node, targetColor string) {
	pt.Save()
	defer pt.Restore()
	ink := hex(typeColor(n.Type))
	if n.Entity == nil {
		ring := qt.NewQPen3(tintColor(ink, 110))
		ring.SetWidthF(1.2)
		pt.SetPenWithPen(ring)
		pt.SetBrushWithStyle(qt.NoBrush)
		inset := rect.Width() * 0.3
		pt.DrawEllipse(rect.Adjusted(inset, inset, -inset, -inset))
		return
	}
	if n.Discovered() {
		pt.SetOpacity(discoveredAlpha)
	}
	pt.SetPenWithStyle(qt.NoPen)
	pt.SetBrush(qt.NewQBrush3(qt.NewQColor6(ink)))
	pt.DrawRoundedRect(rect, rect.Width()*0.25, rect.Width()*0.25)
	f := qt.NewQFont()
	f.SetWeight(qt.QFont__Bold)
	f.SetPixelSize(int(rect.Height() * 0.68))
	pt.SetFont(f)
	pt.SetPen(qt.NewQColor6("#ffffff"))
	pt.DrawText5(rect, int(qt.AlignCenter), typeLetter(n.Type))
	if targetColor != "" {
		pt.SetOpacity(1)
		d := rect.Width() * 0.42
		ring := qt.NewQPen3(color("surface"))
		ring.SetWidthF(1.2)
		pt.SetPenWithPen(ring)
		pt.SetBrush(qt.NewQBrush3(qt.NewQColor6(targetColor)))
		pt.DrawEllipse(qt.NewQRectF4(rect.Right()-d+1, rect.Bottom()-d+1, d, d))
	}
}

// newMethodChipsDelegate paints the methods column: one chip per method
// used on the URL, as in the request list.
func newMethodChipsDelegate(view *qt.QTreeWidget) *qt.QStyledItemDelegate {
	d := qt.NewQStyledItemDelegate2(view.QObject)
	chipFont := qt.NewQFont5(view.Font())
	chipFont.SetWeight(qt.QFont__DemiBold)
	chipFont.SetPointSizeF(view.Font().PointSizeF() * 0.78)
	fm := qt.NewQFontMetrics(chipFont)
	const gap = 4

	chips := func(idx *qt.QModelIndex) []string {
		return strings.Fields(idx.DataWithRole(int(qt.UserRole)).ToString())
	}
	d.OnSizeHint(func(super func(*qt.QStyleOptionViewItem, *qt.QModelIndex) *qt.QSize, opt *qt.QStyleOptionViewItem, idx *qt.QModelIndex) *qt.QSize {
		size := super(opt, idx)
		w := 0
		for _, m := range chips(idx) {
			w += fm.HorizontalAdvance(m) + badgePadX*2 + gap
		}
		return qt.NewQSize2(w+8, size.Height())
	})
	d.OnPaint(func(super func(*qt.QPainter, *qt.QStyleOptionViewItem, *qt.QModelIndex), p *qt.QPainter, opt *qt.QStyleOptionViewItem, idx *qt.QModelIndex) {
		super(p, opt, idx) // background: hover and selection
		p.Save()
		defer p.Restore()
		p.SetRenderHint(qt.QPainter__Antialiasing)
		p.SetFont(chipFont)
		r := opt.Rect()
		x := r.X() + 4
		bh := fm.Height() + 2
		y := r.Y() + (r.Height()-bh)/2
		for _, m := range chips(idx) {
			fg := hex(methodColor(m))
			bw := fm.HorizontalAdvance(m) + badgePadX*2
			rect := qt.NewQRectF4(float64(x), float64(y), float64(bw), float64(bh))
			p.SetPen(tintColor(fg, 150))
			p.SetBrush(qt.NewQBrush3(tintColor(fg, 16)))
			p.DrawRoundedRect(rect, 5, 5)
			p.SetPen(qt.NewQColor6(fg))
			p.DrawText5(rect, int(qt.AlignCenter), m)
			x += bw + gap
		}
	})
	return d
}
