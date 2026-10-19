package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/terminal"
)

// terminalPanel is the Sessions panel's Terminal tab: a shell on the left and
// a Sessions column on the right, laid out like the Agents tab beside it so
// the two read as one place with two kinds of session in it.
//
// The shells themselves belong to the controller's terminal.Manager, not to
// this panel: when reproxy grows a command line of its own, it will be run
// through the same sessions.
type terminalPanel struct {
	mgr  *terminal.Manager
	root *qt.QWidget
	// split holds the shell and the Sessions column; it is turned upright
	// while the Sessions panel is too narrow for them side by side.
	split   *qt.QSplitter
	stacked bool

	view   *terminalView
	bar    *qt.QScrollBar
	pages  *qt.QStackedWidget
	list   *qt.QListWidget
	status *qt.QLabel

	current string   // selected session ID
	order   []string // session IDs in list order
	loading bool     // suppress the list's selection handler
	unsub   func()

	// repaint coalesces output: a shell can write far faster than the
	// screen is worth redrawing, so events only set a flag and this timer
	// paints what the flag says is there.
	repaint *qt.QTimer
	dirty   bool
}

const (
	terminalViewPage  = 0
	terminalEmptyPage = 1
	// terminalFrameMS is the shortest gap between repaints: about 60 a
	// second, whatever the shell is doing.
	terminalFrameMS = 16
)

func newTerminalPanel(mgr *terminal.Manager) *terminalPanel {
	p := &terminalPanel{mgr: mgr}

	p.split = qt.NewQSplitter3(qt.Horizontal)
	p.split.SetChildrenCollapsible(false)
	p.split.SetHandleWidth(1)
	p.split.AddWidget(p.buildTerminal())
	p.split.AddWidget(p.buildSessions())
	stackColumns(p.split, false)

	// The status line runs under both columns rather than under the
	// terminal alone, so the two boxes are the same box: same top edge,
	// same bottom edge.
	root := qt.NewQWidget2()
	rl := qt.NewQVBoxLayout(root)
	rl.SetContentsMargins(0, 0, 0, 0)
	rl.SetSpacing(2)
	rl.AddWidget2(p.split.QWidget, 1)
	p.status = qt.NewQLabel2()
	setName(p.status.QObject, "Muted")
	p.status.SetContentsMargins(8, 0, 8, 2)
	rl.AddWidget(p.status.QWidget)
	p.root = root

	p.repaint = qt.NewQTimer()
	p.repaint.SetInterval(terminalFrameMS)
	p.repaint.OnTimeout(p.flush)
	p.repaint.Start(terminalFrameMS)

	// Sessions change on the shells' own goroutines.
	p.unsub = mgr.Subscribe(func(e terminal.Event) {
		mainthread.Start(func() { p.onEvent(e) })
	})
	p.reloadSessions()
	return p
}

// setStacked lays the shell above the Sessions column, or beside it again.
func (p *terminalPanel) setStacked(on bool) {
	if p.stacked == on {
		return
	}
	p.stacked = on
	stackColumns(p.split, on)
}

// close stops following the sessions; the shells themselves are the
// controller's and are ended when the app shuts down.
func (p *terminalPanel) close() {
	if p.unsub != nil {
		p.unsub()
	}
	if p.repaint != nil {
		p.repaint.Stop()
	}
}

