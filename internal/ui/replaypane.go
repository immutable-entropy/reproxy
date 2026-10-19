package ui

import (
	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/findings"
	"reproxy/internal/focus"
	"reproxy/internal/replay"
)

// replayPane is the Replay section of the main window: any number of replay
// views, one per tab, each closable with the × in its tab. Opening a request
// in Replay adds a tab rather than replacing what is already open, so
// several requests can be worked on at once.
//
// The pane keeps the focus tracker pointing at the tab the user is working
// in, with its unsent edits, so an agent asked about "this request" sees the
// same one (see focus.Tracker).
type replayPane struct {
	root  *qt.QWidget
	stack *qt.QStackedWidget // 0: empty state, 1: the tabs
	tabs  *qt.QTabWidget

	sessions *replay.Sessions
	focus    *focus.Tracker

	views []*replayView // index matches the tab index
	// current is the tab the focus tracker was last told about, so its draft
	// can be captured when the user moves away from it.
	current *replayView
	// active reports whether the Replay section is the one on screen.
	active bool
	// closing is set while a tab is being removed. Qt selects another tab
	// part-way through that, and the selection handler would then read a
	// views slice that no longer matches the tab bar.
	closing bool
	// onFinding is set by the window: it opens the Create Finding dialog on
	// a draft raised from a request or response in any of these tabs.
	onFinding func(findings.Draft)
}

const (
	replayPaneEmpty = 0
	replayPaneTabs  = 1
)

