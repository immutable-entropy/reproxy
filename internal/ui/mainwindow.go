package ui

import (
	"fmt"
	"html"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/filter"
	"reproxy/internal/findings"
	"reproxy/internal/replay"
)

// Layout constants for the main window.
const (
	defaultWidth  = 1280
	defaultHeight = 860
	windowMargin  = 10
	listShare     = 30 // percent of the width given to the request list
	bottomShare   = 28 // percent of the height given to a panel across the bottom
	// sideShare is the width a panel pinned to the left or the right gets:
	// a little more than the height it gets across the bottom, since it is
	// columns of text that have to fit across it.
	sideShare = 32
)

type mainWindow struct {
	ctrl   Controller
	win    *qt.QMainWindow
	model  *flowModel
	list   *qt.QListView
	detail *detailPane

	// The Traffic section's request list runs the full width, in columns
	// under flowHeader, until a request is clicked; then detailPanel slides
	// out on the right and the list narrows to its two-line rows.
	// listPages switches the list for a hint while nothing is captured.
	flowHeader   *qt.QWidget
	listPages    *qt.QStackedWidget
	listHint     *qt.QLabel
	detailOpen   bool
	detailWidth  int // the pane's width when last open; 0 = default share
	detailAnim   *qt.QVariantAnimation
	viewerAction *qt.QAction

	// crawl is the Autocrawl window, made the first time it is opened.
	crawl *crawlWindow
	// quitting is set once the main window's close has begun, so closing
	// the windows that go with it can't re-enter the handler.
	quitting bool

	// Request-list filter bar. It is hidden by default and slides out when the
	// header's search button is clicked; the list shrinks to make room.
	filterEdit *qt.QLineEdit
	regexBtn   *qt.QToolButton
	// dnsBtn switches DNS lookups into the request list. It sits in the
	// Traffic panel's header, beside the search button, so the lookups can
	// be shown without opening the filter bar first.
	dnsBtn *qt.QToolButton
	// methodFilter is the header's funnel: which request methods the list
	// shows. Its debounce folds a Select All / Select None, which reports
	// every method in turn, into one rebuild of the rows.
	methodFilter   *checkFilter
	methodDebounce *qt.QTimer
	filterBox      *qt.QFrame // the bordered field+toggle box (carries invalid styling)
	filterBar      *qt.QWidget
	filterBarH     int                    // the bar's natural (expanded) height
	filterAnim     *qt.QPropertyAnimation // animates the bar's maximumHeight
	searchBtn      *qt.QWidget            // the header toggle (painted)
	searchHover    bool
	searchOpen     bool

	// Density: densityBtn is the Traffic header's button and compactAction
	// the window shortcut behind it; autoCompacted guards the one-shot
	// auto-switch past compactThreshold so it fires only once.
	densityBtn    *iconButton
	compactAction *qt.QAction
	autoCompacted bool

	// pulseTimer animates the transient row flashes (forward/drop feedback);
	// flashTimer restores the status line after a transient flash message.
	pulseTimer *qt.QTimer
	flashTimer *qt.QTimer

	// Panels that can be hidden, and the toggles that hide them, keyed by
	// the names used in the saved layout.
	top          *qt.QSplitter // request list | detail
	outer        *qt.QSplitter // sections and the Sessions panel
	listPanel    *qt.QWidget
	detailPanel  *qt.QWidget // slides out; not a toggle
	panelToggles map[string]*qt.QAction

	// The Sessions panel: the panel itself, where it is pinned, the edge it
	// returns to when its own window is closed, that window (made the
	// first time it is needed) and its saved geometry. sessionsActs and
	// sessionsTabActs are View → Sessions' two groups of items, which the
	// panel's kebab shares.
	sessions        *sessionsPanel
	sessionsSide    sessionsSide
	sessionsDock    sessionsSide
	sessionsWin     *qt.QWidget
	sessionsGeom    []byte
	sessionsActs    map[sessionsSide]*qt.QAction
	sessionsTabActs map[string]*qt.QAction

	// Top-level navigation: the bar, the stack of sections it switches
	// between, and the layout the bar is anchored in.
	nav          *navBar
	sections     *qt.QStackedWidget
	sectionIndex map[string]int
	bodyLayout   *qt.QBoxLayout
	navLabels    *qt.QAction
	navSideActs  map[navSide]*qt.QAction
	replay       *replayPane
	targets      *targetsPane
	explorer     *explorerPane
	files        *filesPane
	findings     *findingsPane

	agents *agentsPanel
	// terminals is the Sessions panel's Terminal tab.
	terminals *terminalPanel

	// File-menu items that follow the open engagement.
	recentMenu      *qt.QMenu
	closeEngagement *qt.QAction
	unsubEngagement func()

	interceptAction *qt.QAction
	interceptToggle *interceptToggle
	interceptStatus *qt.QLabel
	mcpStatus       *qt.QLabel
	// upstreamStatus says which upstream chain is in force, when there is
	// one or the open engagement chose its own (see refreshUpstreamStatus).
	upstreamStatus *qt.QLabel
	status         *qt.QLabel
	total          *qt.QLabel
	mcpTimer       *qt.QTimer

	unsubscribe func()
	// unsubEntities stops following the Entity Explorer's store;
	// entitiesDirty is set while a redraw it asked for is on its way to the
	// main thread, so a burst of changes asks once.
	unsubEntities func()
	entitiesDirty atomic.Bool
	// relatedUnseen counts the hosts that became related since the user
	// last looked at Targets: the Targets badge.
	relatedUnseen int
}

func newMainWindow(ctrl Controller) *mainWindow {
	w := &mainWindow{
		ctrl:         ctrl,
		win:          qt.NewQMainWindow2(),
		model:        newFlowModel(),
		panelToggles: make(map[string]*qt.QAction),
		sectionIndex: make(map[string]int),
		sessionsSide: sessionsBottom,
		sessionsDock: sessionsBottom,
	}
	w.model.targetColor = w.targetColor
	w.win.Resize(defaultWidth, defaultHeight)

	// Body before menus: the View menu needs the panels to exist.
	w.win.SetCentralWidget(w.buildBody())
	w.buildStatusBar()
	w.buildMenus()
	w.applyNavSide(navSideFor(ctrl.Settings().NavPosition))
	w.restoreLayout(ctrl.UIState())

	// The title and the File menu follow the open engagement, which can also
	// change from outside this window (another process opening one).
	w.unsubEngagement = ctrl.Engagements().Subscribe(func() {
		mainthread.Start(w.refreshEngagement)
	})
	w.refreshEngagement()

	// Seed with anything captured before the window existed, then follow
	// live changes. Events arrive on proxy goroutines.
	for _, f := range ctrl.Store().List() {
		w.model.apply(capture.Event{Kind: capture.FlowAdded, Flow: f})
	}
	w.unsubscribe = ctrl.Store().Subscribe(func(e capture.Event) {
		mainthread.Start(func() { w.onStoreEvent(e) })
	})
	// The Explorer is drawn from the entities store, which the indexer
	// fills from the traffic in the background. A batch of traffic, or a
	// Clear, changes many entities at once: that is one redraw request.
	w.unsubEntities = ctrl.Entities().Subscribe(func(e entities.Event) {
		switch e.Kind {
		case entities.Created, entities.Updated, entities.Deleted:
			if !w.entitiesDirty.CompareAndSwap(false, true) {
				return
			}
			mainthread.Start(func() {
				w.entitiesDirty.Store(false)
				w.onEntityEvent(entities.Event{Kind: entities.Updated})
			})
			return
		}
		mainthread.Start(func() { w.onEntityEvent(e) })
	})

	// Closing this window closes reproxy: see quitOnClose.
	w.win.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		super(e)
		w.quitOnClose()
	})

	w.refreshStatus()
	return w
}

func (w *mainWindow) show() { w.win.Show() }

// quitOnClose takes the rest of reproxy down with the main window, whether
// it was closed from its own button or File → Quit. Without it the process
// would go on running behind whatever other window it had opened — the
// Autocrawl window is a window of its own, not a dialog — with the proxy
// still listening and nothing left to stop it from.
//
// The browsers reproxy launched are stopped a moment later, by the app's
// own shutdown (app.Close) once the event loop has ended; they are separate
// processes rather than windows of ours.
func (w *mainWindow) quitOnClose() {
	if w.quitting {
		return // closeOtherWindows can bring us back round
	}
	w.quitting = true
	w.closeOtherWindows()
	// Quit explicitly rather than leaving it to Qt's "last window closed":
	// a window that declines to close must not keep reproxy alive.
	qt.QCoreApplication_Quit()
}

// closeOtherWindows closes the windows that go with the main one. Qt's own
// sweep is used rather than a list of our own, so a window added later is
// covered without anyone remembering to add it here; the main window is in
// it too, which is why quitOnClose guards against coming back round.
func (w *mainWindow) closeOtherWindows() {
	qt.QApplication_CloseAllWindows()
}

// close saves the layout and stops listening for store events. It runs
// after the event loop ends, while the window still exists.
func (w *mainWindow) close() {
	if w.unsubscribe != nil {
		w.unsubscribe()
	}
	if w.unsubEntities != nil {
		w.unsubEntities()
	}
	if w.agents != nil {
		w.agents.close()
	}
	if w.terminals != nil {
		w.terminals.close()
	}
	if w.pulseTimer != nil {
		w.pulseTimer.Stop()
	}
	if w.flashTimer != nil {
		w.flashTimer.Stop()
	}
	if w.mcpTimer != nil {
		w.mcpTimer.Stop()
	}
	if w.replay != nil {
		w.replay.closeAll()
	}
	if w.crawl != nil {
		w.crawl.close()
	}
	if w.targets != nil {
		w.targets.close()
	}
	if w.files != nil {
		w.files.close()
	}
	if w.findings != nil {
		w.findings.close()
	}
	if w.unsubEngagement != nil {
		w.unsubEngagement()
	}
	if err := w.ctrl.SaveUIState(w.layout()); err != nil {
		fmt.Fprintln(os.Stderr, "reproxy: saving window layout:", err)
	}
}