func (p *terminalPanel) buildTerminal() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(6, 6, 6, 6)
	v.SetSpacing(6)

	p.pages = qt.NewQStackedWidget2()
	setName(p.pages.QObject, "ChatArea") // the same framed area as the Agents tab
	p.pages.SetAttribute(qt.WA_StyledBackground)

	// The grid and its scrollbar, which walks back through what scrolled
	// off the top.
	grid := qt.NewQWidget2()
	gl := qt.NewQHBoxLayout(grid)
	gl.SetContentsMargins(0, 0, 0, 0)
	gl.SetSpacing(0)
	p.view = newTerminalView()
	p.view.onSize = func(int, int) { p.syncScrollbar() }
	p.view.onScroll = p.syncScrollbar
	p.bar = qt.NewQScrollBar3(qt.Vertical)
	p.bar.OnValueChanged(func(val int) {
		// The bar runs top to bottom; the view counts lines back from the
		// bottom, so the two are mirror images.
		p.view.scrollTo(p.bar.Maximum() - val)
	})
	gl.AddWidget2(p.view.w, 1)
	gl.AddWidget(p.bar.QWidget)

	emptyBox := qt.NewQWidget2()
	eb := qt.NewQVBoxLayout(emptyBox)
	emptyLabel := qt.NewQLabel3("No terminal open.\nUse + in the Sessions column to start one.")
	setName(emptyLabel.QObject, "EmptyState")
	emptyLabel.SetAlignment(qt.AlignCenter)
	emptyLabel.SetWordWrap(true) // the panel can be pinned to a narrow side
	eb.AddStretch()
	eb.AddWidget(emptyLabel.QWidget)
	eb.AddStretch()

	p.pages.AddWidget(grid)
	p.pages.AddWidget(emptyBox)
	p.pages.SetCurrentIndex(terminalEmptyPage)
	v.AddWidget2(p.pages.QWidget, 1)
	return w
}

func (p *terminalPanel) buildSessions() *qt.QWidget {
	// The pane sits in a wrapper with the terminal's own top, bottom and
	// right margins, so the two boxes line up: without it the frame starts
	// flush against the splitter and sits a few pixels above the terminal
	// beside it. The left margin is the terminal's right one.
	outer := qt.NewQWidget2()
	ol := qt.NewQVBoxLayout(outer)
	ol.SetContentsMargins(0, 6, 6, 6)

	w := qt.NewQFrame2()
	setName(w.QObject, "SessionsPane")
	w.SetAttribute(qt.WA_StyledBackground)
	v := qt.NewQVBoxLayout(w.QWidget)
	v.SetContentsMargins(6, 6, 6, 6)
	v.SetSpacing(6)

	header := qt.NewQHBoxLayout2()
	title := qt.NewQLabel3("Sessions")
	setName(title.QObject, "PanelTitle")
	add := qt.NewQToolButton2()
	add.SetText("+")
	add.SetToolTip("New terminal session")
	add.OnClicked(p.onAdd)
	header.AddWidget(title.QWidget)
	header.AddStretch()
	header.AddWidget(add.QWidget)
	v.AddLayout(header.QLayout)

	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "SessionsList")
	p.list.OnCurrentRowChanged(func(row int) {
		if !p.loading {
			p.selectRow(row)
		}
	})
	p.list.SetContextMenuPolicy(qt.CustomContextMenu)
	p.list.OnCustomContextMenuRequested(func(*qt.QPoint) { p.showSessionMenu() })
	v.AddWidget2(p.list.QWidget, 1)
	w.SetMinimumWidth(160)
	ol.AddWidget(w.QWidget)
	return outer
}

// ---- Sessions --------------------------------------------------------------

// onAdd starts a shell and shows it.
func (p *terminalPanel) onAdd() {
	s, err := p.mgr.New()
	if err != nil {
		qt.QMessageBox_Warning(p.root, "Terminal", "The shell couldn't be started.\n\n"+err.Error())
		return
	}
	p.current = s.ID()
	p.reloadSessions()
	p.showSession(s.ID())
	p.view.w.SetFocus()
}

func (p *terminalPanel) showSessionMenu() {
	row := p.list.CurrentRow()
	if row < 0 || row >= len(p.order) {
		return
	}
	id := p.order[row]
	menu := qt.NewQMenu2()
	menu.AddActionWithText("Close").OnTriggered(func() { p.mgr.Close(id) })
	menu.Exec3(qt.QCursor_Pos(), nil)
}

