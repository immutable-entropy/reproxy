package ui

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/capture"
	"reproxy/internal/elide"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

// replayView is one Replay (repeater) tab: a request templated from a
// captured request, editable on the left, the response read-only on the
// right, and Send to fire the current request at the server and show what
// comes back. It can be sent repeatedly.
//
// It connects directly to the target, not through the proxy, so replays don't
// appear in the capture history.
//
// The view is a view onto a core replay.Session: sends go through
// replay.Sessions, so agents can see (and add to) the same history. Several
// views live side by side in a replayPane, which reports the active one to
// the focus tracker so "this request" in an agent chat means the request
// open here.
type replayView struct {
	root *qt.QWidget
	// messagePair carries request/response and the Edit-menu and theme
	// routing over both of them.
	messagePair
	send       *qt.QPushButton
	method     *qt.QComboBox
	historyBtn *qt.QToolButton
	status     *qt.QLabel
	bottom     *qt.QWidget
	history    *replayHistory

	sessions    *replay.Sessions
	session     replay.Session // as opened; History is not kept current
	template    []byte         // original request bytes, for intercept.ApplyEdit
	closed      bool
	unsubscribe func()

	// onSent lets the pane react to a send made elsewhere in this session
	// (by an agent), e.g. to draw attention to the tab.
	onSent func(*replayView, replay.Entry)
	// onFinding is called when the user raises a finding from a selection in
	// the request or the response here.
	onFinding func(findings.Draft)
	// flowID is the captured flow the messages on show belong to: the
	// request this session was templated from until something is sent, then
	// the send itself (replays are recorded in the store too). It is what a
	// finding raised here points at.
	flowID uint64
	// syncingMethod is set while the method selector and the request's
	// start line are put in step, so neither change echoes back.
	syncingMethod bool
}

// replayMethods are offered in the method selector, which also takes any
// other method typed into it (WebDAV's PROPFIND, a made-up one to see how
// the server copes).
var replayMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT"}

// bodyMethods are the methods that usually carry a body: switching to one
// gives a request with no framing header a Content-Length, which ApplyEdit
// then sets to the body's size, so a body typed in is sent as one.
var bodyMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true}

func newReplayView(sessions *replay.Sessions, sess replay.Session) *replayView {
	r := &replayView{
		root:     qt.NewQWidget2(),
		sessions: sessions,
		session:  sess,
		template: sess.Template.Raw,
		flowID:   sess.Template.FlowID,
	}
	r.buildBody(sess)

	// Sends made elsewhere (by an agent working in this session) join the
	// history as they finish.
	r.unsubscribe = sessions.Subscribe(func(e replay.SessionEvent) {
		if e.Session.ID != sess.ID {
			return
		}
		mainthread.Start(func() { r.onSessionEvent(e) })
	})
	return r
}

// close stops listening and releases the session. The user's own sessions
// live as long as their tab; an agent's session outlives a tab opened onto
// it.
func (r *replayView) close() {
	if r.closed {
		return
	}
	r.closed = true
	if r.unsubscribe != nil {
		r.unsubscribe()
	}
	if r.session.Owner == replay.OwnerUser {
		r.sessions.Close(r.session.ID)
	}
}

// tabTitle is the short label for this view's tab: the method and path, which
// is what tells two replays of the same app apart.
func (r *replayView) tabTitle() string {
	t := r.session.Template
	target := t.URL
	if u, err := url.Parse(t.URL); err == nil && u.Path != "" {
		target = u.Path
		if u.RawQuery != "" {
			target += "?" + u.RawQuery
		}
	}
	return t.Method + " " + elide.Tail(target, 26)
}

// tabToolTip is the full target, since the tab title is shortened.
func (r *replayView) tabToolTip() string {
	t := r.session.Template
	tip := t.Method + " " + t.URL
	if r.session.Owner != replay.OwnerUser {
		tip += "\nAn agent's session"
	}
	return tip
}

// draft is the current request text, for the focus tracker. A closed view's
// widgets are on their way out, so it has nothing to report.
func (r *replayView) draft() string {
	if r.closed {
		return ""
	}
	return r.request.serialize()
}