// Names of the panels that can be hidden, as they appear in the saved
// layout. Keep them stable: changing one resets that panel for everyone.
const (
	panelRequests  = "requests"
	panelStatusBar = "status_bar"
)

// layout captures the window size and position, divider positions and
// which panels are shown.
func (w *mainWindow) layout() config.UIState {
	s := config.UIState{
		WindowGeometry:      w.win.SaveGeometry(),
		OuterSplitter:       w.outer.SaveState(),
		TrafficDetailWidth:  w.currentDetailWidth(),
		Panels:              make(map[string]bool, len(w.panelToggles)),
		Compact:             w.model.compact,
		ShowDNS:             w.model.showDNS,
		HiddenMethods:       w.model.hiddenMethodIDs(),
		NavSection:          w.nav.current,
		NavLabels:           w.navLabels.IsChecked(),
		ExplorerGraph:       w.explorer.graphMode,
		ExplorerHiddenKinds: w.explorer.hiddenKinds(),
		ExplorerHiddenTypes: w.explorer.hiddenTypes(),
		ExplorerRelated:     w.explorer.showRelated,
		SessionsSide:        string(w.sessionsSide),
		SessionsHidden:      w.sessions.hiddenIDs(),
		SessionsWindow:      w.sessionsWindowGeometry(),
	}
	for name, a := range w.panelToggles {
		s.Panels[name] = a.IsChecked()
	}
	return s
}

// restoreLayout applies a saved layout. Anything missing or unreadable keeps
// the default, so a stale or partial file is harmless. Hidden panels keep
// their last size, so showing one again brings it back where it was.
func (w *mainWindow) restoreLayout(s config.UIState) {
	if len(s.WindowGeometry) > 0 {
		w.win.RestoreGeometry(s.WindowGeometry)
	}
	w.detailWidth = s.TrafficDetailWidth
	// Where the Sessions panel sits decides the outer splitter's child
	// order, so it is put there before the saved divider positions are
	// restored into it.
	w.sessionsGeom = s.SessionsWindow
	w.sessions.setHidden(s.SessionsHidden)
	for id, a := range w.sessionsTabActs {
		a.SetChecked(w.sessions.shows(id))
	}
	w.applySessionsSide(sessionsSideFor(s.SessionsSide))
	if len(s.OuterSplitter) > 0 {
		w.outer.RestoreState(s.OuterSplitter)
	}
	for name, shown := range s.Panels {
		if a, ok := w.panelToggles[name]; ok {
			a.SetChecked(shown) // the toggle shows or hides the panel
		}
	}
	if s.Compact {
		w.compactAction.SetChecked(true) // toggled handler applies it
	}
	if s.NavLabels {
		w.navLabels.SetChecked(true) // toggled handler applies it
	}
	if s.ShowDNS {
		w.dnsBtn.SetChecked(true) // toggled handler applies it
	}
	if len(s.HiddenMethods) > 0 {
		w.methodFilter.setHidden(s.HiddenMethods, true) // notified, then applied once
	}
	w.explorer.setGraphMode(s.ExplorerGraph)
	w.explorer.setShowRelated(s.ExplorerRelated)
	w.explorer.setHiddenKinds(s.ExplorerHiddenKinds)
	w.explorer.setHiddenTypes(s.ExplorerHiddenTypes)
	// A section with no item on the bar is not somewhere to reopen on: a
	// layout saved while Analyze was still listed opens on the default.
	if _, ok := w.sectionIndex[s.NavSection]; ok && shown(s.NavSection) {
		w.selectSection(s.NavSection)
	}
}

// ---- Menus ---------------------------------------------------------------

func (w *mainWindow) buildMenus() {
	bar := w.win.MenuBar()

	file := bar.AddMenuWithTitle("&File")
	w.addAction(file, "&New Engagement…", "Ctrl+Shift+N", w.newEngagement)
	w.addAction(file, "&Open Engagement…", "Ctrl+Shift+O", w.openEngagement)
	w.recentMenu = file.AddMenuWithTitle("Open &Recent")
	w.closeEngagement = w.addAction(file, "&Close Engagement", "", w.closeCurrentEngagement)
	file.AddSeparator()
	w.addStdAction(file, "&Quit", qt.QKeySequence__Quit, func() { w.win.Close() }).
		SetMenuRole(qt.QAction__QuitRole)

	edit := bar.AddMenuWithTitle("&Edit")
	w.addStdAction(edit, "&Copy", qt.QKeySequence__Copy, w.copy)
	w.addStdAction(edit, "Select &All", qt.QKeySequence__SelectAll, w.selectAll)
	edit.AddSeparator()
	w.addAction(edit, "C&lear Requests", "Ctrl+K", w.clearRequests)

	// The Tools menu is gone: Settings is on the navigation bar, Open Browser
	// is a button in the corner next to the interception toggle, and MCP runs
	// always-on (its settings are under the navigation bar's Settings). The
	// shortcuts those items had are kept as window-level actions.
	w.addWindowAction("Settings", "Ctrl+,", func() { w.openSettings(settingsGeneral) })
	w.addWindowAction("Open Browser", "Ctrl+Shift+B", w.openBrowser)
	w.interceptAction = w.addWindowAction("Intercept", "Ctrl+I", nil)
	w.interceptAction.SetCheckable(true)
	w.interceptAction.SetChecked(w.ctrl.Interceptor().Enabled())
	w.interceptAction.OnToggled(w.setIntercepting)
	w.addWindowAction("Send Request", "Ctrl+Return", w.sendReplay)
	w.addWindowAction("Close Replay Tab", "Ctrl+W", func() { w.replay.closeCurrent() })

	// The corner of the menu bar: the Autocrawl and Browser buttons and
	// the interception toggle.
	w.interceptToggle = newInterceptToggle(bar, w.interceptAction.IsChecked(), func() { w.interceptAction.Toggle() })
	bar.SetCornerWidget(w.buildMenuCorner())

	// The View menu is three sections: what the navigation bar looks like
	// and where it is, what the Sessions panel holds and where it is, and
	// the status bar. Everything that only concerns the request list is on
	// the Traffic panel's own header instead (its density, DNS and method
	// filters, and the search), with the shortcuts kept as window actions
	// below.
	view := bar.AddMenuWithTitle("&View")

	w.addMenuSection(view, "Navigation")
	w.navLabels = w.addAction(view, "Show &Names", "", nil)
	w.navLabels.SetCheckable(true)
	w.navLabels.SetToolTip("Label the navigation icons with their section names")
	w.navLabels.OnToggled(func(on bool) { w.nav.setLabels(on) })
	navPos := view.AddMenuWithTitle("&Position")
	navGroup := qt.NewQActionGroup(w.win.QObject)
	navGroup.SetExclusive(true)
	w.navSideActs = make(map[navSide]*qt.QAction, len(navSideOptions))
	for _, o := range navSideOptions {
		a := w.addAction(navPos, o.label, "", nil)
		a.SetCheckable(true)
		navGroup.AddAction(a)
		// Triggered, not toggled: the group unchecks the item being left,
		// and applyNavSide ticks one without asking for a move.
		a.OnTriggered(func() { w.chooseNavSide(o.value) })
		w.navSideActs[o.value] = a
	}

	w.addMenuSection(view, "Sessions")
	sessPos := view.AddMenuWithTitle("P&osition")
	sessGroup := qt.NewQActionGroup(w.win.QObject)
	sessGroup.SetExclusive(true)
	w.sessionsActs = make(map[sessionsSide]*qt.QAction, len(sessionsSideOptions))
	for _, o := range sessionsSideOptions {
		a := w.addAction(sessPos, o.label, "", nil)
		a.SetCheckable(true)
		a.SetIcon(paintedIcon(o.value.icon(), color(colorText), 16))
		sessGroup.AddAction(a)
		a.OnTriggered(func() { w.applySessionsSide(o.value) })
		w.sessionsActs[o.value] = a
	}
	// The panel's own kebab offers the same choice as a dev-tools-style row
	// of dock icons; it calls back here so the two can't disagree.
	w.sessions.onSide = w.applySessionsSide
	contents := view.AddMenuWithTitle("&Contents")
	w.sessionsTabActs = make(map[string]*qt.QAction, len(sessionsTabs))
	for _, t := range sessionsTabs {
		a := w.addAction(contents, t.label, "", nil)
		a.SetCheckable(true)
		a.SetChecked(true)
		a.OnToggled(func(on bool) { w.showSessionsTab(t.id, on) })
		w.sessionsTabActs[t.id] = a
	}

	view.AddSeparator()
	w.addToggle(view, panelStatusBar, "&Status Bar", "", w.win.StatusBar().QWidget)

	// Shortcuts the View menu no longer lists, since each has a control of
	// its own where it applies. The request list and the viewer are the
	// Traffic section's two halves; the viewer isn't a saved panel, as it
	// starts closed and opens on a click.
	w.addWindowToggle(panelRequests, "Requests", "Ctrl+1", w.listPanel)
	w.viewerAction = w.addWindowAction("Request/Response Viewer", "Ctrl+2", nil)
	w.viewerAction.SetCheckable(true)
	w.viewerAction.OnToggled(w.setDetailOpen)
	w.compactAction = w.addWindowAction("Compact List", "Ctrl+Shift+C", nil)
	w.compactAction.SetCheckable(true)
	w.compactAction.OnToggled(w.setCompact)
	w.addWindowAction("Find in Requests", "Ctrl+F", w.focusFilter)

	help := bar.AddMenuWithTitle("&Help")
	w.addAction(help, "&About reProxy", "", func() { openAbout(w.win.QWidget, w.ctrl.DataDir()) }).
		SetMenuRole(qt.QAction__AboutRole)
}

