package ui

import (
	"os"
	"sort"
	"strings"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/agent"
	"reproxy/internal/approval"
	"reproxy/internal/config"
)

// agentsPanel is the Sessions panel's Agents tab: a chat on the left (75%) and
// a Sessions column on the right (25%). Sessions are global (the manager
// persists them and, with the hub, they are shared across windows); this
// panel is a view onto the manager's sessions for this window.
type agentsPanel struct {
	ctrl    Controller
	mgr     *agent.Manager
	openRep func(handle string) // open a replay session by "<inst>.r<id>" handle

	root *qt.QWidget
	// split holds the chat and the Sessions column; it is turned upright
	// while the Sessions panel is too narrow for them side by side.
	split   *qt.QSplitter
	stacked bool

	// Chat side.
	transcript *qt.QTextBrowser
	chatBox    *qt.QFrame  // the framed transcript; hidden with no session
	chatSpace  *qt.QWidget // holds the transcript's room while it is hidden
	// expanded holds the message indices whose tool call is expanded to show
	// its raw input/output; revealed holds those whose hidden finding value
	// the user has unmasked. rendered is how many messages the transcript
	// currently shows, so a streamed message is appended with the right index
	// for its disclosure link. iconDir holds the images the transcript
	// references (see agenttranscript.go).
	expanded map[int]bool
	revealed map[int]bool
	rendered int
	iconDir  string
	context  *qt.QLabel
	input    *qt.QPlainTextEdit
	sendBtn  *qt.QPushButton
	stopBtn  *qt.QPushButton
	// sendPages shows Send or Stop in the same place, composer the input
	// row or the line that stands in for it when no session is open.
	sendPages *qt.QStackedWidget
	composer  *qt.QStackedWidget
	// The permission mode lives on the Send button: modeBtn's chevron drops
	// modeMenu, whose actions are modeActs in agentModeOptions order.
	modeBtn  *qt.QToolButton
	modeMenu *qt.QMenu
	modeActs []*qt.QAction

	// Sessions side.
	list *qt.QListWidget
	// lockIcon marks a managed session's row (the findings reviewer's), which
	// the user can't close.
	lockIcon *qt.QIcon

	current      string   // selected session ID
	order        []string // session IDs in list order
	loading      bool     // suppress list-selection handler
	unsub        func()   // manager subscription
	unsubApprove func()   // approval broker subscription
	openSettings func()   // opens Settings → Agents

	approvals *qt.QWidget            // holds pending approval cards
	cards     map[string]*qt.QWidget // approval request ID -> card
}

const (
	// The composer's two pages: the input row, or the line that stands in
	// for it when no session is open.
	agentsChatPage  = 0
	agentsEmptyPage = 1
	// The two faces of the button at the input's edge.
	agentsSendPage = 0
	agentsStopPage = 1
)

func newAgentsPanel(ctrl Controller, openReplay func(string)) *agentsPanel {
	p := &agentsPanel{ctrl: ctrl, mgr: ctrl.Agents(), openRep: openReplay}
	p.lockIcon = managedIcon()

	p.split = qt.NewQSplitter3(qt.Horizontal)
	p.split.SetChildrenCollapsible(false)
	p.split.SetHandleWidth(1)
	p.split.AddWidget(p.buildChat())
	p.split.AddWidget(p.buildSessions())
	stackColumns(p.split, false)

	p.root = p.split.QWidget

	// Follow the manager; events can arrive from turn goroutines.
	p.unsub = p.mgr.Subscribe(func(e agent.Event) {
		mainthread.Start(func() { p.onEvent(e) })
	})
	// Follow approval requests so Ask-mode tool calls show Approve/Deny.
	p.cards = map[string]*qt.QWidget{}
	p.unsubApprove = p.ctrl.Approvals().Subscribe(func(e approval.Event) {
		mainthread.Start(func() { p.onApproval(e) })
	})
	p.reloadSessions()
	p.syncApprovals()
	return p
}

// setStacked lays the chat above the Sessions column, or beside it again.
func (p *agentsPanel) setStacked(on bool) {
	if p.stacked == on {
		return
	}
	p.stacked = on
	stackColumns(p.split, on)
}

