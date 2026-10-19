package ui

import (
	"fmt"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/entities"
	"reproxy/internal/target"
)

// relatedPane is the lower half of the Targets section: the hosts no
// target covers that the targets' traffic refers to (see
// entities.Related) — a CDN a target's pages load scripts from, a sign-on
// service it redirects to, a host its Content-Security-Policy names.
//
//	┌ Related  3 ──────────────────────────────────┐
//	│ cdn.example.net        [Suppress] [Promote]  │
//	│ Domain · 12 URLs · found on app.test         │
//	└──────────────────────────────────────────────┘
//
// Ticking hosts' boxes (or the header's, for all of them) offers Suppress
// and Promote for the lot, each confirmed first.
//
// Clicking a host lists its URLs and where each was found; Promote
// makes it a target, which takes it out of this list and puts its
// URLs in the Explorer; Suppress adds it to a suppression list — the
// user's, which every engagement shares, or the open engagement's own — so
// it isn't suggested again while it is there. The list is worked out from the entities store
// only while the section is on screen.
type relatedPane struct {
	root     *qt.QWidget
	entities *entities.Store
	targets  *target.Store
	// onPromoted is called with the target a host was promoted to.
	onPromoted func(target.Target, error)
	// onShowFlow shows a flow in Traffic.
	onShowFlow func(uint64)
	// suppress adds hosts to the user's suppression list, or with
	// engagementOnly to the open engagement's, in one save; engagement
	// names the open engagement (ok false with none open).
	suppress   func(hosts []string, engagementOnly bool) error
	engagement func() (name string, ok bool)

	list  *qt.QListWidget
	count *qt.QLabel
	empty *qt.QStackedWidget
	hosts []entities.Related

	// selected is the hosts ticked, kept across reloads while they are
	// still related; boxes are the rows' checkboxes.
	selected  map[string]bool
	boxes     map[string]*qt.QCheckBox
	selectAll *qt.QCheckBox
	// bulk holds what the header offers while hosts are ticked.
	bulk         *qt.QWidget
	selCount     *qt.QLabel
	bulkSuppress *qt.QPushButton

	active   bool
	stale    bool
	debounce *qt.QTimer
}

const relatedRefreshDelay = 400 * time.Millisecond

func newRelatedPane(eps *entities.Store, targets *target.Store, onPromoted func(target.Target, error),
	onShowFlow func(uint64), suppress func([]string, bool) error, engagement func() (string, bool)) *relatedPane {
	p := &relatedPane{entities: eps, targets: targets, onPromoted: onPromoted, onShowFlow: onShowFlow,
		suppress: suppress, engagement: engagement, stale: true,
		selected: map[string]bool{}, boxes: map[string]*qt.QCheckBox{}}

	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "TargetList")
	p.list.SetFrameShape(qt.QFrame__NoFrame)
	p.list.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	p.list.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	p.list.OnItemClicked(func(item *qt.QListWidgetItem) {
		if row := p.list.Row(item); row >= 0 && row < len(p.hosts) {
			p.openDetails(p.hosts[row])
		}
	})

	p.count = qt.NewQLabel3("0")
	setName(p.count.QObject, "Badge")

	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)
	p.selectAll = qt.NewQCheckBox2()
	p.selectAll.SetToolTip("Select every related host, or none")
	p.selectAll.OnClicked(func() { p.setAllSelected(len(p.selectedHosts()) < len(p.hosts)) })
	h.AddWidget(p.selectAll.QWidget)
	title := qt.NewQLabel3("Related")
	setName(title.QObject, "PanelTitle")
	title.SetToolTip("Hosts no target covers that your targets' traffic refers to")
	h.AddWidget(title.QWidget)
	h.AddWidget(p.count.QWidget)
	h.AddStretch()
	p.bulk = qt.NewQWidget2()
	b := qt.NewQHBoxLayout(p.bulk)
	b.SetContentsMargins(0, 0, 0, 0)
	b.SetSpacing(8)
	p.selCount = qt.NewQLabel2()
	setName(p.selCount.QObject, "Muted")
	b.AddWidget(p.selCount.QWidget)
	p.bulkSuppress = qt.NewQPushButton3("Suppress Selected")
	p.bulkSuppress.SetToolTip("Stop suggesting the selected hosts, everywhere or in this engagement")
	p.bulkSuppress.OnClicked(func() { p.suppressSelectedMenu(p.bulkSuppress) })
	b.AddWidget(p.bulkSuppress.QWidget)
	promote := qt.NewQPushButton3("Promote Selected")
	promote.SetToolTip("Make each selected host a target")
	promote.OnClicked(p.promoteSelected)
	b.AddWidget(promote.QWidget)
	p.bulk.SetVisible(false)
	h.AddWidget(p.bulk)
	// The header keeps the height of the Targets header, which has a button,
	// or is as tall as the bulk actions need, so it doesn't jump when they
	// appear.
	header.SetMinimumHeight(max(36, promote.SizeHint().Height()+12))

	p.empty = qt.NewQStackedWidget2()
	p.empty.AddWidget(newEmptyState("Nothing related yet",
		"Domains and IP addresses your targets' traffic refers to — links, scripts and fonts "+
			"from CDNs, redirects, Content-Security-Policy sources — appear here when no target covers them."))
	p.empty.AddWidget(p.list.QWidget)

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)
	v.AddWidget(header)
	v.AddWidget(p.empty.QWidget)
	p.root = panel.QWidget

	p.debounce = qt.NewQTimer()
	p.debounce.SetSingleShot(true)
	p.debounce.SetInterval(int(relatedRefreshDelay / time.Millisecond))
	p.debounce.OnTimeout(p.reload)
	return p
}