func (w *mainWindow) addAction(menu *qt.QMenu, text, shortcut string, fn func()) *qt.QAction {
	a := qt.NewQAction5(text, w.win.QObject)
	if shortcut != "" {
		a.SetShortcut(qt.NewQKeySequence2(shortcut))
	}
	if fn != nil {
		a.OnTriggered(fn)
	}
	menu.AddAction(a)
	return a
}

// addMenuSection titles a group of items in a menu. Qt's own addSection
// draws its text through the separator element, which the stylesheet
// collapses to a one-pixel rule, so the title is a label of our own carried
// in a widget action instead.
func (w *mainWindow) addMenuSection(menu *qt.QMenu, title string) {
	label := qt.NewQLabel3(title)
	setName(label.QObject, "MenuSection")
	a := qt.NewQWidgetAction(menu.QObject)
	a.SetDefaultWidget(label.QWidget)
	a.SetEnabled(false) // a title, not something to pick
	menu.AddAction(a.QAction)
}

func (w *mainWindow) addStdAction(menu *qt.QMenu, text string, key qt.QKeySequence__StandardKey, fn func()) *qt.QAction {
	a := w.addAction(menu, text, "", fn)
	a.SetShortcut(qt.NewQKeySequence6(key))
	return a
}

// addWindowAction adds an action to the window with a shortcut but no menu,
// for the shortcuts that used to live in the Tools menu.
func (w *mainWindow) addWindowAction(text, shortcut string, fn func()) *qt.QAction {
	a := qt.NewQAction5(text, w.win.QObject)
	if shortcut != "" {
		a.SetShortcut(qt.NewQKeySequence2(shortcut))
	}
	if fn != nil {
		a.OnTriggered(fn)
	}
	w.win.QWidget.AddAction(a)
	return a
}

// buildMenuCorner is the widget at the right of the menu bar: the
// Autocrawl and Browser buttons and the interception toggle.
func (w *mainWindow) buildMenuCorner() *qt.QWidget {
	corner := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(corner)
	h.SetContentsMargins(0, 0, 6, 0)
	h.SetSpacing(6)
	crawler := newIconButton(iconCrawler, "Autocrawl", w.openCrawl)
	crawler.w.SetAccessibleName("Autocrawl")
	h.AddWidget(crawler.w)
	browser := newIconButton(iconGlobe, "Open a browser pointed at the proxy (Ctrl+Shift+B)", w.openBrowser)
	browser.w.SetAccessibleName("Browser")
	h.AddWidget(browser.w)
	h.AddWidget(w.interceptToggle.w)
	return corner
}

// addToggle adds a checkable menu item that shows or hides panel. name
// identifies it in the saved layout.
func (w *mainWindow) addToggle(menu *qt.QMenu, name, text, shortcut string, panel *qt.QWidget) {
	w.registerToggle(w.addAction(menu, text, shortcut, nil), name, panel)
}

// addWindowToggle is addToggle without a menu item: the shortcut and the
// saved-layout entry alone, for a panel the View menu no longer lists.
func (w *mainWindow) addWindowToggle(name, text, shortcut string, panel *qt.QWidget) {
	w.registerToggle(w.addWindowAction(text, shortcut, nil), name, panel)
}

func (w *mainWindow) registerToggle(a *qt.QAction, name string, panel *qt.QWidget) {
	w.panelToggles[name] = a
	a.SetCheckable(true)
	a.SetChecked(true)
	a.OnToggled(func(on bool) { panel.SetVisible(on) })
}

func (w *mainWindow) openSettings(section string) {
	runSettingsDialog(w.win.QWidget, w.ctrl, section, liveSettings{
		theme: w.applyThemeLive,
		nav:   w.applyNavSide,
	})
	w.refreshStatus()
}

// openCrawl shows the Autocrawl window, raising it if it is open. It is a
// window of its own, not a dialog: the crawl runs while the user works.
func (w *mainWindow) openCrawl() {
	if w.crawl == nil {
		// Double-clicking a result shows its request here, in front.
		w.crawl = newCrawlWindow(w.win.QWidget, w.ctrl, func(id uint64) {
			w.showFlow(id)
			w.win.Raise()
			w.win.ActivateWindow()
		})
	}
	w.crawl.show()
}

// openPlugins shows the Plugins window, on its Plugins section.
func (w *mainWindow) openPlugins() {
	runPluginsWindow(w.win.QWidget, w.ctrl, pluginsSection)
	w.refreshStatus()
}

// applyThemeLive switches the colour theme and repaints the parts that aren't
// driven by the stylesheet alone: the custom-painted request list and
// navigation bar, the footer indicators (whose colours are baked into their
// text), and the syntax highlighting in every open editor.
func (w *mainWindow) applyThemeLive(kind themeKind) {
	setTheme(kind)
	w.list.Viewport().Update()
	w.nav.refreshTheme()
	w.interceptToggle.w.Update()
	w.refreshStatus()
	w.refreshMCPStatus()
	w.detail.refreshTheme()
	w.replay.refreshTheme()
	w.explorer.refreshTheme()
	w.files.refreshTheme()
	w.findings.refreshTheme()
	if w.agents != nil {
		w.agents.refreshTheme()
	}
	// The dock-position actions carry baked-in icons; the kebab's icon row
	// repaints itself, but these need re-rendering in the new ink.
	for side, a := range w.sessionsActs {
		a.SetIcon(paintedIcon(side.icon(), color(colorText), 16))
	}
}

// copy copies the selection in the focused editor, or the selected request's
// URL when the request list has focus.
func (w *mainWindow) copy() {
	if v := w.focusedPlainEditor(); v != nil {
		v.Copy()
		return
	}
	if le := w.focusedLineEditor(); le != nil {
		le.Copy()
		return
	}
	if f, ok := w.selectedFlow(); ok {
		qt.QGuiApplication_Clipboard().SetText(f.URL())
	}
}

func (w *mainWindow) selectAll() {
	if v := w.focusedPlainEditor(); v != nil {
		v.SelectAll()
		return
	}
	if le := w.focusedLineEditor(); le != nil {
		le.SelectAll()
		return
	}
	if w.list.HasFocus() {
		return // single-selection list; nothing to extend
	}
	// Nothing has focus: fall back to the body the section on screen shows.
	if w.nav.current == sectionReplay {
		if v := w.replay.currentView(); v != nil {
			v.request.body.edit.SelectAll()
		}
		return
	}
	w.detail.currentBody().SelectAll()
}

// focusedPlainEditor returns the body/raw editor that has focus, or nil.
func (w *mainWindow) focusedPlainEditor() *qt.QPlainTextEdit {
	focus := qt.QApplication_FocusWidget()
	if focus == nil {
		return nil
	}
	for _, v := range append(w.detail.plainEditors(), w.replay.plainEditors()...) {
		if focus.UnsafePointer() == v.QWidget.UnsafePointer() ||
			focus.UnsafePointer() == v.Viewport().UnsafePointer() {
			return v
		}
	}
	return nil
}

// focusedLineEditor returns the start-line or header field that has focus.
func (w *mainWindow) focusedLineEditor() *qt.QLineEdit {
	focus := qt.QApplication_FocusWidget()
	if focus == nil {
		return nil
	}
	for _, le := range append(w.detail.lineEditors(), w.replay.lineEditors()...) {
		if focus.UnsafePointer() == le.QWidget.UnsafePointer() {
			return le
		}
	}
	return nil
}

// ---- Body ----------------------------------------------------------------

// buildBody lays out the window: the navigation bar against one edge, and
// the section it selects filling the rest.
//
//	┌───┬─────────────────────────────┐
//	│ N │ ┌──────────┬──────────────┐ │   the Traffic section:
//	│ a │ │ Requests │ Req/Response │ │   top: horizontal splitter; the
//	│ v │ │          │  (slides out) │ │   viewer is closed until a click
//	│   │ ├──────────┴──────────────┤ │
//	│   │ │ Terminal | Agents       │ │   the Sessions panel: full width,
//	└───┴─┴─────────────────────────┴─┘   whichever section is on screen
//
// The Sessions panel sits beside the sections rather than inside one, so the
// agents and the shells stay available wherever the user navigates. It is
// pinned across the bottom by default and can go to any of the window's four
// edges, or into a window of its own (applySessionsSide): the splitter the
// two share is turned on its side for the left and the right. Every divider
// is draggable.
func (w *mainWindow) buildBody() *qt.QWidget {
	body := qt.NewQWidget2()
	w.bodyLayout = qt.NewQBoxLayout2(qt.QBoxLayout__LeftToRight, body)
	w.bodyLayout.SetContentsMargins(0, 0, 0, 0)
	w.bodyLayout.SetSpacing(0)

	w.nav = newNavBar(w.selectSection, w.openPlugins, func() { w.openSettings(settingsGeneral) })

	w.listPanel = w.buildListPanel()
	w.detailPanel = w.buildDetailPanel()
	w.sessions = w.buildSessionsPanel()

	w.top = qt.NewQSplitter3(qt.Horizontal)
	w.top.SetChildrenCollapsible(false)
	w.top.SetHandleWidth(windowMargin)
	w.top.AddWidget(w.listPanel)
	w.top.AddWidget(w.detailPanel)
	w.top.SetStretchFactor(0, listShare)
	w.top.SetStretchFactor(1, 100-listShare)
	// Sizes are proportions; the splitter scales them to the real width.
	w.top.SetSizes([]int{listShare * 10, (100 - listShare) * 10})
	// Closed to start with: the list has the full width.
	w.detailPanel.Hide()
	w.setListWide(true)
	w.detailAnim = qt.NewQVariantAnimation2(w.win.QObject)
	w.detailAnim.SetDuration(200)
	w.detailAnim.SetEasingCurve(qt.NewQEasingCurve3(qt.QEasingCurve__OutCubic))
	w.detailAnim.OnValueChanged(func(v *qt.QVariant) { w.sizeDetail(v.ToInt()) })
	w.detailAnim.OnFinished(w.detailSettled)

	w.outer = qt.NewQSplitter3(qt.Vertical)
	w.outer.SetChildrenCollapsible(false)
	w.outer.SetHandleWidth(windowMargin)
	w.outer.AddWidget(w.buildSections())
	w.dockSessions(sessionsBottom)

	// The navigation bar sits flush against the window edge; the content
	// keeps the window margin.
	content := qt.NewQWidget2()
	contentLayout := qt.NewQVBoxLayout(content)
	contentLayout.SetContentsMargins(windowMargin, windowMargin, windowMargin, windowMargin)
	contentLayout.AddWidget(w.outer.QWidget)

	w.bodyLayout.AddWidget(w.nav.root)
	w.bodyLayout.AddWidget2(content, 1)
	return body
}

