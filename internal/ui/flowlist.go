package ui

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/capture"
	"reproxy/internal/filter"
)

// flowModel mirrors the capture.Store for the request list. It must only be
// touched on the Qt main thread; store events are marshalled there by the
// main window.
//
// It keeps every flow in all (arrival order) but only exposes the rows that
// pass the current filter, via visible (indices into all, kept ascending).
// The view, delegate and selection all work in visible-row coordinates.
type flowModel struct {
	*qt.QAbstractListModel
	all     []capture.Flow
	rows    map[uint64]int  // flow ID -> index into all
	visible []int           // indices into all that pass the filter
	held    int             // flows currently held by interception (all, not just visible)
	matcher *filter.Matcher // nil means match everything
	compact bool            // compact (single-line) rows
	// showDNS lists DNS lookups; they are hidden otherwise, except while
	// held, so a held lookup can't wait unseen.
	showDNS bool
	dns     int // DNS lookups in all, so counts can leave hidden ones out
	// hiddenMethods are the request methods the header's method filter
	// turns off, keyed by bucket (see methodBucket). DNS lookups have the
	// DNS toggle of their own and are never filtered by method.
	hiddenMethods map[string]bool
	// methods counts the flows in each bucket, kept up to date as they
	// arrive so the filter's menu can label its checkboxes without walking
	// the list.
	methods map[string]int
	// wide lays rows out as columns (see wideColumns), for while the list has
	// the whole width of the Traffic section to itself.
	wide bool

	// pulses is a transient background flash per flow ID, used to confirm an
	// action (forward, drop) on the row it affected. The main window animates
	// and prunes them; the delegate reads pulseAt while painting.
	pulses map[uint64]pulseState

	// targetColor resolves a flow's target to its colour, for the tab on the
	// left of each row. It returns "" when the flow belongs to no target, or
	// to one that has since been deleted.
	targetColor func(id string) string
}

// pulseState is one in-progress row flash.
type pulseState struct {
	start time.Time
	hex   string
}

const (
	pulseDuration = 650 * time.Millisecond
	pulseMaxAlpha = 64
)

// pulseAt returns the current tint colour and alpha for a flow's row flash, or
// ok=false when it isn't pulsing (or has faded out).
func (m *flowModel) pulseAt(id uint64) (hex string, alpha int, ok bool) {
	p, has := m.pulses[id]
	if !has {
		return "", 0, false
	}
	elapsed := time.Since(p.start)
	if elapsed >= pulseDuration {
		return "", 0, false
	}
	// Ease out: brightest immediately, fading to nothing — reads as a flash.
	a := int(float64(pulseMaxAlpha) * (1 - float64(elapsed)/float64(pulseDuration)))
	if a <= 0 {
		return "", 0, false
	}
	return p.hex, a, true
}

func newFlowModel() *flowModel {
	m := &flowModel{
		QAbstractListModel: qt.NewQAbstractListModel(),
		rows:               make(map[uint64]int),
		pulses:             make(map[uint64]pulseState),
		hiddenMethods:      make(map[string]bool),
		methods:            make(map[string]int),
	}
	m.OnRowCount(func(parent *qt.QModelIndex) int {
		if parent.IsValid() {
			return 0
		}
		return len(m.visible)
	})
	m.OnData(func(idx *qt.QModelIndex, role int) *qt.QVariant {
		f, ok := m.at(idx.Row())
		if !ok {
			return qt.NewQVariant()
		}
		switch qt.ItemDataRole(role) {
		case qt.DisplayRole, qt.ToolTipRole:
			return qt.NewQVariant14(f.Method + " " + f.URL())
		}
		return qt.NewQVariant()
	})
	return m
}

// at returns the flow at a visible row.
func (m *flowModel) at(row int) (capture.Flow, bool) {
	if row < 0 || row >= len(m.visible) {
		return capture.Flow{}, false
	}
	return m.all[m.visible[row]], true
}

