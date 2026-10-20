package ui

import (
	"fmt"
	"html"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/capture"
	"reproxy/internal/findings"
)

// detailPane is the right-hand column: a header (summary, and Forward/Drop
// while a message is held) above Request and Response tabs. Each tab is a
// structured messageView: start line, a key/value header editor, and a
// language-aware body editor.
//
// While a message is held it is editable; Forward serializes the edited
// parts and sends them on (see intercept.ApplyEdit), Drop discards it.
type detailPane struct {
	root    *qt.QStackedWidget
	empty   *qt.QLabel
	summary *qt.QLabel
	// target names the target the shown request fell in, tinted with that
	// target's colour. It is empty for traffic collected without one.
	target  *qt.QLabel
	badge   *qt.QLabel
	replay  *qt.QPushButton
	forward *qt.QPushButton
	drop    *qt.QPushButton
	tabs    *qt.QTabWidget
	// pages is the shared Pretty/Raw/HTML selector in the tab row, driving whichever
	// message (Request or Response) is in view.
	pages *qt.QComboBox
	// search finds text in the shown request and response.
	search *messageSearch

	// messagePair carries request/response and the Edit-menu and theme
	// routing over both of them.
	messagePair
	messages *messagesView

	shownID    uint64
	shownState capture.FlowState
	shownFlow  capture.Flow

	// onForward and onDrop are called with the shown flow's ID; onForward
	// also gets the edited message text of the held message. onReplay opens
	// the Replay tool templated from the shown flow.
	onForward func(id uint64, edited string)
	onDrop    func(id uint64)
	onReplay  func(capture.Flow)
	// onClose closes the pane (it slides away and the request list takes
	// the full width back). The pane shows a close button on both pages.
	onClose func()
	// onFinding is called when the user raises a finding from a selection in
	// the shown message: the draft already carries the value and the request
	// it was seen in, and the window opens the Create Finding dialog on it.
	onFinding func(findings.Draft)
	// targetOf resolves a flow's target to a label and colour for the tag;
	// ok is false when it belongs to none.
	targetOf func(id string) (label, color string, ok bool)
}

const (
	detailPageEmpty = 0
	detailPageFlow  = 1

	tabRequest  = 0
	tabResponse = 1
	tabMessages = 2
)

func newDetailPane() *detailPane {
	d := &detailPane{root: qt.NewQStackedWidget2()}

	// Empty state, with the close button in the corner where the flow
	// page's header has it.
	emptyPage := qt.NewQWidget2()
	emptyLayout := qt.NewQVBoxLayout(emptyPage)
	emptyLayout.SetContentsMargins(0, 8, 10, 0)
	closeRow := qt.NewQHBoxLayout2()
	closeRow.AddStretch()
	closeRow.AddWidget(d.closeButton())
	emptyLayout.AddLayout(closeRow.QLayout)
	emptyLayout.AddStretch()
	title := qt.NewQLabel3("No request selected")
	setName(title.QObject, "EmptyTitle")
	title.SetAlignment(qt.AlignCenter)
	d.empty = qt.NewQLabel2()
	setName(d.empty.QObject, "EmptyState")
	d.empty.SetAlignment(qt.AlignCenter)
	d.empty.SetWordWrap(true)
	emptyLayout.AddWidget(title.QWidget)
	emptyLayout.AddSpacing(6)
	emptyLayout.AddWidget(d.empty.QWidget)
	emptyLayout.AddStretch()

	// Flow view.
	flowPage := qt.NewQWidget2()
	flowLayout := qt.NewQVBoxLayout(flowPage)
	flowLayout.SetContentsMargins(0, 0, 0, 0)
	flowLayout.SetSpacing(0)

	flowLayout.AddWidget(d.buildHeader())

	d.request = newMessageView(false)
	d.response = newMessageView(false)
	// Right-clicking a header or body here can raise a finding; the view
	// says which part the text came from, the pane knows which request and
	// which half of it (see raiseFrom).
	d.request.onFinding = d.raiseFrom(false)
	d.response.onFinding = d.raiseFrom(true)
	// Headers take most of the height here by default; the user can drag the
	// splitter to give the body more room.
	d.request.splitHeaderMajority()
	d.response.splitHeaderMajority()
	d.search.watch(d.request, d.response)
	d.tabs = qt.NewQTabWidget2()
	d.tabs.SetDocumentMode(true)
	d.tabs.AddTab(d.request.root, "Request")
	d.tabs.AddTab(d.response.root, "Response")
	d.messages = newMessagesView() // its tab is added only for WebSocket flows

	// One Pretty/Raw/HTML selector in the tab row's corner, instead of a toolbar in
	// each view, controls the message on show. It saves the vertical space the
	// per-view toolbars took.
	d.pages = qt.NewQComboBox2()
	d.pages.AddItems(viewModes)
	d.pages.OnCurrentIndexChanged(func(i int) { d.currentMessageView().showMode(viewMode(i)) })
	d.tabs.SetCornerWidget(d.pages.QWidget)
	d.tabs.OnCurrentChanged(func(int) { d.syncPages() })
	flowLayout.AddWidget(d.tabs.QWidget)

	d.root.AddWidget(emptyPage)
	d.root.AddWidget(flowPage)
	d.root.SetCurrentIndex(detailPageEmpty)
	return d
}

