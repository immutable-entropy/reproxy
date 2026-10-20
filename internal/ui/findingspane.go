package ui

import (
	"fmt"
	"sort"
	"strings"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/filter"
	"reproxy/internal/findings"
)

// findingsPane is the Findings section of the main window: everything the
// work turned up, in one list.
//
// Each row is a finding — its value, the type pills classifying it, and how
// many requests it was seen in. A finding nobody has opened yet is tinted,
// and the count of those is what the navigation bar's Findings badge shows,
// so the badge and the highlighted rows can never disagree. Clicking a row
// opens the detail pane, which slides out from the right the way the Traffic
// viewer does, and marks the finding viewed.
//
// Above the list are the filters: one clickable pill per configured type,
// and a text box matched against each finding's value, notes, fields and
// sightings (through internal/filter, the same matcher the request list
// uses).
//
// The findings shown belong to the open engagement; opening another one
// replaces them, which arrives here as a store event like any other. Those
// events can come from an agent's goroutine, so they are handed to the main
// thread before anything is touched.
type findingsPane struct {
	root  *qt.QWidget
	store *findings.Store

	list          *qt.QListWidget
	count         *qt.QLabel
	unseen        *qt.QLabel
	pages         *qt.QStackedWidget // 0: empty state, 1: the list
	markRead      *qt.QPushButton    // marks every finding read
	filters       *qt.QWidget        // the type pills' row
	filtersBox    *qt.QHBoxLayout
	pills         []*pill
	chosen        map[string]bool // type IDs the filter is limited to
	search        *qt.QLineEdit
	confFilter    *scoreFilter // hide findings scored below a confidence
	concernFilter *scoreFilter // hide findings scored below a concern

	split       *qt.QSplitter
	detail      *findingDetail
	detailPanel *qt.QWidget
	detailOpen  bool
	detailWidth int
	anim        *qt.QVariantAnimation

	// shown maps a list row to the finding it draws, and current is the
	// selected finding, kept across rebuilds so a change elsewhere doesn't
	// move the selection.
	shown   []uint64
	current uint64
	// rebuilding guards the selection handler while rows are replaced.
	rebuilding bool

	unsubscribe func()
	// onShowFlow is asked to bring a request into view in the Traffic
	// section, for the "show" button beside a sighting.
	onShowFlow func(flowID uint64)
	// onShowFile is asked to bring a recovered file into view in the Files
	// section, for the "open" button beside a recovered-file sighting.
	onShowFile func(path string)
	// onChanged is called after any change, so the window can update the
	// navigation badge.
	onChanged func()
}

const (
	findingRowHeight = 58
	findingListShare = 42 // percent of the section's width the detail pane takes
)

func newFindingsPane(store *findings.Store, onShowFlow func(uint64), onShowFile func(string), onChanged func()) *findingsPane {
	p := &findingsPane{
		root:       qt.NewQWidget2(),
		store:      store,
		chosen:     map[string]bool{},
		onShowFlow: onShowFlow,
		onShowFile: onShowFile,
		onChanged:  onChanged,
	}

	layout := qt.NewQVBoxLayout(p.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.SetSpacing(0)

	p.detail = newFindingDetail(p.detailActions())
	p.detailPanel = p.detail.root

	p.split = qt.NewQSplitter3(qt.Horizontal)
	p.split.SetChildrenCollapsible(false)
	p.split.SetHandleWidth(windowMargin)
	p.split.AddWidget(p.buildList())
	p.split.AddWidget(p.detailPanel)
	p.split.SetStretchFactor(0, 100-findingListShare)
	p.split.SetStretchFactor(1, findingListShare)
	p.detailPanel.Hide() // closed until a finding is clicked

	p.anim = qt.NewQVariantAnimation2(p.root.QObject)
	p.anim.SetDuration(200)
	p.anim.SetEasingCurve(qt.NewQEasingCurve3(qt.QEasingCurve__OutCubic))
	p.anim.OnValueChanged(func(v *qt.QVariant) { p.sizeDetail(v.ToInt()) })
	p.anim.OnFinished(p.detailSettled)

	layout.AddWidget(p.split.QWidget)

	// Findings can be recorded by an agent, on its own goroutine.
	p.unsubscribe = store.Subscribe(func(findings.Event) {
		mainthread.Start(p.reload)
	})
	p.reload()
	return p
}

func (p *findingsPane) close() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func (p *findingsPane) buildList() *qt.QWidget {
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "FindingList")
	p.list.SetFrameShape(qt.QFrame__NoFrame)
	p.list.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	p.list.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	p.list.OnCurrentRowChanged(func(row int) {
		if !p.rebuilding {
			p.selectRow(row)
		}
	})
	p.list.OnClicked(func(*qt.QModelIndex) { p.openCurrent() })
	p.list.OnActivated(func(*qt.QModelIndex) { p.openCurrent() })
	p.list.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if e.Key() == int(qt.Key_Escape) && p.detailOpen {
			p.setDetailOpen(false)
			return
		}
		super(e)
	})

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)
	v.AddWidget(p.buildHeader())
	v.AddWidget(p.buildFilters())

	// An empty list explains what findings are rather than showing nothing.
	p.pages = qt.NewQStackedWidget2()
	p.pages.AddWidget(newEmptyState("No findings yet",
		"A finding is anything worth keeping: a credential, a leaked field, a response that "+
			"shouldn't have come back. Highlight it in a request or response and choose "+
			"Create Finding, or let an agent record one."))
	p.pages.AddWidget(p.list.QWidget)
	v.AddWidget2(p.pages.QWidget, 1)
	return panel.QWidget
}