// matches reports whether f passes the current filter.
func (m *flowModel) matches(f capture.Flow) bool {
	if f.IsDNS() {
		if !m.showDNS && !f.State.Held() {
			return false
		}
	} else if m.hiddenMethods[methodBucket(f.Method)] && !f.State.Held() {
		return false
	}
	return m.matcher == nil || m.matcher.Match(f.SearchText())
}

// listedMethods are the buckets the method filter offers, in the order it
// lists them. Anything else a request is sent with (TRACE, a WebDAV verb, a
// made-up one) counts as methodOther, so nothing can be filtered out
// without a box to bring it back.
var listedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", methodOther}

const methodOther = "other"

// methodBucket is the filter bucket a request method falls in.
func methodBucket(method string) string {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return method
	default:
		return methodOther
	}
}

// methodCount is how many flows the list holds with a bucket's method, DNS
// lookups aside (they have the DNS toggle of their own).
func (m *flowModel) methodCount(bucket string) int { return m.methods[bucket] }

// setHiddenMethods hides exactly the given buckets and shows the rest,
// returning whether anything changed.
func (m *flowModel) setHiddenMethods(hidden map[string]bool) bool {
	if len(hidden) == len(m.hiddenMethods) {
		same := true
		for k, v := range hidden {
			if m.hiddenMethods[k] != v {
				same = false
				break
			}
		}
		if same {
			return false
		}
	}
	m.hiddenMethods = hidden
	return true
}

// hiddenMethodIDs lists the hidden buckets in the filter's order, for saving.
func (m *flowModel) hiddenMethodIDs() []string {
	var out []string
	for _, id := range listedMethods {
		if m.hiddenMethods[id] {
			out = append(out, id)
		}
	}
	return out
}

// visibleRow finds flow index (into all) in visible. ok reports membership;
// pos is its visible row when present, or the row it would insert at to keep
// visible ascending when absent.
func (m *flowModel) visibleRow(index int) (pos int, ok bool) {
	pos = sort.SearchInts(m.visible, index)
	return pos, pos < len(m.visible) && m.visible[pos] == index
}

func (m *flowModel) apply(e capture.Event) {
	switch e.Kind {
	case capture.WSMessageAdded:
		// A WebSocket message doesn't change the row, but the stored flow
		// must carry it so reopening the flow shows the message.
		if index, ok := m.rows[e.Flow.ID]; ok {
			m.all[index] = e.Flow
		}
		return
	case capture.FlowAdded:
		index := len(m.all)
		m.all = append(m.all, e.Flow)
		m.rows[e.Flow.ID] = index
		if e.Flow.State.Held() {
			m.held++
		}
		if e.Flow.IsDNS() {
			m.dns++
		} else {
			m.methods[methodBucket(e.Flow.Method)]++
		}
		if m.matches(e.Flow) {
			row := len(m.visible) // new flows sort last, so append
			m.BeginInsertRows(qt.NewQModelIndex(), row, row)
			m.visible = append(m.visible, index)
			m.EndInsertRows()
		}
	case capture.FlowUpdated:
		index, ok := m.rows[e.Flow.ID]
		if !ok {
			return
		}
		if m.all[index].State.Held() {
			m.held--
		}
		if e.Flow.State.Held() {
			m.held++
		}
		m.all[index] = e.Flow
		row, wasVisible := m.visibleRow(index)
		switch nowMatches := m.matches(e.Flow); {
		case wasVisible && nowMatches:
			idx := m.Index(row, 0, qt.NewQModelIndex())
			m.DataChanged(idx, idx)
		case wasVisible && !nowMatches:
			m.BeginRemoveRows(qt.NewQModelIndex(), row, row)
			m.visible = append(m.visible[:row], m.visible[row+1:]...)
			m.EndRemoveRows()
		case !wasVisible && nowMatches:
			m.BeginInsertRows(qt.NewQModelIndex(), row, row)
			m.visible = append(m.visible, 0)
			copy(m.visible[row+1:], m.visible[row:])
			m.visible[row] = index
			m.EndInsertRows()
		}
	case capture.FlowsReset:
		// The whole list was replaced at once, as when an engagement's saved
		// traffic is loaded: rebuild in one reset rather than row by row.
		m.BeginResetModel()
		m.all = append(m.all[:0], e.Flows...)
		m.rows = make(map[uint64]int, len(m.all))
		m.visible = m.visible[:0]
		m.held, m.dns = 0, 0
		clear(m.methods)
		for i, f := range m.all {
			m.rows[f.ID] = i
			if f.State.Held() {
				m.held++
			}
			if f.IsDNS() {
				m.dns++
			} else {
				m.methods[methodBucket(f.Method)]++
			}
			if m.matches(f) {
				m.visible = append(m.visible, i)
			}
		}
		m.EndResetModel()
	case capture.FlowsCleared:
		m.BeginResetModel()
		m.all = nil
		m.rows = make(map[uint64]int)
		m.visible = nil
		m.held, m.dns = 0, 0
		clear(m.methods)
		m.EndResetModel()
	}
}