// setActive tells the pane whether the Targets section is on screen.
func (p *relatedPane) setActive(on bool) {
	p.active = on
	if on && p.stale {
		p.reload()
	}
}

// changed is called for every entities store event, and when the targets
// change. It only schedules work.
func (p *relatedPane) changed() {
	p.stale = true
	if p.active && !p.debounce.IsActive() {
		p.debounce.Start2()
	}
}

// reload lists the related hosts again.
func (p *relatedPane) reload() {
	p.stale = false
	p.hosts = p.entities.Related()
	p.list.Clear()
	p.boxes = map[string]*qt.QCheckBox{}
	// A ticked host that has gone (promoted, suppressed, or no longer
	// referred to) is no longer selected.
	listed := make(map[string]bool, len(p.hosts))
	for _, r := range p.hosts {
		listed[r.Host] = true
	}
	for h := range p.selected {
		if !listed[h] {
			delete(p.selected, h)
		}
	}
	for _, r := range p.hosts {
		item := qt.NewQListWidgetItem()
		item.SetSizeHint(qt.NewQSize2(0, targetRowHeight))
		p.list.AddItemWithItem(item)
		p.list.SetItemWidget(item, p.buildRow(r))
	}
	p.count.SetText(fmt.Sprint(len(p.hosts)))
	if len(p.hosts) == 0 {
		p.empty.SetCurrentIndex(0)
	} else {
		p.empty.SetCurrentIndex(1)
	}
	p.updateSelection()
}

// selectedHosts lists the ticked hosts, in the list's order.
func (p *relatedPane) selectedHosts() []string {
	var out []string
	for _, r := range p.hosts {
		if p.selected[r.Host] {
			out = append(out, r.Host)
		}
	}
	return out
}

// setAllSelected ticks every host, or none.
func (p *relatedPane) setAllSelected(on bool) {
	for _, r := range p.hosts {
		p.selected[r.Host] = on
		if box := p.boxes[r.Host]; box != nil {
			box.BlockSignals(true)
			box.SetChecked(on)
			box.BlockSignals(false)
		}
	}
	p.updateSelection()
}

// updateSelection shows what is ticked: the header's box (all, some or
// none), and the bulk actions while anything is.
func (p *relatedPane) updateSelection() {
	n := len(p.selectedHosts())
	state := qt.Unchecked
	switch {
	case n > 0 && n == len(p.hosts):
		state = qt.Checked
	case n > 0:
		state = qt.PartiallyChecked
	}
	p.selectAll.BlockSignals(true)
	p.selectAll.SetTristateWithBool(state == qt.PartiallyChecked)
	p.selectAll.SetCheckState(state)
	p.selectAll.BlockSignals(false)
	p.selectAll.SetVisible(len(p.hosts) > 0)
	p.selCount.SetText(countOf(n, "host", "hosts") + " selected")
	p.bulk.SetVisible(n > 0)
}