func (p *findingsPane) buildHeader() *qt.QWidget {
	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)

	title := qt.NewQLabel3("Findings")
	setName(title.QObject, "PanelTitle")
	p.count = qt.NewQLabel3("0")
	setName(p.count.QObject, "Badge")
	p.unseen = qt.NewQLabel2()
	setName(p.unseen.QObject, "HeldBadge")
	p.unseen.SetToolTip("Findings you haven't opened yet")
	p.unseen.Hide()

	p.search = qt.NewQLineEdit2()
	p.search.SetPlaceholderText("Filter findings…")
	p.search.SetClearButtonEnabled(true)
	p.search.SetFixedWidth(260)
	p.search.OnTextChanged(func(string) { p.reload() })

	p.markRead = qt.NewQPushButton3("Mark all read")
	p.markRead.SetAutoDefault(false)
	p.markRead.SetToolTip("Mark every finding as read")
	p.markRead.OnClicked(p.markAllRead)

	// Hide the likely false positives and the findings of little concern by
	// default (below each floor), so the list is the findings worth looking
	// at; each drop-down changes its cutoff and clicking a button turns that
	// filter off.
	p.confFilter = newScoreFilter(scoreFilterSpec{
		label:   "Hide False Positives",
		what:    "confidence",
		heading: "Hide findings with a confidence below",
		help: "50 is unknown. Below leans false positive; above leans true positive. " +
			"Agents score new findings as they review them.",
		min: findings.ConfidenceMin, max: findings.ConfidenceMax,
	}, findings.ConfidenceFloor, p.reload)
	p.concernFilter = newScoreFilter(scoreFilterSpec{
		label:   "Hide Low Concern",
		what:    "concern",
		heading: "Hide findings with a concern below",
		help: "0 is no concern; 100 is a critical issue likely to lead to exploitation. " +
			"Agents score new findings as they review them.",
		min: findings.ConcernMin, max: findings.ConcernMax,
	}, findings.ConcernFloor, p.reload)

	h.AddWidget(title.QWidget)
	h.AddWidget(p.count.QWidget)
	h.AddWidget(p.unseen.QWidget)
	h.AddStretch()
	h.AddWidget(p.confFilter.widget())
	h.AddWidget(p.concernFilter.widget())
	h.AddWidget(p.markRead.QWidget)
	h.AddWidget(p.search.QWidget)
	h.AddWidget(newIconButton(iconPlus, "Record a finding by hand", p.addFinding).w)
	return header
}

// buildFilters holds the row of type pills. It is rebuilt whenever the
// catalog changes, which arrives as a store event.
func (p *findingsPane) buildFilters() *qt.QWidget {
	p.filters = qt.NewQWidget2()
	p.filtersBox = qt.NewQHBoxLayout(p.filters)
	p.filtersBox.SetContentsMargins(rowPadX, 6, rowPadX, 6)
	p.filtersBox.SetSpacing(pillSpacing)
	return p.filters
}