func (d *detailPane) buildHeader() *qt.QWidget {
	header := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 8, 10, 8)
	h.SetSpacing(8)

	d.summary = qt.NewQLabel2()
	setName(d.summary.QObject, "DetailSummary")
	d.summary.SetTextInteractionFlags(qt.TextSelectableByMouse)
	d.summary.SetMinimumWidth(80)
	// Long URLs shouldn't push the buttons off screen.
	d.summary.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)

	d.target = qt.NewQLabel2()
	setName(d.target.QObject, "TargetTag")
	d.target.SetTextFormat(qt.RichText)
	d.target.Hide()

	d.badge = qt.NewQLabel2()
	setName(d.badge.QObject, "HeldBadge")
	d.replay = qt.NewQPushButton3("Replay")
	d.replay.SetAutoDefault(false)
	d.replay.SetToolTip("Open this request in the Replay tool to edit and resend it")
	d.replay.OnClicked(func() {
		if d.onReplay != nil && d.shownFlow.ID != 0 {
			d.onReplay(d.shownFlow)
		}
	})
	d.drop = qt.NewQPushButton3("Drop")
	d.drop.SetAutoDefault(false)
	d.drop.SetToolTip("Discard the message and close the browser's connection")
	d.forward = qt.NewQPushButton3("Forward")
	setName(d.forward.QObject, "PrimaryButton")
	d.forward.SetAutoDefault(false)
	d.forward.SetToolTip("Send the message on, including your edits")

	d.forward.OnClicked(func() {
		if d.onForward != nil {
			d.onForward(d.shownID, d.heldView().serialize())
		}
	})
	d.drop.OnClicked(func() {
		if d.onDrop != nil {
			d.onDrop(d.shownID)
		}
	})

	h.AddWidget2(d.summary.QWidget, 1)
	h.AddWidget(d.target.QWidget)
	d.search = newMessageSearch()

	h.AddWidget(d.badge.QWidget)
	h.AddWidget(d.search.root)
	h.AddWidget(d.replay.QWidget)
	h.AddWidget(d.drop.QWidget)
	h.AddWidget(d.forward.QWidget)
	h.AddWidget(d.closeButton())
	// Keep the height constant whether or not the buttons are showing.
	header.SetFixedHeight(d.forward.SizeHint().Height() + 16)
	d.setHeldControls(false)
	return header
}

// closeButton returns a new close (×) button wired to onClose.
func (d *detailPane) closeButton() *qt.QWidget {
	return newIconButton(iconClose, "Close (Esc in the request list)", func() {
		if d.onClose != nil {
			d.onClose()
		}
	}).w
}

// setHint updates the empty-state text, e.g. with the proxy address.
func (d *detailPane) setHint(text string) {
	d.empty.SetText(text)
}

// show displays f. Re-showing the flow that is already shown only reloads it
// if its state changed, so edits in progress aren't lost.
func (d *detailPane) show(f capture.Flow) {
	if f.ID == d.shownID && f.State == d.shownState {
		return
	}
	newFlow := f.ID != d.shownID
	d.shownID, d.shownState, d.shownFlow = f.ID, f.State, f

	d.summary.SetText(f.Method + "  " + f.URL())
	d.summary.SetToolTip(f.URL())
	d.showTarget(f)

	switch f.State {
	case capture.StateHeldRequest:
		d.request.setMessage(f.Request.Raw, true)
		d.response.setPlaceholder("The response will appear here once the request is forwarded.")
		d.tabs.SetCurrentIndex(tabRequest)
		d.badge.SetText(heldLabel(f, "Request held", "Lookup held"))
	case capture.StateHeldResponse:
		d.request.setMessage(f.Request.Raw, false)
		d.response.setMessage(f.Response.Raw, true)
		d.tabs.SetCurrentIndex(tabResponse)
		d.badge.SetText(heldLabel(f, "Response held", "Answer held"))
	default:
		d.request.setMessage(f.Request.Raw, false)
		switch f.State {
		case capture.StatePending:
			d.response.setPlaceholder("Waiting for response…")
		case capture.StateFailed, capture.StateDropped:
			d.response.setPlaceholder("No response: " + f.Error)
		default:
			d.response.setMessage(f.Response.Raw, false)
		}
		if newFlow {
			d.tabs.SetCurrentIndex(tabRequest)
		}
	}
	d.showMessages(f)
	d.setHeldControls(f.State.Held())
	// Replay needs a request to template from; it is offered whenever a flow
	// with a recorded request is shown, held or not.
	d.replay.SetVisible(len(f.Request.Raw) > 0 && !f.IsDNS()) // a lookup isn't HTTP to resend
	// setMessage resets each view to Pretty; point the shared selector at the
	// visible tab's mode.
	d.syncPages()
	d.root.SetCurrentIndex(detailPageFlow)
}