func (r *replayView) buildBody(sess replay.Session) {
	t := sess.Template
	outer := qt.NewQVBoxLayout(r.root)
	outer.SetContentsMargins(0, 0, 0, 0)
	outer.SetSpacing(windowMargin)

	// Header: the method and target on the left, then the history toggle
	// and Send.
	header := qt.NewQWidget2()
	hb := qt.NewQHBoxLayout(header)
	hb.SetContentsMargins(0, 0, 0, 0)
	hb.SetSpacing(8)
	r.method = qt.NewQComboBox2()
	r.method.SetEditable(true)
	r.method.SetInsertPolicy(qt.QComboBox__NoInsert)
	r.method.AddItems(replayMethods)
	r.method.SetMinimumWidth(110)
	r.method.SetToolTip("The request method. Pick one or type your own; the request's first line follows it.")
	target := qt.NewQLabel3(t.URL)
	setName(target.QObject, "DetailSummary")
	target.SetTextInteractionFlags(qt.TextSelectableByMouse)
	target.SetToolTip(t.URL)
	target.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)

	r.historyBtn = qt.NewQToolButton2()
	r.historyBtn.SetText("History")
	r.historyBtn.SetCheckable(true)
	r.historyBtn.SetFocusPolicy(qt.NoFocus)
	r.historyBtn.SetToolTip("Show the sends made in this session")
	setName(r.historyBtn.QObject, "RegexToggle") // same pill look as the filter toggle

	r.send = qt.NewQPushButton3("Send")
	setName(r.send.QObject, "PrimaryButton")
	r.send.SetAutoDefault(false)
	r.send.SetToolTip("Send this request to the server (Ctrl+Enter)")
	r.send.OnClicked(r.doSend)

	hb.AddWidget(r.method.QWidget)
	hb.AddWidget2(target.QWidget, 1)
	hb.AddWidget(r.historyBtn.QWidget)
	hb.AddWidget(r.send.QWidget)
	outer.AddWidget(header)

	// Request | Response panels.
	r.request = newMessageView(true)
	r.request.setMessage(t.Raw, true)
	// The selector and the start line are two ways to change one thing:
	// each follows the other, whichever the user edits.
	r.syncMethod(r.request.startLine.Text())
	r.method.OnCurrentTextChanged(r.setMethod)
	r.request.startLine.OnTextChanged(r.syncMethod)
	r.response = newMessageView(true)
	// Sending again keeps the response in the view the user picked, so a
	// page being probed can be watched as HTML send after send.
	r.response.keepMode = true
	r.response.setPlaceholder("Send the request to see the response here.")
	// A finding is often what a probe from here turns up, so the same
	// right-click works in a Replay window as in the Traffic viewer.
	r.request.onFinding = r.raiseFrom(false)
	r.response.onFinding = r.raiseFrom(true)
	r.request.splitEvenly()
	r.response.splitEvenly()

	split := qt.NewQSplitter3(qt.Horizontal)
	split.SetChildrenCollapsible(false)
	split.SetHandleWidth(windowMargin)
	split.AddWidget(wrapReplayPanel("Request", r.request.root))
	split.AddWidget(wrapReplayPanel("Response", r.response.root))
	split.SetSizes([]int{500, 500})

	// Bottom: the replay history (hidden until the first send).
	r.history = newReplayHistory(r.loadEntry)
	r.bottom = r.history.root
	r.bottom.SetVisible(false)
	r.historyBtn.OnToggled(func(on bool) { r.bottom.SetVisible(on) })
	// A session opened with history (an agent's) shows it, and its latest
	// request and response.
	for _, e := range sess.History {
		r.history.add(e)
	}
	if last, ok := sess.Last(); ok {
		r.loadEntry(last)
		r.historyBtn.SetChecked(true)
	}

	vsplit := qt.NewQSplitter3(qt.Vertical)
	vsplit.SetChildrenCollapsible(false)
	vsplit.SetHandleWidth(windowMargin)
	vsplit.AddWidget(split.QWidget)
	vsplit.AddWidget(r.bottom)
	vsplit.SetStretchFactor(0, 100-bottomShare)
	vsplit.SetStretchFactor(1, bottomShare)
	vsplit.SetSizes([]int{(100 - bottomShare) * 10, bottomShare * 10})
	outer.AddWidget(vsplit.QWidget)

	r.status = qt.NewQLabel3("Ready")
	setName(r.status.QObject, "Muted")
	outer.AddWidget(r.status.QWidget)
}