func (p *terminalPanel) selectRow(row int) {
	if row < 0 || row >= len(p.order) {
		p.current = ""
		p.showSession("")
		return
	}
	p.current = p.order[row]
	p.showSession(p.current)
}

// showSession puts a session (or none) in the view.
func (p *terminalPanel) showSession(id string) {
	s, ok := p.mgr.Get(id)
	if !ok {
		p.view.setSession(nil)
		p.pages.SetCurrentIndex(terminalEmptyPage)
		p.status.SetText("")
		p.bar.SetVisible(false)
		return
	}
	p.pages.SetCurrentIndex(terminalViewPage)
	p.view.setSession(s)
	p.syncScrollbar()
	p.refreshStatus()
}

func (p *terminalPanel) reloadSessions() {
	sessions := p.mgr.List()
	p.loading = true
	for p.list.Count() > 0 {
		p.list.TakeItem(0)
	}
	p.order = p.order[:0]
	sel := -1
	for i, s := range sessions {
		p.list.AddItem(terminalLabel(s, i+1))
		p.order = append(p.order, s.ID())
		if s.ID() == p.current {
			sel = i
		}
	}
	if sel < 0 && len(sessions) > 0 {
		sel = len(sessions) - 1 // the one just started, or what is left after a close
		p.current = p.order[sel]
	}
	if sel >= 0 {
		p.list.SetCurrentRow(sel)
	}
	p.loading = false
	if sel < 0 {
		p.current = ""
		p.showSession("")
		return
	}
	if _, shown := p.mgr.Get(p.current); shown && p.view.session == nil {
		p.showSession(p.current)
	}
}

// terminalLabel is a session's row: the shell's name, numbered as VS Code
// numbers its terminals, and what it says it is doing when it says anything.
func terminalLabel(s *terminal.Session, n int) string {
	label := fmt.Sprintf("%d: %s", n, s.Name())
	if title := s.Title(); title != "" && title != s.Name() {
		label += " — " + title
	}
	return label
}

// ---- Events ----------------------------------------------------------------

func (p *terminalPanel) onEvent(e terminal.Event) {
	switch e.Kind {
	case terminal.Output:
		if e.ID == p.current {
			p.dirty = true // painted by the next frame
		}
	case terminal.Added, terminal.Closed:
		if e.ID == p.current && e.Kind == terminal.Closed {
			p.current = ""
			p.view.setSession(nil)
		}
		p.reloadSessions()
	case terminal.Titled:
		p.reloadSessions()
	case terminal.Exited:
		p.refreshStatus()
	}
}

// flush repaints the view when output has arrived since the last frame.
func (p *terminalPanel) flush() {
	if !p.dirty {
		return
	}
	p.dirty = false
	p.view.w.Update()
	p.syncScrollbar()
}

// syncScrollbar matches the bar to what the session holds and where the view
// is in it. The bar is hidden while everything fits on the screen.
func (p *terminalPanel) syncScrollbar() {
	if p.view.session == nil {
		p.bar.SetVisible(false)
		return
	}
	back := p.view.session.ScrollbackLen()
	p.bar.SetVisible(back > 0)
	if back == 0 {
		return
	}
	p.bar.BlockSignals(true)
	p.bar.SetRange(0, back)
	p.bar.SetPageStep(max(p.view.rows()-1, 1))
	p.bar.SetSingleStep(1)
	p.bar.SetValue(back - p.view.offset)
	p.bar.BlockSignals(false)
}

// refreshStatus is the line under the terminal: what it is, and whether its
// shell is still there.
func (p *terminalPanel) refreshStatus() {
	s, ok := p.mgr.Get(p.current)
	if !ok {
		p.status.SetText("")
		return
	}
	cols, rows := s.Size()
	text := fmt.Sprintf("%s · %d×%d", s.Name(), cols, rows)
	if s.Exited() {
		text += " · the shell has ended"
	}
	p.status.SetText(text)
}