// showMessages shows or hides the Messages tab: it is there only for a
// WebSocket flow, with the messages it has so far.
func (d *detailPane) showMessages(f capture.Flow) {
	has := len(f.Messages) > 0
	if i := d.tabs.IndexOf(d.messages.root); has && i < 0 {
		d.tabs.AddTab(d.messages.root, "Messages")
	} else if !has && i >= 0 {
		d.tabs.RemoveTab(i)
	}
	if has {
		d.messages.setFlow(f)
	}
}

// wsMessageAdded appends to the Messages tab when a message arrives on the
// shown flow, without disturbing which message is open.
func (d *detailPane) wsMessageAdded(f capture.Flow) {
	if f.ID != d.shownID {
		return
	}
	d.shownFlow = f
	d.showMessages(f)
}

// raiseFrom builds the handler for one half of the exchange: it turns a
// right-click in that view into a finding draft pinned to the shown request.
func (d *detailPane) raiseFrom(response bool) func(string, messagePart, string) {
	return func(value string, part messagePart, detail string) {
		if d.onFinding == nil || d.shownFlow.ID == 0 {
			return
		}
		d.onFinding(findings.Draft{
			Value:  value,
			Source: findings.SourceUser,
			Link: findings.Link{
				FlowID:   d.shownFlow.ID,
				Method:   d.shownFlow.Method,
				URL:      d.shownFlow.URL(),
				Location: findingLocation(response, part),
				Detail:   detail,
			},
		})
	}
}

// findingLocation names the part of the exchange a finding came from, which
// is what the detail pane shows as "response header (Set-Cookie)".
func findingLocation(response bool, part messagePart) string {
	switch {
	case response && part == partBody:
		return findings.InResponseBody
	case response:
		return findings.InResponseHeader
	case part == partBody:
		return findings.InRequestBody
	}
	return findings.InRequestHeader
}

// currentMessageView is the message view behind the visible tab.
func (d *detailPane) currentMessageView() *messageView {
	if d.tabs.CurrentIndex() == tabResponse {
		return d.response
	}
	return d.request
}

// syncPages points the shared Pretty/Raw/HTML selector at the visible
// message's mode, so switching tabs or showing a new flow leaves the two in
// step.
func (d *detailPane) syncPages() {
	idx := int(d.currentMessageView().mode())
	if d.pages.CurrentIndex() != idx {
		d.pages.SetCurrentIndex(idx)
	}
}

// showTarget labels the request with the target it was collected under, in
// that target's colour.
func (d *detailPane) showTarget(f capture.Flow) {
	if d.targetOf == nil || f.TargetID == "" {
		d.target.Hide()
		return
	}
	label, tone, ok := d.targetOf(f.TargetID)
	if !ok {
		d.target.Hide()
		return
	}
	d.target.SetText(fmt.Sprintf(`<span style="color:%s">&#9632;</span>&nbsp; %s`,
		tone, html.EscapeString(label)))
	d.target.SetToolTip("Collected under the target " + label)
	d.target.Show()
}

// refreshTarget re-labels the shown request, after a target is renamed,
// recoloured or deleted.
func (d *detailPane) refreshTarget() {
	if d.shownID != 0 {
		d.showTarget(d.shownFlow)
	}
}

func (d *detailPane) setHeldControls(held bool) {
	d.badge.SetVisible(held)
	d.forward.SetVisible(held)
	d.drop.SetVisible(held)
}

// heldView returns the message view holding the editable message.
func (d *detailPane) heldView() *messageView {
	if d.shownState == capture.StateHeldResponse {
		return d.response
	}
	return d.request
}

// showingHeld reports whether the pane is showing a held message.
func (d *detailPane) showingHeld() bool {
	return d.shownID != 0 && d.shownState.Held()
}

// clear returns to the empty state.
func (d *detailPane) clear() {
	d.shownID, d.shownState, d.shownFlow = 0, 0, capture.Flow{}
	d.target.Hide()
	d.setHeldControls(false)
	d.replay.SetVisible(false)
	d.root.SetCurrentIndex(detailPageEmpty)
}

// currentBody returns the body editor in the visible tab.
func (d *detailPane) currentBody() *qt.QPlainTextEdit {
	if d.tabs.CurrentIndex() == tabResponse {
		return d.response.body.edit
	}
	return d.request.body.edit
}

// heldLabel is the held badge's text: an HTTP message's, or a DNS lookup's.
func heldLabel(f capture.Flow, http, dns string) string {
	if f.IsDNS() {
		return dns
	}
	return http
}