// setFilter applies a new matcher and rebuilds the visible rows. Returns the
// number of rows now shown. Selection is the caller's concern (a reset clears
// it).
func (m *flowModel) setFilter(matcher *filter.Matcher) int {
	m.BeginResetModel()
	m.matcher = matcher
	m.visible = m.visible[:0]
	for i, f := range m.all {
		if m.matches(f) {
			m.visible = append(m.visible, i)
		}
	}
	m.EndResetModel()
	return len(m.visible)
}

// listed is how many flows the list counts: all of them, less the DNS
// lookups while those are hidden.
func (m *flowModel) listed() int {
	if m.showDNS {
		return len(m.all)
	}
	return len(m.all) - m.dns
}

// filtered reports whether a filter is hiding any rows, so the footer can
// count what is shown out of what is held. The DNS toggle isn't one of
// them: hidden lookups are left out of the total instead (see listed).
func (m *flowModel) filtered() bool {
	return len(m.hiddenMethods) > 0 || (m.matcher != nil && !m.matcher.MatchesAll())
}

// rowOf returns the visible row of the flow with the given ID. ok is false
// when it is unknown or hidden by the filter.
func (m *flowModel) rowOf(id uint64) (int, bool) {
	index, ok := m.rows[id]
	if !ok {
		return 0, false
	}
	return m.visibleRow(index)
}

// firstHeld returns the visible row of the oldest held flow, or -1. A held
// flow hidden by the filter is skipped (the status bar still counts it).
func (m *flowModel) firstHeld() int {
	if m.held == 0 {
		return -1
	}
	for row, index := range m.visible {
		if m.all[index].State.Held() {
			return row
		}
	}
	return -1
}

// Row geometry for the request list. Comfortable rows show two lines (path and
// host/meta); compact rows pack method, path and status onto one.
const (
	rowHeight        = 54
	rowHeightCompact = 30
	rowHeightWide    = 38 // full-width rows are one line, a little roomier than compact
	rowPadX          = 14
	// targetTab is the square on the left of a row, coloured like the target
	// the request belongs to. The column is always reserved so rows line up
	// whether or not targets are in use.
	targetTabSize = 8
	targetTabCol  = 14
	rowLine1Y     = 7
	rowLineGap    = 4
	accentWidth   = 3
	badgePadX     = 7 // horizontal padding inside a chip/pill
	badgeGap      = 10
	// compactThreshold auto-switches the list to compact once this many
	// requests have been captured.
	compactThreshold = 50
)

// tintColor returns the palette colour hex at the given alpha (0–255), for the
// translucent fills behind chips, pills and the row pulse.
func tintColor(hex string, alpha int) *qt.QColor {
	c := qt.NewQColor6(hex)
	c.SetAlpha(alpha)
	return c
}