// rebuildFilters lays out one clickable pill per configured type. A type the
// filter is limited to but that no longer exists is dropped from the filter,
// so a removed type can't leave the list showing nothing.
func (p *findingsPane) rebuildFilters(types []findings.Type) {
	clearLayout(p.filtersBox.QLayout)
	p.pills = nil

	known := make(map[string]bool, len(types))
	for _, t := range types {
		known[t.ID] = true
	}
	for id := range p.chosen {
		if !known[id] {
			delete(p.chosen, id)
		}
	}

	for _, t := range types {
		id := t.ID
		pl := newFilterPill(t.Name, t.Color, func() { p.toggleType(id) })
		pl.setOn(p.chosen[id])
		p.pills = append(p.pills, pl)
		p.filtersBox.AddWidget(pl.w)
	}
	p.filtersBox.AddStretch()
	p.filters.SetVisible(len(types) > 0)
}

// markAllRead marks every finding read.
func (p *findingsPane) markAllRead() {
	if _, err := p.store.MarkAllViewed(); err != nil {
		p.report(err)
	}
}

// maxTopTypes is how many type pills the Findings top bar shows.
const maxTopTypes = 6

// topTypes keeps the finding types worth filtering by in the top bar: the
// ones classifying the most findings (plus any the filter is currently set
// to, so an active pill never disappears), up to maxTopTypes.
func topTypes(types []findings.Type, all []findings.Finding, chosen map[string]bool) []findings.Type {
	count := map[string]int{}
	for _, f := range all {
		for _, id := range f.Types {
			count[id]++
		}
	}
	keep := make([]findings.Type, 0, len(types))
	for _, t := range types {
		if count[t.ID] > 0 || chosen[t.ID] {
			keep = append(keep, t)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool {
		ci, cj := count[keep[i].ID], count[keep[j].ID]
		if chosen[keep[i].ID] != chosen[keep[j].ID] {
			return chosen[keep[i].ID] // keep chosen pills first
		}
		return ci > cj
	})
	if len(keep) > maxTopTypes {
		keep = keep[:maxTopTypes]
	}
	return keep
}

func (p *findingsPane) toggleType(id string) {
	if p.chosen[id] {
		delete(p.chosen, id)
	} else {
		p.chosen[id] = true
	}
	p.reload()
}

// reload rebuilds the rows from the store, keeping the selection, and
// refreshes the detail pane in case the shown finding changed.
func (p *findingsPane) reload() {
	types := p.store.Types()
	all := p.store.List()
	p.rebuildFilters(topTypes(types, all, p.chosen))
	catalog := func(id string) (string, string) { return typeLabel(types, id) }

	q := findings.Query{}
	if p.confFilter != nil {
		q.MinConfidence = p.confFilter.min()
	}
	if p.concernFilter != nil {
		q.MinConcern = p.concernFilter.min()
	}
	for id := range p.chosen {
		q.Types = append(q.Types, id)
	}
	// A malformed query is impossible here: the box is a plain substring
	// search, like the request list's with regex off.
	if m, err := filter.Compile(p.search.Text(), false); err == nil {
		q.Match = m.Match
	}
	list := p.store.Select(q)

	p.rebuilding = true
	p.list.Clear()
	p.shown = p.shown[:0]
	for _, f := range list {
		item := qt.NewQListWidgetItem()
		item.SetSizeHint(qt.NewQSize2(0, findingRowHeight))
		p.list.AddItemWithItem(item)
		p.list.SetItemWidget(item, p.buildRow(f, catalog))
		p.shown = append(p.shown, f.ID)
	}
	p.rebuilding = false

	total, unviewed := p.store.Len(), p.store.Unviewed()
	p.count.SetText(countLabel(len(list), total))
	p.unseen.SetText(fmt.Sprintf("%d new", unviewed))
	p.unseen.SetVisible(unviewed > 0)
	if p.markRead != nil {
		p.markRead.SetVisible(unviewed > 0)
	}

	page := 1
	if len(list) == 0 {
		page = 0
	}
	p.pages.SetCurrentIndex(page)

	// Keep the selection on the same finding where it still shows.
	row := -1
	for i, id := range p.shown {
		if id == p.current {
			row = i
		}
	}
	if row >= 0 {
		p.list.SetCurrentRow(row)
	}
	if f, ok := p.store.Get(p.current); ok {
		p.detail.show(f, types)
	} else if p.detailOpen {
		p.setDetailOpen(false) // the shown finding is gone
	}
	if p.onChanged != nil {
		p.onChanged()
	}
}

// countLabel is the header's count: the number shown, and the total when a
// filter is hiding some of them.
func countLabel(shown, total int) string {
	if shown == total {
		return fmt.Sprint(total)
	}
	return fmt.Sprintf("%d of %d", shown, total)
}

// buildRow is one finding's line: its value, its pills, and what it was seen
// in. An unviewed finding carries a property the stylesheet tints, which is
// how "lightly highlighted until viewed" is drawn.
func (p *findingsPane) buildRow(f findings.Finding, catalog func(string) (string, string)) *qt.QWidget {
	row := qt.NewQWidget2()
	setName(row.QObject, "FindingRow")
	row.SetAttribute(qt.WA_StyledBackground)
	// A string, because that is what a stylesheet selector matches on
	// (see #FindingRow[unviewed="true"] in theme.qss).
	row.SetProperty("unviewed", qt.NewQVariant11(fmt.Sprint(f.Unread())))

	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(10)

	text := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(text)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(3)
	// A hidden finding's row says what it is rather than what it holds; the
	// value itself is revealed in the detail pane, with the eye.
	value := qt.NewQLabel3(f.SummaryMasked())
	// A finding reviewed and scored below the floor reads muted: likely noise,
	// kept on record but out of the way. Unreviewed findings read normally.
	if f.Confidence != nil && *f.Confidence < findings.ConfidenceFloor {
		setName(value.QObject, "Muted")
	} else {
		setName(value.QObject, "PanelTitle")
	}
	value.SetToolTip(f.SummaryMasked())
	sub := findingSubtitle(f)
	// Lead the subtitle with the scores that have been assessed, confidence
	// then concern, so a reviewed finding's judgement shows at a glance.
	for _, note := range []string{concernNote(f.Concern), confidenceNote(f.Confidence)} {
		if note != "" {
			sub = note + " · " + sub
		}
	}
	if f.AwaitingReview {
		// Not announced yet: the reviewer agent takes a first look, and
		// only what it finds notable arrives unread.
		sub = "Awaiting agent review · " + sub
	}
	detail := qt.NewQLabel3(sub)
	setName(detail.QObject, "Muted")
	for _, l := range []*qt.QLabel{value, detail} {
		// A long value mustn't push the pills off the row.
		l.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	}
	v.AddWidget(value.QWidget)
	v.AddWidget(detail.QWidget)
	h.AddWidget2(text, 1)
	h.AddWidget(pillRow(f.Types, catalog))
	return row
}

// findingSubtitle is a finding's second line: where it came from, how many
// requests it was seen in, and when it was recorded.
func findingSubtitle(f findings.Finding) string {
	parts := []string{findingSource(f)}
	switch n := len(f.Links); {
	case n == 1:
		parts = append(parts, "1 request")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d requests", n))
	}
	parts = append(parts, f.Created.Format("15:04:05"))
	return strings.Join(parts, " · ")
}

// findingSource says who raised a finding, naming the agent session or
// plugin when there is one: "an agent said this" is the first thing to know
// about a finding you didn't write yourself.
func findingSource(f findings.Finding) string {
	switch f.Source {
	case findings.SourceAgent:
		if f.Author != "" {
			return "Agent: " + f.Author
		}
		return "Agent"
	case findings.SourcePlugin:
		if f.Author != "" {
			return "Plugin: " + f.Author
		}
		return "Plugin"
	}
	return "You"
}

func (p *findingsPane) selectRow(row int) {
	if row < 0 || row >= len(p.shown) {
		return
	}
	p.current = p.shown[row]
}

// openCurrent opens the detail pane on the selected finding, which is also
// what marks it viewed.
func (p *findingsPane) openCurrent() {
	f, ok := p.store.Get(p.current)
	if !ok {
		return
	}
	p.detail.show(f, p.store.Types())
	p.setDetailOpen(true)
	if !f.Viewed {
		// Opening a finding is reading it: the badge and the row's tint go
		// together, and reload() redraws both.
		p.report(p.store.MarkViewed(f.ID))
	}
}

// detailActions wires the detail pane's buttons back to the store.
func (p *findingsPane) detailActions() findingDetailActions {
	return findingDetailActions{
		onClose: func() { p.setDetailOpen(false) },
		onEdit: func(id uint64) {
			f, ok := p.store.Get(id)
			if !ok {
				return
			}
			p.runDialog(f)
		},
		onRemove: func(id uint64) {
			f, ok := p.store.Get(id)
			if !ok {
				return
			}
			answer := qt.QMessageBox_Question6(p.root, "Delete finding",
				"Delete \""+f.SummaryMasked()+"\"?\n\nIts notes, details and sightings are removed from "+
					"the engagement. This can't be undone.",
				qt.QMessageBox__Yes|qt.QMessageBox__Cancel, qt.QMessageBox__Cancel)
			if answer != qt.QMessageBox__Yes {
				return
			}
			p.current = 0
			p.setDetailOpen(false)
			p.report(p.store.Remove(id))
		},
		onShowFlow: func(flowID uint64) {
			if p.onShowFlow != nil {
				p.onShowFlow(flowID)
			}
		},
		onShowFile: func(path string) {
			if p.onShowFile != nil {
				p.onShowFile(path)
			}
		},
	}
}

// addFinding records one by hand, with nothing filled in: the Findings
// section's own + button, for something noticed away from a message.
func (p *findingsPane) addFinding() {
	draft := findings.Draft{Source: findings.SourceUser}
	if f, ok := runFindingDialog(p.root, p.store, draft, nil); ok {
		p.current = f.ID
		p.reload()
	}
}

// runDialog edits an existing finding.
func (p *findingsPane) runDialog(f findings.Finding) {
	if _, ok := runFindingDialog(p.root, p.store, findings.Draft{}, &f); ok {
		p.reload()
	}
}

func (p *findingsPane) report(err error) {
	if err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't save the finding", sentence(err.Error()))
		return
	}
}