// buildSections fills the stack the navigation bar switches between.
// Analyze is a placeholder, and is kept off the bar until it does
// something (navSection.hidden); its page is built with the rest.
func (w *mainWindow) buildSections() *qt.QWidget {
	w.sections = qt.NewQStackedWidget2()
	w.replay = newReplayPane(w.ctrl.ReplaySessions(), w.ctrl.Focus())
	w.targets = newTargetsPane(w.ctrl.Targets(), w.ctrl.Entities(), w.onTargetsChanged, w.showFlow,
		w.ctrl.SuppressHosts, func() (string, bool) {
			_, name, ok := w.ctrl.EngagementSettings()
			return name, ok
		})
	w.explorer = newExplorerPane(w.ctrl.Entities(), w.ctrl.Store(), w.ctrl.Targets(), func(f capture.Flow) {
		// From an Explorer requests window: bring the main window forward too.
		w.openReplay(f)
		w.win.Raise()
		w.win.ActivateWindow()
	})
	// Findings: the list, and the two places a finding is raised from a
	// message. The badge follows the store, so a finding an agent records
	// while the user is in another section announces itself.
	// Files: what the work recovered, from the same store the plugins, the
	// agents and the command line record into.
	w.files = newFilesPane(w.ctrl.Files(), w.showFlow)
	w.findings = newFindingsPane(w.ctrl.Findings(), w.showFlow, w.showRecoveredFile, w.refreshFindingsBadge)
	w.detail.onFinding = w.raiseFinding
	w.replay.onFinding = w.raiseFinding

	for _, s := range []struct {
		id   string
		page *qt.QWidget
	}{
		{sectionTargets, w.targets.root},
		{sectionTraffic, w.top.QWidget},
		{sectionReplay, w.replay.root},
		{sectionExplorer, w.explorer.root},
		{sectionFiles, w.files.root},
		{sectionFindings, w.findings.root},
		// Analyze has no item on the bar yet (navSections), but its page is
		// built and registered like the rest: nothing else has to know.
		{sectionAnalyze, newEmptyState("Analyze", "Nothing here yet.")},
	} {
		w.sectionIndex[s.id] = w.sections.AddWidget(s.page)
	}

	w.selectSection(sectionTraffic)
	return w.sections.QWidget
}

// selectSection shows one section and marks it current in the navigation
// bar, whether the user clicked the bar or the app switched sections itself
// (opening a request in Replay, say).
func (w *mainWindow) selectSection(id string) {
	i, ok := w.sectionIndex[id]
	if !ok {
		return
	}
	was := w.nav.current
	w.sections.SetCurrentIndex(i)
	w.nav.setCurrent(id)
	if was == id {
		return
	}
	// Tell the Replay pane when it gains or loses the screen, so the focus
	// tracker follows the user (see replayPane.setActive).
	if was == sectionReplay || id == sectionReplay {
		w.replay.setActive(id == sectionReplay)
	}
	// The Explorer is only rebuilt while it is on screen.
	if was == sectionExplorer || id == sectionExplorer {
		w.explorer.setActive(id == sectionExplorer)
	}
	// Likewise the Related list; and looking at it is what clears the
	// count of related hosts found.
	if was == sectionTargets || id == sectionTargets {
		w.targets.setActive(id == sectionTargets)
	}
	if id == sectionTargets {
		w.relatedUnseen = 0
		w.nav.setBadge(sectionTargets, 0)
	}
	// Coming to Traffic with messages held: put the oldest in front of the
	// user, since it is waiting on them.
	if id == sectionTraffic && w.model.held > 0 {
		w.openNextHeld()
	}
	// Held messages are a notification only while the user is elsewhere.
	w.refreshBadges()
}

// applyNavSide anchors the navigation bar to an edge of the window, or hides
// it. It is called at startup, from View → Navigation → Position, and
// whenever Settings → General changes it — which is why it ticks the menu's
// item itself rather than leaving that to whoever asked for the move.
func (w *mainWindow) applyNavSide(side navSide) {
	if a := w.navSideActs[side]; a != nil && !a.IsChecked() {
		a.SetChecked(true)
	}
	w.bodyLayout.RemoveWidget(w.nav.root)
	if side.vertical() {
		w.bodyLayout.SetDirection(qt.QBoxLayout__LeftToRight)
	} else {
		w.bodyLayout.SetDirection(qt.QBoxLayout__TopToBottom)
	}
	if side == navLeft || side == navTop {
		w.bodyLayout.InsertWidget(0, w.nav.root)
	} else {
		w.bodyLayout.AddWidget(w.nav.root)
	}
	w.nav.setSide(side)
}

// chooseNavSide moves the navigation bar from View → Navigation → Position
// and saves the choice. It is the same setting as Settings → General's
// anchor (config.Config.NavPosition), so there is one place it is kept and
// the two can't drift apart.
func (w *mainWindow) chooseNavSide(side navSide) {
	w.applyNavSide(side)
	cfg := w.ctrl.Settings()
	if navSideFor(cfg.NavPosition) == side {
		return
	}
	cfg.NavPosition = string(side)
	if err := w.ctrl.ApplySettings(cfg); err != nil {
		w.flash("Couldn't save where the navigation bar sits: " + err.Error())
	}
}

// buildSessionsPanel makes the Sessions panel and the two tabs it holds.
func (w *mainWindow) buildSessionsPanel() *sessionsPanel {
	w.agents = newAgentsPanel(w.ctrl, w.openReplaySessionByHandle)
	w.agents.setOpenSettings(func() { w.openSettings(settingsAgents) })
	w.terminals = newTerminalPanel(w.ctrl.Terminals())
	p := newSessionsPanel(map[string]*qt.QWidget{
		sessionsTerminal: w.terminals.root,
		sessionsAgents:   w.agents.root,
	})
	// Pinned to a side there is no room for a tab's two columns abreast, so
	// the panel tells its tabs to stack them when it gets thin.
	p.setStacker(func(stacked bool) {
		w.agents.setStacked(stacked)
		w.terminals.setStacked(stacked)
	})
	return p
}

// applySessionsSide pins the Sessions panel to one of the window's four
// edges, or floats it in a window of its own. It is called at startup, from
// View → Sessions → Position, from the panel's own kebab, and when the
// floating window is closed, so it ticks the menu's item itself.
func (w *mainWindow) applySessionsSide(side sessionsSide) {
	if a := w.sessionsActs[side]; a != nil && !a.IsChecked() {
		a.SetChecked(true)
	}
	w.sessions.markSide(side)
	if w.sessionsSide == sessionsWindow && side != sessionsWindow {
		w.sessionsGeom = w.sessionsWindowGeometry() // where to reopen it
	}
	w.sessionsSide = side
	if side.docked() {
		w.sessionsDock = side
		w.dockSessions(side)
	} else {
		w.floatSessions()
	}
	w.refreshSessions()
}

// dockSessions puts the panel at one of the four edges of the window: the
// splitter it shares with the sections is turned on its side for the left
// and the right, and the panel goes before or after them in it. The divider
// is re-proportioned every time, since moving the panel across the window
// is exactly when its old size is worth nothing — and a height is no use as
// a width anyway.
func (w *mainWindow) dockSessions(side sessionsSide) {
	share := bottomShare
	if side.beside() {
		w.outer.SetOrientation(qt.Horizontal)
		share = sideShare
	} else {
		w.outer.SetOrientation(qt.Vertical)
	}
	if side.first() {
		w.outer.InsertWidget(0, w.sessions.root)
		w.outer.SetStretchFactor(0, share)
		w.outer.SetStretchFactor(1, 100-share)
		w.outer.SetSizes([]int{share * 10, (100 - share) * 10})
	} else {
		w.outer.AddWidget(w.sessions.root)
		w.outer.SetStretchFactor(0, 100-share)
		w.outer.SetStretchFactor(1, share)
		w.outer.SetSizes([]int{(100 - share) * 10, share * 10})
	}
	if w.sessionsWin != nil {
		w.sessionsWin.Hide()
	}
}

// floatSessions moves the panel into a window of its own, making the window
// the first time it is needed. Adding the panel to the window's layout takes
// it out of the splitter, which closes the gap it left.
func (w *mainWindow) floatSessions() {
	if w.sessionsWin == nil {
		w.sessionsWin = w.newSessionsWindow()
	}
	w.sessionsWin.Layout().AddWidget(w.sessions.root)
	if len(w.sessionsGeom) > 0 {
		w.sessionsWin.RestoreGeometry(w.sessionsGeom)
	}
}