// newFlowDelegate paints each request. Method and status are rendered as a
// chip and a pill — a tinted shape as well as a colour — so they don't rely on
// hue alone. Comfortable (default):
//
//	[GET]  /path?query                        (200)
//	       localhost:3000                 12 ms · 1.2 KB
//
// Compact (model.compact) folds it to one line:
//
//	[GET]  /path?query          localhost:3000     (200)
//
// Wide (model.wide, the list alone at full width) lays the same facts out in
// columns under a header (see newFlowHeader and wideColumns):
//
//	14:02:11  [GET]  localhost:3000  /path?query      (200)   12 ms   1.2 KB
func newFlowDelegate(model *flowModel, view *qt.QListView) *qt.QStyledItemDelegate {
	d := qt.NewQStyledItemDelegate2(view.QObject)

	baseFont := view.Font()
	smallFont := qt.NewQFont5(baseFont)
	smallFont.SetPointSizeF(baseFont.PointSizeF() * 0.9)
	chipFont := qt.NewQFont5(baseFont)
	chipFont.SetWeight(qt.QFont__DemiBold)
	chipFont.SetPointSizeF(baseFont.PointSizeF() * 0.84)

	baseFM := qt.NewQFontMetrics(baseFont)
	smallFM := qt.NewQFontMetrics(smallFont)
	chipFM := qt.NewQFontMetrics(chipFont)

	// Reserve fixed columns for the widest method and status labels so the path
	// and host line up across rows regardless of chip width.
	methodCol := chipFM.HorizontalAdvance("DELETE") + badgePadX*2
	statusCol := chipFM.HorizontalAdvance("HELD") + badgePadX*2

	d.OnSizeHint(func(_ func(*qt.QStyleOptionViewItem, *qt.QModelIndex) *qt.QSize, _ *qt.QStyleOptionViewItem, _ *qt.QModelIndex) *qt.QSize {
		switch {
		case model.compact:
			return qt.NewQSize2(200, rowHeightCompact)
		case model.wide:
			return qt.NewQSize2(200, rowHeightWide)
		}
		return qt.NewQSize2(200, rowHeight)
	})

	// paintBadge draws text in a rounded badge centred vertically on cy. When
	// alignRight, x is the badge's right edge; otherwise its left edge. filled
	// tints the background (status pill); otherwise a soft border (method chip).
	paintBadge := func(p *qt.QPainter, x, cy int, alignRight, filled bool, text, fg string) {
		tw := chipFM.HorizontalAdvance(text)
		bw := tw + badgePadX*2
		bh := chipFM.Height() + 2
		bx := x
		if alignRight {
			bx = x - bw
		}
		rect := qt.NewQRectF4(float64(bx), float64(cy-bh/2), float64(bw), float64(bh))
		if filled {
			p.SetPen(tintColor(fg, 0)) // no visible border
			p.SetBrush(qt.NewQBrush3(tintColor(fg, 30)))
		} else {
			p.SetPen(tintColor(fg, 150)) // soft 1px border
			p.SetBrush(qt.NewQBrush3(tintColor(fg, 16)))
		}
		p.DrawRoundedRect(rect, 5, 5)
		p.SetFont(chipFont)
		p.SetPen(color(fg))
		p.DrawText5(rect, int(qt.AlignCenter), text)
	}

	d.OnPaint(func(_ func(*qt.QPainter, *qt.QStyleOptionViewItem, *qt.QModelIndex), p *qt.QPainter, opt *qt.QStyleOptionViewItem, idx *qt.QModelIndex) {
		f, ok := model.at(idx.Row())
		if !ok {
			return
		}
		r := opt.Rect()
		state := opt.State()
		selected := state&qt.QStyle__State_Selected != 0
		hovered := state&qt.QStyle__State_MouseOver != 0

		p.Save()
		defer p.Restore()
		p.SetRenderHint(qt.QPainter__Antialiasing)

		switch {
		case selected:
			p.FillRect6(r, color(colorSelected))
			p.FillRect5(r.X(), r.Y(), accentWidth, r.Height(), color(colorAccent))
		case hovered:
			p.FillRect6(r, color(colorHover))
		}
		// Transient action flash (forward/drop), over the background.
		if hex, a, pulsing := model.pulseAt(f.ID); pulsing {
			p.FillRect6(r, tintColor(hex, a))
		}
		p.FillRect5(r.X(), r.Y()+r.Height()-1, r.Width(), 1, color(colorDivider))

		// The target's colour, as a small square in its own column.
		if model.targetColor != nil && f.TargetID != "" {
			if hexColor := model.targetColor(f.TargetID); hexColor != "" {
				y := r.Y() + (r.Height()-targetTabSize)/2
				p.SetPenWithStyle(qt.NoPen)
				p.SetBrush(qt.NewQBrush3(qt.NewQColor6(hexColor)))
				p.DrawRoundedRect(qt.NewQRectF4(
					float64(r.X()+rowPadX-3), float64(y),
					targetTabSize, targetTabSize), 2, 2)
			}
		}

		left := r.X() + rowPadX + targetTabCol
		right := r.X() + r.Width() - rowPadX
		vcenterRight := int(qt.AlignRight | qt.AlignVCenter)
		vcenter := int(qt.AlignLeft | qt.AlignVCenter)

		status, statusColor := statusLabel(f)
		pathX := left + methodCol + badgeGap
		pathRight := right - statusCol - badgeGap

		// sourceTag marks a replayed request ahead of its path, and returns
		// where the path starts after it.
		sourceTag := func(x, cy int) int {
			text, tone, ok := sourceLabel(f.Source)
			if !ok {
				return x
			}
			paintBadge(p, x, cy, false, true, text, tone)
			return x + chipFM.HorizontalAdvance(text) + badgePadX*2 + 6
		}

		if model.wide {
			cy := r.Y() + r.Height()/2
			cols := wideColumns(left, right, methodCol, statusCol)
			text := func(c wideColumn, font *qt.QFont, fm *qt.QFontMetrics, ink, s string, align int, mode qt.TextElideMode) {
				p.SetFont(font)
				p.SetPen(color(ink))
				p.DrawText7(c.x, cy-fm.Height()/2, c.w, fm.Height(), align,
					fm.ElidedText(s, mode, c.w))
			}
			text(cols.time, smallFont, smallFM, colorMuted, f.Started.Format("15:04:05"), vcenter, qt.ElideRight)
			paintBadge(p, cols.method.x, cy, false, false, f.Method, methodColor(f.Method))
			text(cols.host, smallFont, smallFM, colorMuted, f.Host, vcenter, qt.ElideRight)
			pathCol := cols.path
			if x := sourceTag(pathCol.x, cy); x != pathCol.x {
				pathCol = wideColumn{x, max(0, pathCol.x+pathCol.w-x)}
			}
			text(pathCol, baseFont, baseFM, colorText, f.Path, vcenter, qt.ElideMiddle)
			paintBadge(p, cols.status.x, cy, false, true, status, statusColor)
			duration, size := wideTiming(f)
			text(cols.duration, smallFont, smallFM, colorMuted, duration, vcenterRight, qt.ElideRight)
			text(cols.size, smallFont, smallFM, colorMuted, size, vcenterRight, qt.ElideRight)
			return
		}

		if model.compact {
			cy := r.Y() + r.Height()/2
			paintBadge(p, left, cy, false, false, f.Method, methodColor(f.Method))
			paintBadge(p, right, cy, true, true, status, statusColor)

			// Host trails the path, dimmed, so compact rows still show it.
			host := f.Host
			hostW := smallFM.HorizontalAdvance(host)
			if hostW > 160 {
				hostW = 160
			}
			hostX := pathRight - hostW
			p.SetFont(smallFont)
			p.SetPen(color(colorMuted))
			p.DrawText7(hostX, cy-smallFM.Height()/2, hostW, smallFM.Height(), vcenterRight,
				smallFM.ElidedText(host, qt.ElideRight, hostW))

			pathX = sourceTag(pathX, cy)
			p.SetFont(baseFont)
			p.SetPen(color(colorText))
			pathW := hostX - badgeGap - pathX
			p.DrawText7(pathX, cy-baseFM.Height()/2, pathW, baseFM.Height(), vcenter,
				baseFM.ElidedText(f.Path, qt.ElideRight, pathW))
			return
		}

		line1 := r.Y() + rowLine1Y
		line2 := line1 + baseFM.Height() + rowLineGap
		cy1 := line1 + baseFM.Height()/2

		// Line 1: method chip, path, status pill.
		paintBadge(p, left, cy1, false, false, f.Method, methodColor(f.Method))
		paintBadge(p, right, cy1, true, true, status, statusColor)

		line1X := sourceTag(pathX, cy1) // the host below stays under the tag
		p.SetFont(baseFont)
		p.SetPen(color(colorText))
		pathW := pathRight - line1X
		p.DrawText7(line1X, line1, pathW, baseFM.Height(), vcenter,
			baseFM.ElidedText(f.Path, qt.ElideMiddle, pathW))

		// Line 2: host, timing and size.
		p.SetFont(smallFont)
		p.SetPen(color(colorMuted))
		meta := flowMeta(f)
		metaW := smallFM.HorizontalAdvance(meta)
		p.DrawText7(right-metaW, line2, metaW, smallFM.Height(), vcenterRight, meta)
		hostW := right - metaW - badgeGap - pathX
		p.DrawText7(pathX, line2, hostW, smallFM.Height(), vcenter,
			smallFM.ElidedText(f.Host, qt.ElideRight, hostW))
	})
	return d
}