// close stops listening; called when the window closes.
func (p *agentsPanel) close() {
	if p.unsub != nil {
		p.unsub()
	}
	if p.unsubApprove != nil {
		p.unsubApprove()
	}
	if p.iconDir != "" {
		os.RemoveAll(p.iconDir)
	}
}

func (p *agentsPanel) buildChat() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(6, 6, 6, 6)
	v.SetSpacing(8)

	// The transcript, framed as the "chat window" and set apart from the
	// input beneath it. With no session open there is nothing to show, so
	// the box isn't drawn at all: the tab stays empty until a session puts
	// something in it, and what the user needs to know is said down where
	// the input would be (see the composer's empty page).
	p.chatBox = qt.NewQFrame2()
	setName(p.chatBox.QObject, "ChatArea")
	p.chatBox.SetAttribute(qt.WA_StyledBackground)
	cb := qt.NewQVBoxLayout(p.chatBox.QWidget)
	cb.SetContentsMargins(0, 0, 0, 0)
	p.transcript = qt.NewQTextBrowser2()
	p.transcript.SetOpenExternalLinks(false)
	// A link is an in-transcript control (expand a tool call, open a replay),
	// never a page to navigate to: handle it in onAnchor and don't let the
	// browser try to load it, which would clear the document.
	p.transcript.SetOpenLinks(false)
	p.transcript.SetReadOnly(true)
	p.transcript.SetFrameShape(qt.QFrame__NoFrame)
	setName(p.transcript.QObject, "AgentTranscript")
	p.transcript.OnAnchorClicked(func(url *qt.QUrl) { p.onAnchor(url.ToString()) })
	p.expanded = map[int]bool{}
	p.revealed = map[int]bool{}
	p.setupTranscriptAssets() // object-type icons and the themed stylesheet
	cb.AddWidget(p.transcript.QWidget)
	p.chatBox.SetVisible(false)
	v.AddWidget2(p.chatBox.QWidget, 1)

	// With the transcript hidden this takes its place, so the line standing
	// in for the input stays where the input is rather than rising to the
	// top of an empty tab.
	p.chatSpace = qt.NewQWidget2()
	p.chatSpace.SetSizePolicy2(qt.QSizePolicy__Expanding, qt.QSizePolicy__Expanding)
	v.AddWidget2(p.chatSpace, 1)

	// Pending approval cards (Ask-mode tool calls) sit just above the input.
	p.approvals = qt.NewQWidget2()
	av := qt.NewQVBoxLayout(p.approvals)
	av.SetContentsMargins(0, 0, 0, 0)
	av.SetSpacing(4)
	p.approvals.SetVisible(false)
	v.AddWidget(p.approvals)

	// Context line: what "this request" will refer to. The permission mode
	// used to sit beside it on a row of its own; it is now folded into the
	// Send button, which gives the transcript that row back.
	p.context = qt.NewQLabel2()
	setName(p.context.QObject, "Muted")
	p.context.SetWordWrap(true)
	p.context.SetContentsMargins(2, 0, 2, 0)
	v.AddWidget(p.context.QWidget)

	// The composer, or — with no session open — the line that says so in
	// its place, so an empty tab costs one line rather than a boxed notice.
	p.composer = qt.NewQStackedWidget2()
	p.composer.AddWidget(p.buildComposer())

	emptyLine := qt.NewQLabel3("No agent session open.  Use + in the Sessions column to start one.")
	setName(emptyLine.QObject, "EmptyState")
	emptyLine.SetAlignment(qt.AlignCenter)
	emptyLine.SetWordWrap(true) // the panel can be pinned to a narrow side
	p.composer.AddWidget(emptyLine.QWidget)
	p.composer.SetCurrentIndex(agentsEmptyPage)
	v.AddWidget(p.composer.QWidget)

	return w
}