// newSessionsWindow builds the window the Sessions panel floats in: a window
// of its own, but the main window's child, so it is closed with it and never
// keeps reproxy running on its own (as the Autocrawl window is).
func (w *mainWindow) newSessionsWindow() *qt.QWidget {
	win := qt.NewQWidget(w.win.QWidget)
	win.SetWindowTitle("Sessions")
	win.SetWindowFlags(qt.Window)
	win.SetAttribute2(qt.WA_QuitOnClose, false)
	v := qt.NewQVBoxLayout(win)
	v.SetContentsMargins(windowMargin, windowMargin, windowMargin, windowMargin)
	width, height := 980, 420
	if scr := qt.QGuiApplication_PrimaryScreen(); scr != nil {
		avail := scr.AvailableGeometry()
		width, height = min(width, avail.Width()-40), min(height, avail.Height()-60)
	}
	win.Resize(width, height)
	win.SetMinimumSize2(min(520, width), min(220, height))
	// Closing the window pins the panel back to the edge it was last at,
	// rather than leaving it nowhere with only the View menu to find it
	// again. On the way out of the app it is left alone: closeOtherWindows
	// closes it, and re-docking a panel then would be pointless work on a
	// layout about to be saved.
	win.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		super(e)
		if w.quitting || w.sessionsSide != sessionsWindow {
			return
		}
		w.sessionsGeom = win.SaveGeometry()
		w.applySessionsSide(w.sessionsDock)
	})
	return win
}

// sessionsWindowGeometry is the floating window's geometry, or the last one
// saved while it isn't on screen.
func (w *mainWindow) sessionsWindowGeometry() []byte {
	if w.sessionsWin != nil && w.sessionsSide == sessionsWindow {
		return w.sessionsWin.SaveGeometry()
	}
	return w.sessionsGeom
}

// showSessionsTab puts one of the panel's tabs in or takes it out, from
// View → Sessions → Contents.
func (w *mainWindow) showSessionsTab(id string, on bool) {
	w.sessions.setShown(id, on)
	w.refreshSessions()
}

// refreshSessions hides the panel once nothing is left in it, which is how
// Contents turns it off altogether, and brings it back when a tab is ticked
// on again.
func (w *mainWindow) refreshSessions() {
	shown := !w.sessions.empty()
	w.sessions.root.SetVisible(shown)
	if w.sessionsWin != nil {
		w.sessionsWin.SetVisible(shown && w.sessionsSide == sessionsWindow)
		if shown && w.sessionsSide == sessionsWindow {
			w.sessionsWin.Raise()
		}
	}
}

// targetLabel resolves a flow's target for the request viewer's tag.
func (w *mainWindow) targetLabel(id string) (string, string, bool) {
	t, ok := w.ctrl.Targets().Get(id)
	if !ok {
		return "", "", false
	}
	return t.Label(), t.Color, true
}

// targetColor is the flow list's lookup for a row's colour square. A flow
// whose target has been deleted gets no square rather than a stale one.
func (w *mainWindow) targetColor(id string) string {
	t, ok := w.ctrl.Targets().Get(id)
	if !ok {
		return ""
	}
	return t.Color
}

// onTargetsChanged repaints everything that shows a target's label or
// colour, after one is added, edited, recoloured or removed — including when
// a whole new set arrives with another engagement.
func (w *mainWindow) onTargetsChanged() {
	w.list.Viewport().Update()
	w.detail.refreshTarget()
	w.explorer.targetsChanged()
}

// refreshBadges updates the navigation bar's notification counts. Traffic
// badges the messages being held, so held traffic is noticed while the user
// is working in another section; it isn't a count of all traffic, and it
// clears while Traffic is the section on screen.
func (w *mainWindow) refreshBadges() {
	held := w.model.held
	if w.nav.current == sectionTraffic {
		held = 0
	}
	w.nav.setBadge(sectionTraffic, held)
}

func (w *mainWindow) buildListPanel() *qt.QWidget {
	w.list = qt.NewQListView2()
	w.list.SetModel(w.model.QAbstractItemModel)
	w.list.SetItemDelegate(newFlowDelegate(w.model, w.list).QAbstractItemDelegate)
	w.list.SetUniformItemSizes(true)
	w.list.SetMouseTracking(true)
	w.list.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	w.list.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	w.list.SetMinimumWidth(260)

	w.list.SelectionModel().OnCurrentChanged(func(current, _ *qt.QModelIndex) {
		if f, ok := w.model.at(current.Row()); ok {
			w.detail.show(f)
			w.ctrl.Focus().SelectFlow(f.ID)
		} else {
			w.detail.clear()
			w.ctrl.Focus().SelectFlow(0)
		}
	})
	// Clicking a request (even the selected one) or pressing Enter on it
	// opens the viewer; Esc closes it again.
	w.list.OnClicked(func(*qt.QModelIndex) { w.setDetailOpen(true) })
	// Right-click: the actions plugins offer for the request (flowactions.go).
	w.list.SetContextMenuPolicy(qt.CustomContextMenu)
	w.list.OnCustomContextMenuRequested(w.showFlowMenu)
	w.list.OnActivated(func(*qt.QModelIndex) { w.setDetailOpen(true) })
	w.list.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if e.Key() == int(qt.Key_Escape) && w.detailOpen {
			w.setDetailOpen(false)
			return
		}
		super(e)
	})

	// Drives the fade of row flashes; runs only while a flash is active.
	w.pulseTimer = qt.NewQTimer()
	w.pulseTimer.SetInterval(30)
	w.pulseTimer.OnTimeout(func() {
		now := time.Now()
		for id, ps := range w.model.pulses {
			if now.Sub(ps.start) >= pulseDuration {
				delete(w.model.pulses, id)
			}
		}
		w.list.Viewport().Update()
		if len(w.model.pulses) == 0 {
			w.pulseTimer.Stop()
		}
	})

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)

	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	// No right/vertical padding: the search button sits flush in the corner and
	// spans the full header height. The title stays vertically centred.
	h.SetContentsMargins(rowPadX, 0, 0, 0)
	h.SetSpacing(8)
	title := qt.NewQLabel3("Traffic")
	setName(title.QObject, "PanelTitle")
	// The request count lives in the footer, not here.
	h.AddWidget(title.QWidget)
	h.AddStretch()
	headerH := title.SizeHint().Height() + 20
	header.SetFixedHeight(headerH)
	// These decide what the list holds and how it is drawn, so they sit in
	// the header rather than in the filter bar: none of them should be
	// reachable only through the search.
	h.AddWidget3(w.buildDensityToggle(), 0, qt.AlignVCenter)
	h.AddWidget3(w.buildDNSToggle().QWidget, 0, qt.AlignVCenter)
	h.AddWidget3(w.buildMethodFilter(), 0, qt.AlignVCenter)
	h.AddSpacing(rowPadX - 8) // the layout's own 8px, plus the header's gutter
	h.AddWidget(w.buildSearchButton(headerH))

	w.filterBar = w.buildFilterBar()
	w.flowHeader = newFlowHeader(w.list)

	// With nothing captured yet, the hint (how to point a browser at the
	// proxy) takes the list's place.
	w.listPages = qt.NewQStackedWidget2()
	hintPage := qt.NewQWidget2()
	hv := qt.NewQVBoxLayout(hintPage)
	hv.AddStretch()
	hintTitle := qt.NewQLabel3("No traffic yet")
	setName(hintTitle.QObject, "EmptyTitle")
	hintTitle.SetAlignment(qt.AlignCenter)
	w.listHint = qt.NewQLabel2()
	setName(w.listHint.QObject, "EmptyState")
	w.listHint.SetAlignment(qt.AlignCenter)
	w.listHint.SetWordWrap(true)
	hv.AddWidget(hintTitle.QWidget)
	hv.AddSpacing(6)
	hv.AddWidget(w.listHint.QWidget)
	hv.AddStretch()
	w.listPages.AddWidget(hintPage)
	w.listPages.AddWidget(w.list.QWidget)

	v.AddWidget(header)
	v.AddWidget(w.filterBar)
	v.AddWidget(w.flowHeader)
	v.AddWidget(w.listPages.QWidget)
	return panel.QWidget
}

// buildDensityToggle is the header's density button: it packs the requests
// onto single lines, or gives them room again. Like a play/pause button it
// shows the density it would switch to, so the two are never both on screen;
// the Ctrl+Shift+C shortcut and the auto-switch past compactThreshold go
// through the same setCompact, which keeps the button in step.
func (w *mainWindow) buildDensityToggle() *qt.QWidget {
	w.densityBtn = newIconButton(iconRowsTight, "", func() { w.setCompact(!w.model.compact) })
	w.syncDensityToggle()
	return w.densityBtn.w
}

// syncDensityToggle puts the density the button offers on its icon and tip.
func (w *mainWindow) syncDensityToggle() {
	if w.densityBtn == nil {
		return
	}
	if w.model.compact {
		w.densityBtn.setIcon(iconRowsLoose)
		w.densityBtn.w.SetToolTip("Give the requests more room (Ctrl+Shift+C)")
		return
	}
	w.densityBtn.setIcon(iconRowsTight)
	w.densityBtn.w.SetToolTip("Pack the requests onto single lines (Ctrl+Shift+C)")
}

// buildDNSToggle is the header's DNS pill: it lists the name lookups made
// for the requests beside them, or hides them again.
func (w *mainWindow) buildDNSToggle() *qt.QToolButton {
	w.dnsBtn = qt.NewQToolButton2()
	w.dnsBtn.SetText("DNS")
	w.dnsBtn.SetCheckable(true)
	w.dnsBtn.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	w.dnsBtn.SetFocusPolicy(qt.NoFocus)
	w.dnsBtn.SetToolTip("Show DNS lookups in the list (they are hidden by default; a held one always shows)")
	setName(w.dnsBtn.QObject, "HeaderToggle")
	w.dnsBtn.OnToggled(w.setShowDNS)
	return w.dnsBtn
}