// wideColumn is one column of the full-width list: its left edge and width.
type wideColumn struct{ x, w int }

// wideLayout places the full-width list's columns between left and right.
// The row painter and the header both use it, so labels sit over their
// values.
type wideLayout struct {
	time, method, host, path, status, duration, size wideColumn
}

const (
	wideTimeCol     = 64
	wideHostMin     = 140
	wideHostMax     = 260
	wideDurationCol = 64
	wideSizeCol     = 64
)

// wideColumns lays the columns out. methodCol and statusCol are the widths of
// the widest method chip and status pill. The host takes a quarter of the
// width (within limits) and the path everything that is left.
func wideColumns(left, right, methodCol, statusCol int) wideLayout {
	var l wideLayout
	x := left
	next := func(w int) wideColumn {
		c := wideColumn{x, w}
		x += w + badgeGap*2
		return c
	}
	l.time = next(wideTimeCol)
	l.method = next(methodCol)
	hostW := (right - left) / 4
	hostW = max(wideHostMin, min(wideHostMax, hostW))
	l.host = next(hostW)

	// The fixed columns on the right, laid out from the right edge.
	l.size = wideColumn{right - wideSizeCol, wideSizeCol}
	l.duration = wideColumn{l.size.x - badgeGap*2 - wideDurationCol, wideDurationCol}
	l.status = wideColumn{l.duration.x - badgeGap*2 - statusCol, statusCol}
	l.path = wideColumn{x, max(0, l.status.x-badgeGap*2-x)}
	return l
}