// buildComposer is the input and its button: the text area with Send (and
// Stop, while running) flush against its right edge inside one bordered
// control, so the button reads as that edge rather than as something
// floating inside the box. The chevron beside Send drops the permission
// mode, which is a choice about the next send and so belongs on the button
// that makes it.
func (p *agentsPanel) buildComposer() *qt.QWidget {
	inputBox := qt.NewQFrame2()
	setName(inputBox.QObject, "AgentInputBox")
	inputBox.SetAttribute(qt.WA_StyledBackground)
	ib := qt.NewQHBoxLayout(inputBox.QWidget)
	ib.SetContentsMargins(0, 0, 0, 0)
	ib.SetSpacing(0)

	p.input = qt.NewQPlainTextEdit2()
	setName(p.input.QObject, "AgentInput")
	p.input.SetPlaceholderText("Ask the agent to review, test or replay traffic…  (Enter to send, Shift+Enter for a newline)")
	p.input.SetMaximumHeight(90)
	p.installInputKeys()
	ib.AddWidget2(p.input.QWidget, 1)

	// Send and Stop take the same place: only one of them applies at a time.
	// The button fills the box's height, so it reads as its right edge.
	p.sendPages = qt.NewQStackedWidget2()
	p.sendPages.SetSizePolicy2(qt.QSizePolicy__Minimum, qt.QSizePolicy__Expanding)
	p.sendBtn = qt.NewQPushButton3("Send")
	setName(p.sendBtn.QObject, "AgentSendButton")
	p.sendBtn.SetAutoDefault(false)
	p.sendBtn.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	p.sendBtn.SetSizePolicy2(qt.QSizePolicy__Minimum, qt.QSizePolicy__Expanding)
	p.sendBtn.OnClicked(p.onSend)
	p.stopBtn = qt.NewQPushButton3("Stop")
	setName(p.stopBtn.QObject, "AgentStopButton")
	p.stopBtn.SetAutoDefault(false)
	p.stopBtn.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	p.stopBtn.SetSizePolicy2(qt.QSizePolicy__Minimum, qt.QSizePolicy__Expanding)
	p.stopBtn.OnClicked(func() {
		if p.current != "" {
			p.mgr.Stop(p.current)
		}
	})
	p.sendPages.AddWidget(p.sendBtn.QWidget)
	p.sendPages.AddWidget(p.stopBtn.QWidget)
	ib.AddWidget(p.sendPages.QWidget)

	p.modeBtn = qt.NewQToolButton2()
	setName(p.modeBtn.QObject, "AgentModeButton")
	p.modeBtn.SetText("▾")
	p.modeBtn.SetFocusPolicy(qt.NoFocus)
	p.modeBtn.SetCursor(qt.NewQCursor2(qt.PointingHandCursor))
	p.modeBtn.SetPopupMode(qt.QToolButton__InstantPopup)
	p.modeBtn.SetSizePolicy2(qt.QSizePolicy__Fixed, qt.QSizePolicy__Expanding)
	p.modeMenu = qt.NewQMenu2()
	for _, o := range agentModeOptions {
		act := p.modeMenu.AddActionWithText(o.label)
		act.SetCheckable(true)
		p.modeActs = append(p.modeActs, act)
		act.OnTriggered(func() { p.setMode(agent.Mode(o.value)) })
	}
	p.modeBtn.SetMenu(p.modeMenu)
	ib.AddWidget(p.modeBtn.QWidget)
	p.showMode(agent.ModeAsk)

	return inputBox.QWidget
}

func (p *agentsPanel) buildSessions() *qt.QWidget {
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
	add.SetToolTip("New agent session")
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
	return w.QWidget
}

// ---- Sessions column -------------------------------------------------------

// onAdd creates a session. With no profile configured it points the user at
// Settings; with one it uses it; with several it offers a menu.
func (p *agentsPanel) onAdd() {
	profiles := p.ctrl.AgentConfig().Profiles
	switch len(profiles) {
	case 0:
		if qt.QMessageBox_Question2(p.root, "No agents configured",
			"Add an AI agent in Settings → Agents first. Open settings now?",
			qt.QMessageBox__Yes, qt.QMessageBox__No) == int(qt.QMessageBox__Yes) {
			if p.openSettings != nil {
				p.openSettings()
			}
		}
	case 1:
		p.createSession(profiles[0])
	default:
		menu := qt.NewQMenu2()
		for _, prof := range profiles {
			prof := prof
			menu.AddActionWithText(agentListLabel(prof)).OnTriggered(func() { p.createSession(prof) })
		}
		menu.Exec3(qt.QCursor_Pos(), nil)
	}
}

func (p *agentsPanel) createSession(prof config.AgentProfile) {
	sum := p.mgr.Create(prof.ID, "")
	p.current = sum.ID
	p.reloadSessions()
	p.showSession(sum.ID)
	p.input.SetFocus()
}