// buildMethodFilter is the header's funnel: the request methods the list
// shows, ticked on and off one at a time or all at once. A lookup is not a
// request, so DNS keeps the toggle of its own beside it, and a held message
// is listed whatever its method — nothing may wait on the user unseen.
func (w *mainWindow) buildMethodFilter() *qt.QWidget {
	entries := make([]filterEntry, 0, len(listedMethods))
	for _, id := range listedMethods {
		label := id
		if id == methodOther {
			label = "Other"
		}
		entries = append(entries, filterEntry{id: id, label: label})
	}
	w.methodDebounce = qt.NewQTimer()
	w.methodDebounce.SetSingleShot(true)
	w.methodDebounce.SetInterval(0) // the end of this turn of the event loop
	w.methodDebounce.OnTimeout(w.applyMethodFilter)

	w.methodFilter = newIconCheckFilter(iconFilter, "Filter by request method", entries,
		func(string, bool) { w.methodDebounce.Start(0) },
		func(hidden int) string {
			switch {
			case hidden == 0:
				return "every method"
			case hidden == len(entries):
				return "no methods"
			default:
				return fmt.Sprintf("%d of %d methods", len(entries)-hidden, len(entries))
			}
		})
	// The counts are only read while the menu is open, so they are worked
	// out as it drops rather than on every request that arrives.
	w.methodFilter.onOpen(func() { w.methodFilter.setCounts(w.model.methodCount) })
	return w.methodFilter.widget()
}

// applyMethodFilter puts the funnel's choice on the list.
func (w *mainWindow) applyMethodFilter() {
	hidden := make(map[string]bool, len(listedMethods))
	for _, id := range w.methodFilter.hiddenIDs() {
		hidden[id] = true
	}
	if !w.model.setHiddenMethods(hidden) {
		return
	}
	w.applyFilter()
	w.refreshCounts()
}

// buildSearchButton is the header's filter toggle: a painted, full-height
// button flush in the corner, bordered in the header's own border colour so it
// reads as part of the header. Clicking it slides the filter bar in or out.
func (w *mainWindow) buildSearchButton(height int) *qt.QWidget {
	b := qt.NewQWidget2()
	b.SetFixedSize2(height, height) // square, full header height
	b.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	b.SetToolTip("Filter requests (Ctrl+F)")
	b.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) {
		p := qt.NewQPainter2(b.QPaintDevice)
		defer p.End()
		p.SetRenderHint(qt.QPainter__Antialiasing)
		bw, bh := b.Width(), b.Height()
		switch {
		case w.searchOpen:
			p.FillRect5(0, 0, bw, bh, color(colorSelected))
		case w.searchHover:
			p.FillRect5(0, 0, bw, bh, color(colorHover))
		}
		// A left edge in the header's border colour, so it looks built in.
		p.FillRect5(0, 0, 1, bh, color("borderSubtle"))
		ink := colorMuted
		if w.searchOpen || w.searchHover {
			ink = colorText
		}
		const sz = 18.0
		drawIcon(p, iconSearch, (float64(bw)-sz)/2, (float64(bh)-sz)/2, sz, color(ink))
	})
	b.OnEnterEvent(func(_ func(*qt.QEnterEvent), _ *qt.QEnterEvent) { w.searchHover = true; b.Update() })
	b.OnLeaveEvent(func(_ func(*qt.QEvent), _ *qt.QEvent) { w.searchHover = false; b.Update() })
	b.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) {
		if e.Button() == qt.LeftButton {
			w.toggleFilter(!w.searchOpen)
		}
	})
	w.searchBtn = b
	return b
}

// buildFilterBar is the row above the request list: a text field with a
// VS Code-style ".*" regex toggle attached to its right, inside one bordered
// control. It is hidden (collapsed to zero height) until the search button
// slides it out.
func (w *mainWindow) buildFilterBar() *qt.QWidget {
	bar := qt.NewQWidget2()
	setName(bar.QObject, "FilterBar")
	bar.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(bar)
	h.SetContentsMargins(rowPadX, 6, rowPadX, 6)
	h.SetSpacing(6)

	// The field and the toggle share one bordered box, so the toggle reads as
	// part of the input.
	box := qt.NewQFrame2()
	setName(box.QObject, "FilterInputBox")
	box.SetAttribute(qt.WA_StyledBackground)
	w.filterBox = box
	bl := qt.NewQHBoxLayout(box.QWidget)
	bl.SetContentsMargins(0, 0, 0, 0)
	bl.SetSpacing(0)

	w.filterEdit = qt.NewQLineEdit2()
	w.filterEdit.SetPlaceholderText("Filter requests…")
	w.filterEdit.SetClearButtonEnabled(true)
	setName(w.filterEdit.QObject, "FilterEdit")
	w.filterEdit.OnTextChanged(func(string) { w.applyFilter() })

	w.regexBtn = qt.NewQToolButton2()
	w.regexBtn.SetText(".*")
	w.regexBtn.SetCheckable(true)
	w.regexBtn.SetFocusPolicy(qt.NoFocus)
	w.regexBtn.SetToolTip("Use regular expression")
	setName(w.regexBtn.QObject, "RegexToggle")
	w.regexBtn.OnToggled(func(bool) { w.applyFilter() })

	bl.AddWidget2(w.filterEdit.QWidget, 1)
	bl.AddWidget(w.regexBtn.QWidget)
	h.AddWidget2(box.QWidget, 1)

	// Collapsed by default; the search button animates it open.
	w.filterBarH = bar.SizeHint().Height()
	bar.SetMinimumHeight(0)
	bar.SetMaximumHeight(0)
	w.filterAnim = qt.NewQPropertyAnimation2(bar.QObject, []byte("maximumHeight"))
	w.filterAnim.SetDuration(150)
	return bar
}

// toggleFilter slides the filter bar out (open) or in (closed). Closing it
// also clears any active filter, so hidden filtering never silently hides rows.
func (w *mainWindow) toggleFilter(open bool) {
	if open == w.searchOpen {
		return
	}
	w.searchOpen = open
	from := w.filterBar.MaximumHeight()
	to := 0
	if open {
		// Recompute from the now-styled size so padding/border are included.
		w.filterBarH = w.filterBar.SizeHint().Height()
		to = w.filterBarH
	}
	w.filterAnim.Stop()
	w.filterAnim.SetStartValue(qt.NewQVariant4(from))
	w.filterAnim.SetEndValue(qt.NewQVariant4(to))
	w.filterAnim.Start()
	if w.searchBtn != nil {
		w.searchBtn.Update()
	}
	if open {
		w.filterEdit.SetFocus()
		w.filterEdit.SelectAll()
	} else if w.filterEdit.Text() != "" {
		w.filterEdit.Clear() // clears the filter via OnTextChanged
	}
}

// focusFilter opens the filter bar (if closed) and puts the cursor in it, so
// Ctrl+F starts a fresh filter or refines the current one. In Replay, or
// with the keyboard in the request/response viewer, Ctrl+F searches the
// messages on show instead.
func (w *mainWindow) focusFilter() {
	if w.replay.active {
		if v := w.replay.currentView(); v != nil {
			v.search.open()
		}
		return
	}
	if fw := qt.QApplication_FocusWidget(); fw != nil && w.detail.root.IsAncestorOf(fw) {
		w.detail.search.open()
		return
	}
	if !w.searchOpen {
		w.toggleFilter(true)
		return // toggleFilter focuses and selects on open
	}
	w.filterEdit.SetFocus()
	w.filterEdit.SelectAll()
}

// setShowDNS lists DNS lookups in the request list, or hides them.
func (w *mainWindow) setShowDNS(on bool) {
	w.dnsBtn.SetChecked(on) // no signal when already set, so no loop
	if w.model.showDNS == on {
		return
	}
	w.model.showDNS = on
	w.applyFilter()
	w.refreshCounts()
}

// applyFilter recompiles the matcher from the filter bar and applies it,
// preserving the selected request when it survives the filter. An invalid
// regex marks the field and matches nothing, as in VS Code.
func (w *mainWindow) applyFilter() {
	text := w.filterEdit.Text()
	useRegex := w.regexBtn.IsChecked()
	matcher, err := filter.Compile(text, useRegex)
	w.setFilterInvalid(err != nil)
	if err != nil {
		w.filterEdit.SetToolTip("Invalid regular expression: " + err.Error())
		matcher = filter.MatchNothing()
	} else {
		w.filterEdit.SetToolTip("")
	}

	selected, hadSelection := w.selectedFlow()
	w.model.setFilter(matcher)
	if hadSelection {
		if row, ok := w.model.rowOf(selected.ID); ok {
			w.selectRow(row)
		} else {
			w.detail.clear()
		}
	}
	w.refreshCounts()
}

// setFilterInvalid toggles the error styling on the filter box.
func (w *mainWindow) setFilterInvalid(invalid bool) {
	w.filterBox.SetProperty("invalid", qt.NewQVariant8(invalid))
	// Re-polish so the stylesheet picks up the property change.
	w.filterBox.Style().Unpolish(w.filterBox.QWidget)
	w.filterBox.Style().Polish(w.filterBox.QWidget)
}

// setListWide switches the request list between its full-width layout
// (columns under a header) and the narrow two-line rows used beside the
// viewer.
func (w *mainWindow) setListWide(wide bool) {
	w.model.wide = wide
	w.syncFlowHeader()
	w.list.DoItemsLayout()
	w.list.Viewport().Update()
}

// syncFlowHeader shows the column header only over a full-width list that
// has rows; the no-traffic hint stands alone.
func (w *mainWindow) syncFlowHeader() {
	w.flowHeader.SetVisible(w.model.wide && len(w.model.all) > 0)
}

// setDetailOpen slides the request/response viewer out from the right (the
// list narrowing to make room) or back in (the list taking the full width
// again). The Ctrl+2 action follows it.
func (w *mainWindow) setDetailOpen(open bool) {
	if open == w.detailOpen {
		return
	}
	w.detailOpen = open
	w.viewerAction.SetChecked(open) // re-enters here and returns at once

	avail := w.top.Width() - w.top.HandleWidth()
	from, to := 0, 0
	if open {
		to = w.detailWidth
		if to <= 0 {
			to = avail * (100 - listShare) / 100
		}
		// Leave the list room for its narrow rows, and the viewer room for
		// its header.
		to = max(min(to, avail-w.list.MinimumWidth()-2), min(400, avail/2))
		w.setListWide(false)
		w.detailPanel.SetMinimumWidth(0) // so it can start from nothing
		w.sizeDetail(0)
		w.detailPanel.Show()
	} else {
		from = w.currentDetailWidth()
		w.detailWidth = from
		w.detailPanel.SetMinimumWidth(0)
	}
	w.detailAnim.Stop()
	w.detailAnim.SetStartValue(qt.NewQVariant4(from))
	w.detailAnim.SetEndValue(qt.NewQVariant4(to))
	w.detailAnim.Start()
}