func (p *relatedPane) buildRow(r entities.Related) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(10)

	box := qt.NewQCheckBox2()
	box.SetToolTip("Select " + r.Host + " to suppress or promote it with others")
	box.SetChecked(p.selected[r.Host])
	box.OnToggled(func(on bool) {
		p.selected[r.Host] = on
		p.updateSelection()
	})
	p.boxes[r.Host] = box
	h.AddWidget(box.QWidget)

	text := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(text)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(2)
	label := qt.NewQLabel3(r.Host)
	setName(label.QObject, "PanelTitle")
	detail := qt.NewQLabel3(relatedSubtitle(r))
	setName(detail.QObject, "Muted")
	detail.SetToolTip(relatedSubtitle(r))
	for _, l := range []*qt.QLabel{label, detail} {
		l.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	}
	v.AddWidget(label.QWidget)
	v.AddWidget(detail.QWidget)
	h.AddWidget2(text, 1)

	host := r.Host
	suppress := qt.NewQPushButton3("Suppress")
	suppress.SetToolTip("Stop suggesting " + host + ", everywhere or in this engagement")
	suppress.OnClicked(func() { p.suppressMenu(suppress, host, nil) })
	h.AddWidget(suppress.QWidget)
	promote := qt.NewQPushButton3("Promote")
	promote.SetToolTip("Make " + host + " a target, putting its URLs in the Explorer")
	promote.OnClicked(func() { p.promote(host) })
	h.AddWidget(promote.QWidget)
	return row
}

// relatedSubtitle is a related host's second line: what it is, how much
// is known on it, and where it was found.
func relatedSubtitle(r entities.Related) string {
	kind := "Domain"
	if r.IP {
		kind = "IP address"
	}
	s := kind + " · " + countOf(r.URLs, "URL", "URLs")
	if r.URLs == 0 {
		s = kind + " · no URLs yet" // found as a host (DNS, say), not a link
	}
	if len(r.FoundOn) > 0 {
		s += " · found on " + strings.Join(r.FoundOn, ", ")
	}
	return s
}

// promote makes host a target, unless one covers it already (the list
// can be a moment behind).
func (p *relatedPane) promote(host string) {
	if m, ok := p.targets.Set().MatchHost(host); ok {
		qt.QMessageBox_Information(p.root, "Already a target",
			fmt.Sprintf("%s is already covered by the target %s.", host, m.Name))
		return
	}
	t, err := p.targets.Create("", host)
	p.onPromoted(t, err)
}

// suppressMenu offers the two lists a host can be suppressed in, under
// the button, and suppresses it in the one picked. done runs first when it
// is (the details window closes itself with it).
func (p *relatedPane) suppressMenu(button *qt.QPushButton, host string, done func()) {
	menu := qt.NewQMenu2()
	pick := func(engagementOnly bool) func() {
		return func() {
			if done != nil {
				done()
			}
			p.suppressHost(host, engagementOnly)
		}
	}
	menu.AddActionWithText("Suppress Everywhere").OnTriggered(pick(false))
	name, open := p.engagement()
	label := "Suppress in This Engagement"
	if open {
		label = "Suppress in " + name + " Only"
	}
	only := menu.AddActionWithText(label)
	only.SetEnabled(open)
	if !open {
		only.SetToolTip("Open an engagement to keep a suppression to it")
	}
	only.OnTriggered(pick(true))
	menu.Exec3(button.MapToGlobalWithQPoint(qt.NewQPoint2(0, button.Height())), nil)
}

// suppressHost adds host to a suppression list. The store works the
// related hosts out again, which takes it off this list.
func (p *relatedPane) suppressHost(host string, engagementOnly bool) {
	if err := p.suppress([]string{host}, engagementOnly); err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't suppress "+host, sentence(err.Error()))
	}
}

// suppressSelectedMenu offers the two lists the ticked hosts can be
// suppressed in, under the button, and confirms before suppressing them.
func (p *relatedPane) suppressSelectedMenu(button *qt.QPushButton) {
	menu := qt.NewQMenu2()
	menu.AddActionWithText("Suppress Everywhere…").OnTriggered(func() { p.suppressSelected(false) })
	name, open := p.engagement()
	label := "Suppress in This Engagement…"
	if open {
		label = "Suppress in " + name + " Only…"
	}
	only := menu.AddActionWithText(label)
	only.SetEnabled(open)
	if !open {
		only.SetToolTip("Open an engagement to keep a suppression to it")
	}
	only.OnTriggered(func() { p.suppressSelected(true) })
	menu.Exec3(button.MapToGlobalWithQPoint(qt.NewQPoint2(0, button.Height())), nil)
}