// ---- The sliding detail pane --------------------------------------------

// setDetailOpen slides the detail pane out or away. It mirrors the Traffic
// viewer (mainWindow.setDetailOpen): the list keeps the full width until a
// finding is clicked, and the pane's last width is remembered so reopening
// it brings it back where it was.
func (p *findingsPane) setDetailOpen(open bool) {
	if open == p.detailOpen {
		return
	}
	p.detailOpen = open

	avail := p.split.Width() - p.split.HandleWidth()
	from, to := 0, 0
	if open {
		to = p.detailWidth
		if to <= 0 {
			to = avail * findingListShare / 100
		}
		to = max(min(to, avail-240), min(420, avail/2))
		p.detailPanel.SetMinimumWidth(0) // so it can start from nothing
		p.sizeDetail(0)
		p.detailPanel.Show()
	} else {
		from = p.currentDetailWidth()
		p.detailWidth = from
		p.detailPanel.SetMinimumWidth(0)
	}
	p.anim.Stop()
	p.anim.SetStartValue(qt.NewQVariant4(from))
	p.anim.SetEndValue(qt.NewQVariant4(to))
	p.anim.Start()
}

// sizeDetail gives the detail pane width pixels and the list the rest.
func (p *findingsPane) sizeDetail(width int) {
	avail := p.split.Width() - p.split.HandleWidth()
	p.split.SetSizes([]int{max(avail-width, 0), width})
}

// currentDetailWidth is the pane's width right now, or 0 when it is closed.
func (p *findingsPane) currentDetailWidth() int {
	sizes := p.split.Sizes()
	if len(sizes) < 2 {
		return 0
	}
	return sizes[1]
}

// detailSettled finishes a slide: an open pane gets a minimum width back so
// the divider can't squeeze it away, and a closed one is hidden.
func (p *findingsPane) detailSettled() {
	if p.detailOpen {
		p.detailPanel.SetMinimumWidth(320)
		return
	}
	p.detailPanel.Hide()
	p.sizeDetail(0)
}

// refreshTheme repaints the parts whose colours are painted rather than
// styled: the pills, and the detail pane's editors.
func (p *findingsPane) refreshTheme() {
	p.reload()
	p.detail.refreshTheme()
}