func (p *agentsPanel) showSessionMenu() {
	row := p.list.CurrentRow()
	if row < 0 || row >= len(p.order) {
		return
	}
	id := p.order[row]
	menu := qt.NewQMenu2()
	menu.AddActionWithText("Close").OnTriggered(func() { p.closeSession(id) })
	menu.Exec3(qt.QCursor_Pos(), nil)
}

// closeSession closes a session, unless it is a managed one (the findings
// reviewer's): that is refused, with a note on how to actually turn it off, so
// it can't be shut from under the service that runs it.
func (p *agentsPanel) closeSession(id string) {
	if s, ok := p.mgr.Get(id); ok && s.Managed {
		qt.QMessageBox_Information(p.root, "Can't close this session",
			"reProxy runs the findings review session while an agent's “Watch for and "+
				"review new findings” setting is on. To stop and remove it, turn that "+
				"setting off for the agent in Settings → Agents.")
		return
	}
	p.mgr.Close(id)
}

func (p *agentsPanel) selectRow(row int) {
	if row < 0 || row >= len(p.order) {
		p.current = ""
		p.showChat(false)
		return
	}
	p.current = p.order[row]
	p.showSession(p.current)
	p.refreshApprovalCards()
}

func (p *agentsPanel) reloadSessions() {
	sums := p.mgr.List()
	// Managed sessions (the Findings review session) pin to the top; the
	// manager's oldest-first order is kept within each group.
	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].Managed && !sums[j].Managed
	})
	p.loading = true
	for p.list.Count() > 0 {
		p.list.TakeItem(0)
	}
	p.order = p.order[:0]
	sel := -1
	for i, s := range sums {
		p.list.AddItem(sessionLabel(s))
		p.decorateRow(i, s)
		p.order = append(p.order, s.ID)
		if s.ID == p.current {
			sel = i
		}
	}
	if sel < 0 && len(sums) > 0 && p.current == "" {
		// keep nothing selected until the user picks
	}
	p.loading = false
	if sel >= 0 {
		p.list.SetCurrentRow(sel)
	} else if p.current == "" {
		p.showChat(false)
	}
}

func sessionLabel(s agent.Summary) string {
	dot := statusDot(s.Status)
	title := s.Title
	if title == "" {
		title = "New chat"
	}
	return dot + " " + title
}

func statusDot(st agent.Status) string {
	switch st {
	case agent.StatusRunning:
		return "●" // active
	case agent.StatusError:
		return "▲"
	}
	return "○"
}

// ---- Chat side -------------------------------------------------------------

func (p *agentsPanel) showSession(id string) {
	s, ok := p.mgr.Get(id)
	if !ok {
		p.showChat(false)
		return
	}
	p.showChat(true)
	p.expanded = map[int]bool{} // a different session: nothing expanded yet
	p.revealed = map[int]bool{} // or revealed
	p.transcript.SetHtml(p.renderTranscript(s))
	p.rendered = len(s.Messages)
	p.scrollToEnd()
	p.showMode(s.Mode)
	p.reflectStatus(s.Status)
	p.updateContextChip()
}

// showChat puts the chat on screen, or takes it off when there is no
// session: the transcript's box goes with it, so an empty tab is empty
// rather than an empty box, and the line in the composer's place says what
// to do about it.
func (p *agentsPanel) showChat(on bool) {
	p.chatBox.SetVisible(on)
	p.chatSpace.SetVisible(!on)
	p.context.SetVisible(on)
	page := agentsEmptyPage
	if on {
		page = agentsChatPage
	}
	p.composer.SetCurrentIndex(page)
}

func (p *agentsPanel) reflectStatus(st agent.Status) {
	running := st == agent.StatusRunning
	page := agentsSendPage
	if running {
		page = agentsStopPage
	}
	p.sendPages.SetCurrentIndex(page)
	p.sendBtn.SetEnabled(!running)
}

func (p *agentsPanel) onEvent(e agent.Event) {
	switch e.Kind {
	case agent.SessionAdded, agent.SessionRemoved:
		p.reloadSessions()
		if e.Kind == agent.SessionRemoved && e.Summary.ID == p.current {
			p.current = ""
			p.showChat(false)
		}
	case agent.SessionChanged:
		p.updateRow(e.Summary)
		if e.Summary.ID == p.current {
			p.reflectStatus(e.Summary.Status)
		}
	case agent.MessageAdded:
		if e.SessionID == p.current {
			p.appendMessage(e.Message)
		}
	}
}