func newReplayPane(sessions *replay.Sessions, tracker *focus.Tracker) *replayPane {
	p := &replayPane{
		root:     qt.NewQWidget2(),
		stack:    qt.NewQStackedWidget2(),
		tabs:     qt.NewQTabWidget2(),
		sessions: sessions,
		focus:    tracker,
	}

	p.tabs.SetDocumentMode(true)
	p.tabs.SetTabsClosable(true)
	p.tabs.SetMovable(true)
	p.tabs.OnTabCloseRequested(p.closeTab)
	p.tabs.OnCurrentChanged(func(int) { p.currentChanged() })

	p.stack.AddWidget(newEmptyState("Nothing in Replay yet",
		"Select a request, then click Replay to open it here. "+
			"Each request you open gets its own tab."))
	p.stack.AddWidget(p.tabs.QWidget)
	p.stack.SetCurrentIndex(replayPaneEmpty)

	layout := qt.NewQVBoxLayout(p.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.AddWidget(p.stack.QWidget)
	return p
}

// open adds a tab on sess and selects it. A session already open is raised
// instead of opened twice.
func (p *replayPane) open(sess replay.Session) {
	if p.selectSession(sess.ID) {
		return
	}
	v := newReplayView(p.sessions, sess)
	v.onSent = p.sentElsewhere
	// The window wires onFinding after the pane is built, so the view asks
	// the pane for it each time rather than copying it now.
	v.onFinding = func(d findings.Draft) {
		if p.onFinding != nil {
			p.onFinding(d)
		}
	}
	p.views = append(p.views, v)
	i := p.tabs.AddTab(v.root, v.tabTitle())
	p.tabs.SetTabToolTip(i, v.tabToolTip())
	p.stack.SetCurrentIndex(replayPaneTabs)
	p.tabs.SetCurrentIndex(i)
}

// selectSession selects the tab on a session, reporting whether it was open.
func (p *replayPane) selectSession(id uint64) bool {
	for i, v := range p.views {
		if v.session.ID == id {
			p.stack.SetCurrentIndex(replayPaneTabs)
			p.tabs.SetCurrentIndex(i)
			return true
		}
	}
	return false
}

// closeTab closes one tab, releasing its session.
func (p *replayPane) closeTab(i int) {
	if i < 0 || i >= len(p.views) {
		return
	}
	v := p.views[i]
	// Drop the view from the index before its tab goes, and ignore the
	// selection change Qt makes during the removal: the two are only back in
	// step once both are done.
	p.views = append(p.views[:i], p.views[i+1:]...)
	if v == p.current {
		p.current = nil
	}
	p.closing = true
	p.tabs.RemoveTab(i)
	p.closing = false

	v.close()
	p.focus.CloseReplay(v.session.ID)
	v.root.DeleteLater()
	if len(p.views) == 0 {
		p.stack.SetCurrentIndex(replayPaneEmpty)
	}
	p.currentChanged() // settle the focus tracker on whatever is selected now
}

// closeCurrent closes the selected tab, for the menu action.
func (p *replayPane) closeCurrent() {
	if i := p.tabs.CurrentIndex(); i >= 0 {
		p.closeTab(i)
	}
}

// closeAll releases every session, for application exit.
func (p *replayPane) closeAll() {
	for _, v := range p.views {
		v.close()
	}
	p.views = nil
	p.current = nil
}

// currentView returns the selected view, or nil when nothing is open.
func (p *replayPane) currentView() *replayView {
	i := p.tabs.CurrentIndex()
	if i < 0 || i >= len(p.views) {
		return nil
	}
	return p.views[i]
}

// currentChanged moves the focus tracker to the newly selected tab, keeping
// the edits of the one being left.
func (p *replayPane) currentChanged() {
	if p.closing {
		return
	}
	next := p.currentView()
	if p.current != nil && p.current != next {
		p.focus.SetReplayDraft(p.current.session.ID, p.current.draft())
	}
	p.current = next
	if next == nil {
		return
	}
	p.clearMark() // the user is looking at it now
	if p.active {
		p.focus.ActivateReplay(next.session.ID, next.draft())
	}
}

// setActive is called when the Replay section is shown or left. Leaving
// records the open tab's unsent edits — exactly when the user turns to the
// Agents chat to talk about them — rather than forgetting the session.
func (p *replayPane) setActive(on bool) {
	p.active = on
	v := p.currentView()
	if v == nil {
		return
	}
	if on {
		p.focus.ActivateReplay(v.session.ID, v.draft())
	} else {
		p.focus.SetReplayDraft(v.session.ID, v.draft())
	}
}

// sentElsewhere marks a tab whose session an agent just sent in, so a send
// the user didn't make is visible on the tab itself.
func (p *replayPane) sentElsewhere(v *replayView, e replay.Entry) {
	if e.By == replay.OwnerUser {
		return
	}
	if i := p.tabs.IndexOf(v.root); i >= 0 && i != p.tabs.CurrentIndex() {
		p.tabs.SetTabText(i, "• "+v.tabTitle())
	}
}

// clearMark removes the agent-activity mark from the selected tab.
func (p *replayPane) clearMark() {
	i := p.tabs.CurrentIndex()
	if v := p.currentView(); v != nil {
		p.tabs.SetTabText(i, v.tabTitle())
	}
}

// refreshTheme recolours every open view after a theme change.
func (p *replayPane) refreshTheme() {
	for _, v := range p.views {
		v.refreshTheme()
	}
}

// plainEditors and lineEditors expose the open views' editors for Edit-menu
// routing (Copy and Select All act on whichever has focus).
func (p *replayPane) plainEditors() []*qt.QPlainTextEdit {
	var out []*qt.QPlainTextEdit
	for _, v := range p.views {
		out = append(out, v.plainEditors()...)
	}
	return out
}

func (p *replayPane) lineEditors() []*qt.QLineEdit {
	var out []*qt.QLineEdit
	for _, v := range p.views {
		out = append(out, v.lineEditors()...)
	}
	return out
}

// newEmptyState is the centred "nothing here" placeholder used by sections
// with no content yet.
func newEmptyState(title, detail string) *qt.QWidget {
	page := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(page)
	v.AddStretch()
	t := qt.NewQLabel3(title)
	setName(t.QObject, "EmptyTitle")
	t.SetAlignment(qt.AlignCenter)
	v.AddWidget(t.QWidget)
	if detail != "" {
		d := qt.NewQLabel3(detail)
		setName(d.QObject, "EmptyState")
		d.SetAlignment(qt.AlignCenter)
		d.SetWordWrap(true)
		v.AddSpacing(6)
		v.AddWidget(d.QWidget)
	}
	v.AddStretch()
	return page
}