// setMethod puts the method chosen in the selector into the request's start
// line.
func (r *replayView) setMethod(method string) {
	if r.syncingMethod {
		return
	}
	method = strings.TrimSpace(method)
	line := r.request.startLine.Text()
	next := intercept.WithMethod(line, method)
	if next == line {
		return
	}
	r.syncingMethod = true
	r.request.startLine.SetText(next)
	r.syncingMethod = false
	if bodyMethods[method] && !r.request.hasFraming() {
		r.request.addRow("Content-Length", "0")
	}
}

// syncMethod shows the start line's method in the selector, after the line
// was edited or a past send loaded.
func (r *replayView) syncMethod(line string) {
	if r.syncingMethod {
		return
	}
	method := intercept.RequestMethod(line)
	if method == r.method.CurrentText() {
		return
	}
	r.syncingMethod = true
	r.method.SetCurrentText(method)
	r.syncingMethod = false
}

func wrapReplayPanel(title string, content *qt.QWidget) *qt.QWidget {
	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 10, rowPadX, 10)
	t := qt.NewQLabel3(title)
	setName(t.QObject, "PanelTitle")
	h.AddWidget(t.QWidget)
	h.AddStretch()
	v.AddWidget(header)
	v.AddWidget(content)
	panel.SetMinimumWidth(360)
	return panel.QWidget
}

// doSend builds the current request and sends it, off the main thread. The
// history entry arrives through the session's events, like anyone else's.
func (r *replayView) doSend() {
	raw := intercept.ApplyEdit(r.template, r.request.serialize(), true)
	r.send.SetEnabled(false)
	r.status.SetText("Sending…")

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e, _ := r.sessions.Send(ctx, r.session.ID, raw, replay.OwnerUser)
		mainthread.Start(func() {
			if r.closed {
				return // the tab went away while the request was in flight
			}
			r.send.SetEnabled(true)
			r.showResponse(e)
		})
	}()
}

// onSessionEvent records a finished send in the history.
func (r *replayView) onSessionEvent(e replay.SessionEvent) {
	if r.closed || e.Kind != replay.SessionSent {
		return
	}
	r.history.add(e.Entry)
	if e.Entry.By != replay.OwnerUser {
		r.status.SetText(fmt.Sprintf("An agent sent request #%d · %s", e.Entry.Seq, entryStatus(e.Entry)))
	}
	// The history is hidden until there's something in it.
	if !r.historyBtn.IsChecked() {
		r.historyBtn.SetChecked(true) // its handler shows the pane
	}
	if r.onSent != nil {
		r.onSent(r, e.Entry)
	}
}

// raiseFrom builds the handler for one half of the exchange: it turns a
// right-click into a finding draft pinned to the request on show.
func (r *replayView) raiseFrom(response bool) func(string, messagePart, string) {
	return func(value string, part messagePart, detail string) {
		if r.onFinding == nil {
			return
		}
		t := r.session.Template
		r.onFinding(findings.Draft{
			Value:  value,
			Source: findings.SourceUser,
			Link: findings.Link{
				FlowID:   r.flowID,
				Method:   t.Method,
				URL:      t.URL,
				Location: findingLocation(response, part),
				Detail:   detail,
			},
		})
	}
}

// showResponse fills the response pane and status line from a send.
func (r *replayView) showResponse(e replay.Entry) {
	// The send was recorded in the capture store as a flow of its own, so a
	// finding raised from what came back points at the request that
	// produced it rather than at the original.
	if e.FlowID != 0 {
		r.flowID = e.FlowID
	}
	if e.Err != "" {
		r.response.setPlaceholder("Error: " + e.Err)
	} else {
		r.response.setMessage(e.Response.Raw, false)
	}
	r.status.SetText(entryStatus(e))
}

// entryStatus is the status-line text for a send's result.
func entryStatus(e replay.Entry) string {
	if e.Err != "" {
		return "Failed: " + e.Err
	}
	return fmt.Sprintf("%s · %s · %s", e.Response.Status,
		formatDuration(e.Response.Duration), capture.FormatBytes(int64(e.Response.Size)))
}

// loadEntry restores a past send into the editors so it can be inspected,
// edited and sent again.
func (r *replayView) loadEntry(e replay.Entry) {
	r.request.setMessage(e.Request, true)
	r.showResponse(e)
}