// sizeDetail gives the viewer width pixels and the list the rest.
func (w *mainWindow) sizeDetail(width int) {
	avail := w.top.Width() - w.top.HandleWidth()
	w.top.SetSizes([]int{max(avail-width, 0), width})
}

// detailSettled finishes a slide: an open viewer gets its minimum width back
// (so the divider can't squeeze it away), a closed one is hidden and the list
// goes back to full width.
func (w *mainWindow) detailSettled() {
	if w.detailOpen {
		w.detailPanel.SetMinimumWidth(400)
		return
	}
	w.detailPanel.Hide()
	w.setListWide(true)
	if row := w.list.CurrentIndex().Row(); row >= 0 {
		w.selectRow(row) // keep the selected request in view at the new row height
	}
}

// currentDetailWidth is the viewer's width while it is open, or the width it
// will reopen at while it is closed.
func (w *mainWindow) currentDetailWidth() int {
	if w.detailOpen && w.detailPanel.IsVisible() {
		if sizes := w.top.Sizes(); len(sizes) == 2 && sizes[1] > 0 {
			return sizes[1]
		}
	}
	return w.detailWidth
}

// setCompact switches the request list between comfortable and compact rows
// and relays out the view. It is idempotent and safe to call from the
// header's button, the shortcut or the auto-switch, each of which it keeps
// in step with the others.
func (w *mainWindow) setCompact(on bool) {
	if w.compactAction != nil && w.compactAction.IsChecked() != on {
		w.compactAction.SetChecked(on) // re-enters here and does the work
	}
	if w.model.compact == on {
		return
	}
	w.model.compact = on
	w.syncDensityToggle()
	// Re-query item sizes for the new row height.
	w.list.DoItemsLayout()
	w.list.Viewport().Update()
}

func (w *mainWindow) buildDetailPanel() *qt.QWidget {
	w.detail = newDetailPane()
	w.detail.onForward = func(id uint64, edited string) {
		held := w.detail.shownState // the held state, before the store releases it
		if err := w.ctrl.Interceptor().Forward(id, &edited); err == nil {
			w.flash(forwardMessage(held))
			w.pulseRow(id, colorSuccess)
		}
	}
	w.detail.onDrop = func(id uint64) {
		if err := w.ctrl.Interceptor().Drop(id); err == nil {
			w.flash("Dropped — connection closed")
			w.pulseRow(id, colorServerError)
		}
	}
	w.detail.onReplay = w.openReplay
	w.detail.onClose = func() { w.setDetailOpen(false) }
	w.detail.targetOf = w.targetLabel

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.AddWidget(w.detail.root.QWidget)
	panel.SetMinimumWidth(400)
	return panel.QWidget
}

func (w *mainWindow) selectedFlow() (capture.Flow, bool) {
	return w.model.at(w.list.CurrentIndex().Row())
}

// openReplay starts a replay session templated from f and opens a tab on it
// in the Replay section.
func (w *mainWindow) openReplay(f capture.Flow) {
	sess := w.ctrl.ReplaySessions().Create(replay.OwnerUser, replay.TemplateFrom(f))
	w.showReplay(sess)
}

// openReplaySession opens (or selects) a tab on an existing session, such as
// one an agent created.
func (w *mainWindow) openReplaySession(id uint64) bool {
	sess, ok := w.ctrl.ReplaySessions().Get(id)
	if !ok {
		return false
	}
	w.showReplay(sess)
	return true
}

// showReplay opens a tab on sess and brings the Replay section forward.
func (w *mainWindow) showReplay(sess replay.Session) {
	w.selectSection(sectionReplay)
	w.replay.open(sess)
}

// openReplaySessionByHandle opens a Replay tab on a session named by an
// agent's "<instance>.r<id>" handle. Only sessions in this process open
// directly; a handle for another instance's session is ignored (its own
// window shows it).
func (w *mainWindow) openReplaySessionByHandle(handle string) {
	inst, id, ok := parseReplayHandle(handle)
	if !ok || inst != "" {
		return
	}
	w.openReplaySession(id)
}

// sendReplay sends the request in the open Replay tab (Ctrl+Enter). It does
// nothing unless the Replay section is the one on screen, so the shortcut
// stays out of the way while editing a held message.
func (w *mainWindow) sendReplay() {
	if w.nav.current != sectionReplay {
		return
	}
	if v := w.replay.currentView(); v != nil {
		w.replay.clearMark()
		v.doSend()
	}
}

// flash shows a transient message in the status line, then restores the normal
// proxy status after a moment. Used to confirm actions that otherwise only
// change a row. It replaces only the central status label, so the intercept
// indicator and request count stay put (QStatusBar.showMessage overlaps our
// custom widgets instead of hiding them, so we drive our own label).
func (w *mainWindow) flash(msg string) {
	w.status.SetText(msg)
	if w.flashTimer == nil {
		w.flashTimer = qt.NewQTimer()
		w.flashTimer.SetSingleShot(true)
		w.flashTimer.OnTimeout(func() { w.refreshStatus() })
	}
	w.flashTimer.Start(2600)
}

// pulseRow briefly tints a flow's row to confirm an action on it.
func (w *mainWindow) pulseRow(id uint64, hex string) {
	w.model.pulses[id] = pulseState{start: time.Now(), hex: hex}
	if !w.pulseTimer.IsActive() {
		w.pulseTimer.Start2()
	}
	w.list.Viewport().Update()
}

// forwardMessage names what was just forwarded, for the status flash.
func forwardMessage(held capture.FlowState) string {
	if held == capture.StateHeldResponse {
		return "Response forwarded"
	}
	return "Request forwarded"
}

func (w *mainWindow) selectRow(row int) {
	idx := w.model.Index(row, 0, qt.NewQModelIndex())
	w.list.SetCurrentIndex(idx)
	w.list.ScrollTo(idx, qt.QAbstractItemView__EnsureVisible)
}

// ---- Findings ------------------------------------------------------------

// raiseFinding opens the Create Finding window on a draft raised from a
// message — in the Traffic viewer or in a Replay tab. The draft already
// carries the selected text and the request it came from; the user adds the
// types, notes and details.
//
// Recording a value that is already a finding adds a sighting to it rather
// than making a second one (the dialog says so), so flagging the same token
// on a dozen endpoints builds one finding with a dozen sightings.
func (w *mainWindow) raiseFinding(draft findings.Draft) {
	f, ok := runFindingDialog(w.win.QWidget, w.ctrl.Findings(), draft, nil)
	if !ok {
		return
	}
	w.flash("Finding recorded: " + f.SummaryMasked())
}

// showFlow brings a request into view in the Traffic section: the Findings
// pane's way of getting from a finding back to the traffic it came from. A
// finding outlives its traffic, so the request may be gone.
func (w *mainWindow) showFlow(id uint64) {
	row, ok := w.model.rowOf(id)
	if !ok {
		w.flash("That request is no longer in the list.")
		return
	}
	w.selectSection(sectionTraffic)
	w.selectRow(row)
	w.setDetailOpen(true)
}

// showRecoveredFile brings a recovered file into view in the Files section,
// for a finding's recovered-file link.
func (w *mainWindow) showRecoveredFile(path string) {
	w.selectSection(sectionFiles)
	if !w.files.revealPath(path) {
		w.flash("That recovered file is no longer here.")
	}
}

// onEntityEvent follows the entities store: the Explorer and the Related
// list are redrawn when next on screen, and a newly related host counts on
// the Targets badge until the user goes to look.
func (w *mainWindow) onEntityEvent(e entities.Event) {
	switch e.Kind {
	case entities.RelatedAdded:
		if w.nav.current != sectionTargets {
			w.relatedUnseen++
			w.nav.setBadge(sectionTargets, w.relatedUnseen)
		}
	case entities.Reset, entities.Cleared:
		// Another engagement (or an emptied Explorer): its related hosts aren't
		// news, and the old ones are gone.
		w.relatedUnseen = 0
		w.nav.setBadge(sectionTargets, 0)
	}
	w.targets.related.changed()
	// RelatedAdded only names a host, and changes nothing the Explorer draws.
	// RelatedChanged does: the suppression list (or the targets) decides
	// which hosts are left out of it.
	if e.Kind != entities.RelatedAdded {
		w.explorer.storeChanged()
	}
}

// refreshFindingsBadge keeps the navigation bar's Findings count equal to
// the number of findings the user hasn't opened.
func (w *mainWindow) refreshFindingsBadge() {
	w.nav.setBadge(sectionFindings, w.ctrl.Findings().Unviewed())
}

// ---- Browser -------------------------------------------------------------

// openBrowser launches the configured browser pointed at the proxy. A
// browser that dies straight away (no display, broken install) is reported
// too, not just one that can't be started.
func (w *mainWindow) openBrowser() {
	s, err := w.ctrl.LaunchBrowser()
	if err != nil {
		w.browserError(err)
		return
	}
	w.flash("Opened " + s.Browser.Name + ", using the proxy")
	if warn := s.Warning(); warn != "" {
		// The browser launched, but HTTPS interception won't be trusted.
		qt.QMessageBox_Warning(w.win.QWidget, "HTTPS may show warnings",
			sentence(warn)+"\n\nHTTPS sites will show certificate warnings until the "+
				"proxy CA is trusted. You can download it from Settings → Traffic Proxy.")
	}
	go func() {
		if err := s.FailedEarly(10 * time.Second); err != nil {
			mainthread.Start(func() { w.browserError(fmt.Errorf("%s exited: %w", s.Browser.Name, err)) })
		}
	}()
}