func (p *agentsPanel) updateRow(s agent.Summary) {
	for i, id := range p.order {
		if id == s.ID {
			// A session that has just become managed (set right after it is
			// created) must jump to the top: rebuild so it sorts into place.
			if s.Managed && i != 0 {
				p.reloadSessions()
				return
			}
			if item := p.list.Item(i); item != nil {
				item.SetText(sessionLabel(s))
				p.decorateRow(i, s)
			}
			return
		}
	}
}

// decorateRow marks a managed session's row with a lock and a tooltip saying
// why it can't be closed, and clears both from a row that is no longer managed.
func (p *agentsPanel) decorateRow(i int, s agent.Summary) {
	item := p.list.Item(i)
	if item == nil {
		return
	}
	if s.Managed {
		item.SetIcon(p.lockIcon)
		item.SetToolTip("Managed by reProxy — turn off this agent's " +
			"“Watch for and review new findings” setting in Settings → Agents to remove it.")
		return
	}
	item.SetIcon(qt.NewQIcon())
	item.SetToolTip("")
}

// managedIcon is a small padlock, in the muted colour, for a managed session's
// row.
func managedIcon() *qt.QIcon {
	const s = 32
	pix := qt.NewQPixmap2(s, s)
	pix.FillWithFillColor(qt.NewQColor2(qt.Transparent))
	pt := qt.NewQPainter2(pix.QPaintDevice)
	pt.SetRenderHint(qt.QPainter__Antialiasing)
	col := color(colorMuted)
	// The body: a filled rounded rectangle.
	pt.SetPenWithStyle(qt.NoPen)
	pt.SetBrush(qt.NewQBrush3(col))
	pt.DrawRoundedRect(qt.NewQRectF4(8, 15, 16, 12), 3, 3)
	// The shackle: the top half of an ellipse above it, open at the bottom.
	pen := qt.NewQPen3(col)
	pen.SetWidthF(2.6)
	pen.SetCapStyle(qt.RoundCap)
	pt.SetPenWithPen(pen)
	pt.SetBrush(qt.NewQBrush3(qt.NewQColor2(qt.Transparent)))
	pt.DrawArc(qt.NewQRectF4(11, 7, 10, 15), 0, 180*16)
	pt.End()
	return qt.NewQIcon2(pix)
}

func (p *agentsPanel) onSend() {
	text := strings.TrimSpace(p.input.ToPlainText())
	if text == "" || p.current == "" {
		return
	}
	if err := p.mgr.Send(p.current, text); err != nil {
		return
	}
	p.input.SetPlainText("")
}

// setMode is the chevron's menu: it records the choice and, with a session
// open, applies it to that session.
func (p *agentsPanel) setMode(m agent.Mode) {
	p.showMode(m)
	if p.current != "" {
		p.mgr.SetMode(p.current, m)
	}
}

// showMode puts the mode on the button: its tooltip says which is in force
// and the menu ticks it. Ask is what a session starts in.
func (p *agentsPanel) showMode(m agent.Mode) {
	for i, o := range agentModeOptions {
		if i < len(p.modeActs) {
			p.modeActs[i].SetChecked(o.value == string(m))
		}
	}
	label := agentModeOptions[0].label
	for _, o := range agentModeOptions {
		if o.value == string(m) {
			label = o.label
		}
	}
	tip := label + " — click to change.\n" +
		"Ask: approve each action that sends or changes traffic.\n" +
		"Auto: act without asking (still kept to the request's domain)."
	p.modeBtn.SetToolTip(tip)
	p.sendBtn.SetToolTip("Send (Enter) · " + label)
}

// updateContextChip shows what "this request" would mean in the next
// message, from the focus tracker.
func (p *agentsPanel) updateContextChip() {
	f := p.ctrl.Focus().Snapshot()
	switch f.Recent {
	case "replay":
		if s, ok := p.ctrl.ReplaySessions().Get(f.ReplaySession); ok {
			p.context.SetText("Context: Replay · " + s.Template.Method + " " + s.Template.URL)
			return
		}
	case "flow":
		if flow, ok := p.ctrl.Store().Get(f.SelectedFlow); ok {
			p.context.SetText("Context: selected request · " + flow.Method + " " + flow.URL())
			return
		}
	}
	p.context.SetText("Context: none — select a request or open a Replay window to give the agent one.")
}