// suppressSelected confirms, then suppresses every ticked host in one
// save.
func (p *relatedPane) suppressSelected(engagementOnly bool) {
	hosts := p.selectedHosts()
	if len(hosts) == 0 {
		return
	}
	where, undo := "in every engagement", "Settings → Global → Targets"
	if engagementOnly {
		name, _ := p.engagement()
		where, undo = "in "+name+" only", "Settings → Engagement"
	}
	n := countOf(len(hosts), "host", "hosts")
	if ok, _ := confirm(p.root, confirmOptions{
		title:   "Suppress " + n + "?",
		heading: "Suppress " + n + " " + where + "?",
		body: []string{"They won't be suggested as related again while they are suppressed. What is already " +
			"known about them is kept, and you can lift a suppression in " + undo + "."},
		items:  hosts,
		cancel: "Cancel",
		ok:     "Suppress " + n,
	}); !ok {
		return
	}
	if err := p.suppress(hosts, engagementOnly); err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't suppress "+n, sentence(err.Error()))
		return
	}
	p.setAllSelected(false)
}

// promoteSelected confirms, then makes every ticked host a target in one
// save. Hosts a target already covers (the list can be a moment behind)
// are left out.
func (p *relatedPane) promoteSelected() {
	var hosts []string
	for _, h := range p.selectedHosts() {
		if _, covered := p.targets.Set().MatchHost(h); !covered {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		p.setAllSelected(false)
		return
	}
	n := countOf(len(hosts), "host", "hosts")
	if ok, _ := confirm(p.root, confirmOptions{
		title:   "Promote " + n + "?",
		heading: "Promote " + n + " to targets?",
		body: []string{"Each becomes a target of its own: its traffic is collected and can be intercepted, its " +
			"URLs join the Explorer, the hosts it refers to are suggested here in turn, and Autocrawl crawls it " +
			"unless you leave it out. Make sure each is in scope for your engagement."},
		items:  hosts,
		cancel: "Cancel",
		ok:     "Promote " + n,
	}); !ok {
		return
	}
	added, err := p.targets.CreateAll("", hosts)
	var first target.Target
	if len(added) > 0 {
		first = added[0]
	}
	p.onPromoted(first, err)
	if err == nil {
		p.setAllSelected(false)
	}
}

// openDetails opens a window listing a related host's URLs and, under
// each, where it was found. Double-clicking where opens that request in
// Traffic.
func (p *relatedPane) openDetails(r entities.Related) {
	dlg := qt.NewQDialog(p.root)
	dlg.SetAttribute(qt.WA_DeleteOnClose)
	dlg.SetWindowTitle("Related · " + r.Host)
	dlg.Resize(900, 480)

	v := qt.NewQVBoxLayout(dlg.QWidget)
	v.SetContentsMargins(16, 14, 16, 14)
	v.SetSpacing(10)

	eps := p.entities.OnHost(r.Host)
	title := qt.NewQLabel3(fmt.Sprintf("%s on %s", countOf(len(eps), "URL", "URLs"), r.Host))
	// A host found without a URL (a plugin's DNS lookup, say) carries its
	// own links: list it first, so where it came from shows.
	if h, ok := p.entities.Get(entities.HostKey("", r.Host)); ok && len(h.Links) > 0 {
		eps = append([]entities.Entity{h}, eps...)
	}
	setName(title.QObject, "PanelTitle")
	title.SetTextInteractionFlags(qt.TextSelectableByMouse)
	v.AddWidget(title.QWidget)
	hint := qt.NewQLabel3("Each URL lists where it was found. Double-click one of those to see that request in Traffic.")
	setName(hint.QObject, "Muted")
	hint.SetWordWrap(true)
	v.AddWidget(hint.QWidget)

	tree := qt.NewQTreeWidget2()
	setName(tree.QObject, "RequestsTable")
	tree.SetUniformRowHeights(true)
	tree.SetAllColumnsShowFocus(true)
	tree.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	cols := []string{"URL", "How", "Seen"}
	tree.SetColumnCount(len(cols))
	tree.SetHeaderLabels(cols)
	header := tree.Header()
	header.SetStretchLastSection(false)
	header.SetSectionResizeMode2(0, qt.QHeaderView__Stretch)
	header.SetSectionResizeMode2(1, qt.QHeaderView__ResizeToContents)
	header.SetSectionResizeMode2(2, qt.QHeaderView__ResizeToContents)

	flows := map[uintptr]uint64{}
	for _, e := range eps {
		item := qt.NewQTreeWidgetItem3(tree)
		state := "discovered"
		if e.Visited() {
			state = "requested " + countOf(e.Hits, "time", "times")
		}
		item.SetText(0, e.Label())
		item.SetToolTip(0, e.Label())
		item.SetText(1, state)
		item.SetText(2, e.FirstSeen.Format("2006-01-02 15:04"))
		for _, l := range e.Links {
			child := qt.NewQTreeWidgetItem6(item)
			from := l.FromURL
			if from == "" {
				from = "(no page)"
			}
			child.SetText(0, "found on "+from)
			child.SetToolTip(0, from)
			child.SetText(1, linkHow(l))
			child.SetText(2, l.Added.Format("2006-01-02 15:04"))
			if l.FromFlowID != 0 {
				flows[uintptr(child.UnsafePointer())] = l.FromFlowID
			}
		}
	}
	tree.ExpandAll()
	tree.OnItemDoubleClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if id, ok := flows[uintptr(item.UnsafePointer())]; ok {
			p.onShowFlow(id)
		}
	})
	v.AddWidget2(tree.QWidget, 1)

	promote := qt.NewQPushButton3("Promote to Target")
	promote.SetDefault(true)
	promote.SetToolTip("Make " + r.Host + " a target, putting these URLs in the Explorer")
	promote.OnClicked(func() {
		dlg.Close()
		p.promote(r.Host)
	})
	suppress := qt.NewQPushButton3("Suppress")
	suppress.SetAutoDefault(false)
	suppress.SetToolTip("Stop suggesting " + r.Host + ", everywhere or in this engagement")
	suppress.OnClicked(func() { p.suppressMenu(suppress, r.Host, func() { dlg.Close() }) })
	closeBtn := qt.NewQPushButton3("Close")
	closeBtn.OnClicked(func() { dlg.Close() })
	buttons := qt.NewQHBoxLayout2()
	buttons.AddWidget(suppress.QWidget)
	buttons.AddStretch()
	buttons.AddWidget(closeBtn.QWidget)
	buttons.AddWidget(promote.QWidget)
	v.AddLayout(buttons.QLayout)
	dlg.Show()
}