// wideTiming is the Duration and Size columns' text: blank until the
// exchange completes.
func wideTiming(f capture.Flow) (duration, size string) {
	if f.State != capture.StateComplete {
		return "", ""
	}
	return formatDuration(f.Duration), capture.FormatBytes(f.Response.Size)
}

// newFlowHeader is the column header shown above the full-width list. It is
// painted to match the delegate's columns (wideColumns) rather than being a
// QHeaderView, since the list is a QListView with custom-painted rows.
func newFlowHeader(view *qt.QListView) *qt.QWidget {
	h := qt.NewQWidget2()
	setName(h.QObject, "FlowHeader")
	h.SetAttribute(qt.WA_StyledBackground)

	font := qt.NewQFont5(view.Font())
	font.SetPointSizeF(font.PointSizeF() * 0.84)
	font.SetWeight(qt.QFont__DemiBold)
	fm := qt.NewQFontMetrics(font)
	chipFont := qt.NewQFont5(view.Font())
	chipFont.SetWeight(qt.QFont__DemiBold)
	chipFont.SetPointSizeF(view.Font().PointSizeF() * 0.84)
	chipFM := qt.NewQFontMetrics(chipFont)
	methodCol := chipFM.HorizontalAdvance("DELETE") + badgePadX*2
	statusCol := chipFM.HorizontalAdvance("HELD") + badgePadX*2

	h.SetFixedHeight(fm.Height() + 12)
	h.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) {
		p := qt.NewQPainter2(h.QPaintDevice)
		defer p.End()
		// Line the columns up with the rows: the viewport sits inside the
		// list's frame, and the rows end where the viewport does.
		vp := view.Viewport()
		left := vp.X() + rowPadX + targetTabCol
		right := vp.X() + vp.Width() - rowPadX
		cols := wideColumns(left, right, methodCol, statusCol)
		p.SetFont(font)
		p.SetPen(color(colorMuted))
		vcenter := int(qt.AlignLeft | qt.AlignVCenter)
		vright := int(qt.AlignRight | qt.AlignVCenter)
		for _, c := range []struct {
			col   wideColumn
			label string
			align int
		}{
			{cols.time, "Time", vcenter},
			{cols.method, "Method", vcenter},
			{cols.host, "Host", vcenter},
			{cols.path, "Path", vcenter},
			{cols.status, "Status", vcenter},
			{cols.duration, "Duration", vright},
			{cols.size, "Size", vright},
		} {
			p.DrawText7(c.col.x, 0, c.col.w, h.Height(), c.align,
				fm.ElidedText(c.label, qt.ElideRight, c.col.w))
		}
		p.FillRect5(0, h.Height()-1, h.Width(), 1, color(colorDivider))
	})
	// The list resizing, or its scroll bar coming and going, changes the
	// viewport's width, and with it where the right-hand columns sit.
	view.VerticalScrollBar().OnRangeChanged(func(int, int) { h.Update() })
	view.OnResizeEvent(func(super func(*qt.QResizeEvent), e *qt.QResizeEvent) {
		super(e)
		h.Update()
	})
	return h
}