func (w *mainWindow) browserError(err error) {
	qt.QMessageBox_Warning(w.win.QWidget, "Couldn't open browser",
		sentence(err.Error())+"\n\nChoose a browser in Settings → Browser.")
}

// sentence capitalises a Go-style error message for display on its own.
func sentence(msg string) string {
	r, size := utf8.DecodeRuneInString(msg)
	if r == utf8.RuneError {
		return msg
	}
	return string(unicode.ToUpper(r)) + msg[size:]
}

// ---- Interception --------------------------------------------------------

func (w *mainWindow) setIntercepting(on bool) {
	// Turning it off forwards everything held; store events update the list.
	w.ctrl.Interceptor().SetEnabled(on)
	w.interceptToggle.setOn(on)
	w.refreshStatus()
	if on {
		w.flash("Interception on")
	} else {
		w.flash("Interception off — held traffic forwarded")
	}
}

// openNextHeld shows the oldest held message, so held messages are worked
// through in the order they arrived. While Traffic is on screen the viewer
// slides out with it; elsewhere it is only selected, and selectSection opens
// the viewer when the user comes back to Traffic.
func (w *mainWindow) openNextHeld() {
	if row := w.model.firstHeld(); row >= 0 {
		w.selectRow(row)
		if w.nav.current == sectionTraffic {
			w.setDetailOpen(true)
		}
	}
}

// ---- Status bar ----------------------------------------------------------

func (w *mainWindow) buildStatusBar() {
	sb := w.win.StatusBar()
	sb.SetSizeGripEnabled(false)
	w.interceptStatus = qt.NewQLabel2()
	w.interceptStatus.SetContentsMargins(6, 2, 10, 2)
	w.status = qt.NewQLabel2()
	w.status.SetContentsMargins(6, 2, 6, 2)
	w.mcpStatus = qt.NewQLabel2()
	w.mcpStatus.SetContentsMargins(6, 2, 10, 2)
	w.mcpStatus.SetToolTip("MCP server status")
	w.upstreamStatus = qt.NewQLabel2()
	w.upstreamStatus.SetContentsMargins(6, 2, 10, 2)
	w.total = qt.NewQLabel2()
	w.total.SetContentsMargins(6, 2, 6, 2)
	sb.AddWidget(w.interceptStatus.QWidget)
	sb.AddWidget2(w.status.QWidget, 1)
	sb.AddPermanentWidget(w.upstreamStatus.QWidget)
	sb.AddPermanentWidget(w.mcpStatus.QWidget)
	sb.AddPermanentWidget(w.total.QWidget)

	// The MCP server runs on the hub process and can change from elsewhere,
	// so poll its status for the footer. Cheap: a local call, or one short
	// loopback request when this process isn't the hub. The upstream
	// indicator rides along: opening an engagement can change the chain.
	w.refreshMCPStatus()
	w.refreshUpstreamStatus()
	w.mcpTimer = qt.NewQTimer()
	w.mcpTimer.SetInterval(2000)
	w.mcpTimer.OnTimeout(func() {
		w.refreshMCPStatus()
		w.refreshUpstreamStatus()
	})
	w.mcpTimer.Start2()
}

// ---- Footer status indicators --------------------------------------------

// The colours every footer indicator uses, so one dot means the same thing as
// the next: green while something is running, red when it is stopped, amber
// when it is up but wants attention or has hit a problem.
const (
	statusRunning = colorSuccess
	statusStopped = colorServerError
	statusIssue   = colorHeld
)

// footerStatus renders one indicator: a coloured bullet followed by plain
// text. key is a palette key, resolved to the theme's colour here — these
// labels are rich text, so they need a real colour value, not the key itself.
func footerStatus(key, text string) string {
	return fmt.Sprintf(`<span style="color:%s">●</span>&nbsp; %s`, hex(key), html.EscapeString(text))
}

// refreshMCPStatus updates the footer indicator.
func (w *mainWindow) refreshMCPStatus() {
	st := w.ctrl.MCPStatus()
	switch {
	case st.Error != "":
		w.mcpStatus.SetText(footerStatus(statusIssue, "MCP: "+st.Error))
	case st.Running:
		w.mcpStatus.SetText(footerStatus(statusRunning, "MCP "+st.Addr))
	default:
		w.mcpStatus.SetText(footerStatus(statusStopped, "MCP off"))
	}
}

// refreshUpstreamStatus shows the upstream chain in force, so that opening
// an engagement with its own chain (or with none, over a global one) never
// changes where traffic goes unnoticed. It is hidden while traffic goes
// direct and nothing is overridden.
func (w *mainWindow) refreshUpstreamStatus() {
	chain := w.ctrl.Effective().Upstream
	eng, _, open := w.ctrl.EngagementSettings()
	own := open && eng.Upstream != nil
	active := chain.Active()
	if len(active) == 0 && !own {
		w.upstreamStatus.SetVisible(false)
		return
	}
	text := "Direct"
	if n := len(active); n == 1 {
		text = "Via upstream proxy"
	} else if n > 1 {
		text = fmt.Sprintf("Via %d upstream proxies", n)
	}
	tip := "Connections go straight to their servers."
	if len(active) > 0 {
		hops := make([]string, len(active))
		for i, p := range active {
			hops[i] = fmt.Sprintf("%d. %s %s", i+1, p.Kind, p.Addr())
		}
		tip = "Connections leave through:\n" + strings.Join(hops, "\n")
	}
	if own {
		text += " · engagement"
		tip += "\n\nSet by the open engagement (Settings → Engagement → Outbound Traffic)."
	} else {
		tip += "\n\nSet in Settings → Global → Outbound Traffic."
	}
	w.upstreamStatus.SetText(footerStatus(statusRunning, text))
	w.upstreamStatus.SetToolTip(tip)
	w.upstreamStatus.SetVisible(true)
}

func (w *mainWindow) refreshStatus() {
	addr, err := w.ctrl.ProxyStatus()
	if err != nil {
		w.status.SetText(footerStatus(statusStopped, "Proxy not running: "+err.Error()))
		w.listHint.SetText("The proxy couldn't start. Choose another address in Settings → Traffic Proxy.")
		w.detail.setHint("The proxy couldn't start.")
	} else {
		w.status.SetText(footerStatus(statusRunning, "SOCKS5 proxy listening on "+addr))
		w.listHint.SetText(fmt.Sprintf(
			"Point your browser's SOCKS5 proxy at %s and browse your app.\nRequests will appear here.", addr))
		w.detail.setHint("Click a request on the left to see it here.")
	}
	w.refreshCounts()
}

func (w *mainWindow) refreshCounts() {
	total := w.model.listed()
	shown := len(w.model.visible)

	// Auto-switch to compact rows once the list gets long, one time only so
	// the user can switch back.
	if !w.autoCompacted && !w.model.compact && total > compactThreshold {
		w.autoCompacted = true
		w.setCompact(true)
	}

	w.refreshBadges()
	if total == 0 {
		w.listPages.SetCurrentIndex(0) // the hint
	} else {
		w.listPages.SetCurrentIndex(1)
	}
	w.syncFlowHeader()

	switch {
	case w.model.filtered():
		w.total.SetText(fmt.Sprintf("%d of %d shown", shown, total))
	case total == 1:
		w.total.SetText("1 request")
	default:
		w.total.SetText(fmt.Sprintf("%d requests", total))
	}

	// The indicator is only shown while interception is on, so it is never
	// "stopped": green means it is running and nothing is waiting, amber that
	// messages are held for the user to deal with.
	on := w.ctrl.Interceptor().Enabled()
	w.interceptStatus.SetVisible(on)
	if on {
		text, tone := "Intercept on", statusRunning
		if w.model.held > 0 {
			text, tone = fmt.Sprintf("Intercept on · %d held", w.model.held), statusIssue
		}
		w.interceptStatus.SetText(footerStatus(tone, text))
	}
}

// ---- Store events --------------------------------------------------------

func (w *mainWindow) onStoreEvent(e capture.Event) {
	// Keep following new requests only if the user is already at the bottom,
	// so scrolling back through history isn't interrupted.
	sb := w.list.VerticalScrollBar()
	atBottom := sb.Value() >= sb.Maximum()
	wasShowingHeld := w.detail.showingHeld()

	w.model.apply(e)

	switch e.Kind {
	case capture.WSMessageAdded:
		if e.Flow.ID == w.detail.shownID {
			w.detail.wsMessageAdded(e.Flow)
		}
		return
	case capture.FlowAdded:
		if atBottom {
			w.list.ScrollToBottom()
		}
	case capture.FlowUpdated:
		if e.Flow.ID == w.detail.shownID {
			w.detail.show(e.Flow)
		}
	case capture.FlowsReset:
		// A different engagement's traffic: nothing that was on screen
		// belongs to it.
		w.detail.clear()
		w.setDetailOpen(false)
		w.list.ScrollToBottom()
	case capture.FlowsCleared:
		w.detail.clear()
		w.setDetailOpen(false)
	}

	// Work through held messages in arrival order: open one when it arrives
	// and nothing held is open, and the next once the open one is resolved.
	if e.Flow.State.Held() && !w.detail.showingHeld() ||
		wasShowingHeld && !w.detail.showingHeld() {
		w.openNextHeld()
	}
	w.refreshCounts()
}

// parseReplayHandle splits "<instance>.r<id>" into its parts. An empty
// instance means "this process". A bare "r<id>" (no instance) is also this
// process.
func parseReplayHandle(h string) (instance string, id uint64, ok bool) {
	if i := indexByte(h, '.'); i >= 0 {
		instance = h[:i]
		h = h[i+1:]
	}
	if len(h) < 2 || h[0] != 'r' {
		return "", 0, false
	}
	var n uint64
	for _, c := range h[1:] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return instance, n, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