// ---- Transcript rendering --------------------------------------------------
//
// The transcript is a QTextBrowser rendering HTML. Message and tool-call
// rendering — the readable chat, the collapsible tool rows and the object
// cards — lives in agenttranscript.go, which also themes it.

// openSettings is set by the main window so the empty-state can open
// Settings → Agents.
func (p *agentsPanel) setOpenSettings(fn func()) { p.openSettings = fn }

var agentModeOptions = []option[string]{
	{"Ask before acting", string(agent.ModeAsk)},
	{"Act automatically", string(agent.ModeAuto)},
}

// installInputKeys makes Enter send and Shift+Enter insert a newline.
func (p *agentsPanel) installInputKeys() {
	p.input.OnKeyPressEvent(func(super func(e *qt.QKeyEvent), e *qt.QKeyEvent) {
		key := e.Key()
		isEnter := key == int(qt.Key_Return) || key == int(qt.Key_Enter)
		if isEnter && e.Modifiers()&qt.ShiftModifier == 0 {
			p.onSend()
			return
		}
		super(e)
	})
}

// ---- Approvals -------------------------------------------------------------

// syncApprovals renders any requests already pending when the panel opens.
func (p *agentsPanel) syncApprovals() {
	for _, r := range p.ctrl.Approvals().Pending() {
		p.onApproval(approval.Event{Request: r})
	}
}

// onApproval adds or removes an Approve/Deny card for an agent action.
func (p *agentsPanel) onApproval(e approval.Event) {
	if e.Resolved {
		p.removeCard(e.Request.ID)
		return
	}
	// Only show cards for the session in view, or for external MCP clients
	// (which have no session), so each window prompts for what it is showing.
	if e.Request.Session != "" && e.Request.Session != p.current {
		return
	}
	if _, exists := p.cards[e.Request.ID]; exists {
		return
	}
	p.cards[e.Request.ID] = p.buildApprovalCard(e.Request)
	av := p.approvals.Layout()
	av.AddWidget(p.cards[e.Request.ID])
	p.approvals.SetVisible(true)
}

func (p *agentsPanel) buildApprovalCard(r approval.Request) *qt.QWidget {
	card := qt.NewQFrame2()
	setName(card.QObject, "ApprovalCard")
	card.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(card.QWidget)
	h.SetContentsMargins(10, 6, 10, 6)
	h.SetSpacing(8)

	who := "The agent"
	if r.Session == "" {
		who = "An MCP client"
		if r.Client != "" {
			who = r.Client
		}
	}
	label := qt.NewQLabel3(who + " wants to " + approvalVerb(r))
	label.SetWordWrap(true)
	h.AddWidget2(label.QWidget, 1)

	allow := qt.NewQPushButton3("Approve")
	setName(allow.QObject, "PrimaryButton")
	allow.SetAutoDefault(false)
	id := r.ID
	allow.OnClicked(func() { p.ctrl.Approvals().Decide(id, true) })
	deny := qt.NewQPushButton3("Deny")
	deny.SetAutoDefault(false)
	deny.OnClicked(func() { p.ctrl.Approvals().Decide(id, false) })
	h.AddWidget(allow.QWidget)
	h.AddWidget(deny.QWidget)
	return card.QWidget
}

// approvalVerb turns a request into a short action phrase.
func approvalVerb(r approval.Request) string {
	if r.Summary != "" {
		return r.Summary
	}
	return r.Tool
}

func (p *agentsPanel) removeCard(id string) {
	card, ok := p.cards[id]
	if !ok {
		return
	}
	delete(p.cards, id)
	card.SetParent(nil)
	card.DeleteLater()
	p.approvals.SetVisible(len(p.cards) > 0)
}

// When the shown session changes, re-render which approval cards apply.
func (p *agentsPanel) refreshApprovalCards() {
	for id := range p.cards {
		p.removeCard(id)
	}
	p.syncApprovals()
}