func statusLabel(f capture.Flow) (string, string) {
	switch f.State {
	case capture.StatePending:
		return "…", colorMuted
	case capture.StateFailed:
		return "ERR", colorServerError
	case capture.StateHeldRequest, capture.StateHeldResponse:
		return "HELD", colorHeld
	case capture.StateDropped:
		return "DROP", colorMuted
	}
	if f.IsDNS() {
		// The badge is four characters wide; the full status (NXDOMAIN,
		// SERVFAIL…) is in the answer.
		switch f.StatusText {
		case "NOERROR":
			return "OK", colorSuccess
		case "NXDOMAIN":
			return "NX", colorClientError
		case "UPSTREAM":
			return "UP", colorMuted
		}
		return "FAIL", colorServerError
	}
	c := colorMuted
	switch {
	case f.StatusCode >= 500:
		c = colorServerError
	case f.StatusCode >= 400:
		c = colorClientError
	case f.StatusCode >= 300:
		c = colorRedirect
	case f.StatusCode >= 200:
		c = colorSuccess
	}
	return strconv.Itoa(f.StatusCode), c
}

// sourceLabel is the tag a replayed request carries in the list: who sent
// it. Traffic through the proxy has none (ok is false).
func sourceLabel(s capture.Source) (text, tone string, ok bool) {
	switch s {
	case capture.SourceReplay:
		return "REPLAY", colorAccent, true
	case capture.SourceAgent:
		return "AI", colorHeld, true
	case capture.SourceCrawl:
		return "CRAWL", colorRedirect, true
	}
	return "", "", false
}

func methodColor(method string) string {
	switch method {
	case "GET", "HEAD":
		return colorMethodGet
	case "POST":
		return colorMethodPost
	case "PUT", "PATCH":
		return colorMethodPut
	case "DELETE":
		return colorMethodDelete
	}
	return colorMethodOther
}

func flowMeta(f capture.Flow) string {
	var meta string
	switch f.State {
	case capture.StatePending:
		meta = f.Started.Format("15:04:05")
	case capture.StateHeldRequest:
		meta = "request held"
	case capture.StateHeldResponse:
		meta = "response held"
	case capture.StateDropped:
		meta = f.Error
	default:
		meta = fmt.Sprintf("%s · %s", formatDuration(f.Duration), capture.FormatBytes(f.Response.Size))
	}
	if f.Edited() {
		meta += " · edited"
	}
	return meta
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1 ms"
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2f s", d.Seconds())
}