// linkHow says how a link was found, for people: "html:src" is "<src>
// attribute", a referral "Referer header".
func linkHow(l entities.Link) string {
	if l.Source == entities.OriginReferer {
		if l.Detail == "origin" {
			return "Origin header"
		}
		return "Referer header"
	}
	if name, ok := strings.CutPrefix(l.Source, "plugin:"); ok {
		if l.Detail != "" {
			return "plugin " + name + ": " + l.Detail
		}
		return "plugin " + name
	}
	d := l.Detail
	switch {
	case d == "":
		return "link"
	case strings.HasPrefix(d, "html:"):
		return d[len("html:"):] + " attribute"
	case strings.HasPrefix(d, "header:"):
		switch h := d[len("header:"):]; h {
		case "csp":
			return "Content-Security-Policy header"
		case "cors":
			return "Access-Control-Allow-Origin header"
		default:
			return strings.ToUpper(h[:1]) + h[1:] + " header"
		}
	case strings.HasPrefix(d, "js:"):
		return "script (" + d[len("js:"):] + ")"
	case strings.HasPrefix(d, "json:"):
		return "JSON " + d[len("json:"):]
	case strings.HasPrefix(d, "robots:"):
		return "robots.txt " + d[len("robots:"):]
	case strings.HasPrefix(d, "openapi:"):
		return "OpenAPI " + d[len("openapi:"):]
	case d == "css":
		return "stylesheet"
	case d == "js":
		return "script"
	}
	return d
}
