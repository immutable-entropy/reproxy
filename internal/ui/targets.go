package ui

import (
	"fmt"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/entities"
	"reproxy/internal/target"
)

// targetsPane is the Targets section of the main window: the list of hosts
// in scope, full width, and below it, half its height, the hosts related
// to them (relatedPane), which can be promoted to targets.
//
// Each row shows the target's colour, its label (its name, or the address
// itself when it has no name) and its address, with a pencil that opens the
// target's editor (its rules, and its own settings, live on tabs there).
// Order is priority order — the first target covering a host owns that
// host's traffic (see target.Set.Match) — so rows can be moved up and down.
//
// The targets shown belong to the open engagement; opening another one
// replaces them (see internal/engagement), which arrives here as a store
// change like any other.
type targetsPane struct {
	root  *qt.QWidget
	store *target.Store

	list  *qt.QListWidget
	count *qt.QLabel
	empty *qt.QStackedWidget

	// current is the selected target's ID, kept across rebuilds so a change
	// elsewhere doesn't move the selection.
	current string
	// rebuilding guards the selection handler while rows are replaced.
	rebuilding bool

	// related lists the hosts the targets' traffic refers to.
	related *relatedPane

	unsubscribe func()
	// onChanged is called after any edit, so the rest of the window can
	// recolour flows that belong to a target.
	onChanged func()
}

const (
	targetRowHeight = 54
	targetSwatch    = 12
)

func newTargetsPane(store *target.Store, eps *entities.Store, onChanged func(), onShowFlow func(uint64),
	suppress func([]string, bool) error, engagement func() (string, bool)) *targetsPane {
	p := &targetsPane{root: qt.NewQWidget2(), store: store, onChanged: onChanged}
	p.related = newRelatedPane(eps, store, func(t target.Target, err error) {
		if err == nil {
			p.current = t.ID
		}
		p.report(err)
	}, onShowFlow, suppress, engagement)

	layout := qt.NewQVBoxLayout(p.root)
	layout.SetContentsMargins(0, 0, 0, 0)
	layout.SetSpacing(windowMargin)
	layout.AddWidget2(p.buildList(), 2)
	layout.AddWidget2(p.related.root, 1)

	p.unsubscribe = store.Subscribe(p.reload)
	p.reload()
	return p
}

func (p *targetsPane) close() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func (p *targetsPane) buildList() *qt.QWidget {
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "TargetList")
	p.list.SetFrameShape(qt.QFrame__NoFrame)
	p.list.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	p.list.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)
	p.list.OnCurrentRowChanged(func(row int) {
		if p.rebuilding {
			return
		}
		p.selectRow(row)
	})

	p.count = qt.NewQLabel3("0")
	setName(p.count.QObject, "Badge")
	add := newIconButton(iconPlus, "Add a target", p.addTarget)
	imp := newIconButton(iconImport,
		"Import targets from a Burp Suite scope file", p.importScope)

	header := qt.NewQWidget2()
	setName(header.QObject, "PanelHeader")
	header.SetAttribute(qt.WA_StyledBackground)
	h := qt.NewQHBoxLayout(header)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(8)
	title := qt.NewQLabel3("Targets")
	setName(title.QObject, "PanelTitle")
	h.AddWidget(title.QWidget)
	h.AddWidget(p.count.QWidget)
	h.AddStretch()
	h.AddWidget(imp.w)
	h.AddWidget(add.w)

	// An empty list explains itself rather than showing a blank panel.
	p.empty = qt.NewQStackedWidget2()
	p.empty.AddWidget(newEmptyState("No targets yet",
		"A target is one host in scope: a domain, an IP address or a CIDR range, "+
			"and the paths within it worth harvesting. Use + to add one, or import "+
			"a Burp Suite scope file to bring in a whole program's scope at once."))
	p.empty.AddWidget(p.list.QWidget)

	panel := qt.NewQFrame2()
	setName(panel.QObject, "Panel")
	v := qt.NewQVBoxLayout(panel.QWidget)
	v.SetContentsMargins(1, 1, 1, 1)
	v.SetSpacing(0)
	v.AddWidget(header)
	v.AddWidget(p.empty.QWidget)
	return panel.QWidget
}

// setActive tells the pane whether its section is on screen.
func (p *targetsPane) setActive(on bool) { p.related.setActive(on) }

// reload rebuilds the rows from the store, keeping the selection.
func (p *targetsPane) reload() {
	// A target added or removed changes what is related.
	p.related.changed()
	list := p.store.List()

	p.rebuilding = true
	p.list.Clear()
	for i, t := range list {
		item := qt.NewQListWidgetItem()
		item.SetSizeHint(qt.NewQSize2(0, targetRowHeight))
		p.list.AddItemWithItem(item)
		p.list.SetItemWidget(item, p.buildRow(t, i, len(list)))
	}
	p.rebuilding = false

	p.count.SetText(fmt.Sprint(len(list)))
	page := 1
	if len(list) == 0 {
		page = 0
	}
	p.empty.SetCurrentIndex(page)

	row := 0
	for i, t := range list {
		if t.ID == p.current {
			row = i
		}
	}
	if len(list) == 0 {
		p.current = ""
		return
	}
	p.list.SetCurrentRow(row)
	p.selectRow(row)
}

// buildRow is one target's line: colour, label, what it covers, and the
// actions on the right.
func (p *targetsPane) buildRow(t target.Target, index, total int) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(rowPadX, 6, rowPadX-4, 6)
	h.SetSpacing(10)

	h.AddWidget(newColorSwatch(t.Color, targetSwatch).w)

	text := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(text)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(2)
	label := qt.NewQLabel3(t.Label())
	setName(label.QObject, "PanelTitle")
	detail := qt.NewQLabel3(targetSubtitle(t))
	setName(detail.QObject, "Muted")
	detail.SetToolTip(targetSubtitle(t))
	// A long address or a long name shouldn't push the buttons off the row.
	for _, l := range []*qt.QLabel{label, detail} {
		l.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	}
	v.AddWidget(label.QWidget)
	v.AddWidget(detail.QWidget)
	h.AddWidget2(text, 1)

	if !t.Enabled {
		off := qt.NewQLabel3("Off")
		setName(off.QObject, "Badge")
		off.SetToolTip("This target is switched off and matches no traffic")
		h.AddWidget(off.QWidget)
	}

	id := t.ID
	up := newIconButton(iconUp, "Give this target priority over the one above", func() {
		p.current = id
		p.report(p.store.Move(id, -1))
	})
	down := newIconButton(iconDown, "Move below the next target", func() {
		p.current = id
		p.report(p.store.Move(id, 1))
	})
	up.setEnabled(index > 0)
	down.setEnabled(index < total-1)
	edit := newIconButton(iconEdit, "Edit this target", func() { p.editTarget(id) })

	for _, b := range []*iconButton{up, down, edit} {
		h.AddWidget(b.w)
	}
	return row
}

// targetSubtitle is the second line of a target's row: the address when the
// target has a name of its own (the label already shows the name), otherwise
// what kind of address it is. Extra rules are noted either way, since they
// decide what is actually harvested.
func targetSubtitle(t target.Target) string {
	s := kindLabel(t.Kind)
	if t.Name != "" {
		s = t.Display()
	}
	if extra := len(t.Rules) - 1; extra == 1 {
		s += " · 1 path rule"
	} else if extra > 1 {
		s += fmt.Sprintf(" · %d path rules", extra)
	}
	return s
}

func (p *targetsPane) selectRow(row int) {
	list := p.store.List()
	if row < 0 || row >= len(list) {
		return
	}
	p.current = list[row].ID
}

// addTarget opens the editor on a fresh target; nothing is saved unless the
// user accepts it.
func (p *targetsPane) addTarget() {
	fresh := target.New(target.KindDomain, "", p.store.NextColor())
	added, _, ok := runTargetDialog(p.root, fresh, true)
	if !ok {
		return
	}
	p.current = added.ID
	p.report(p.store.Save(added))
}

// importScope adds the targets a Burp Suite scope file describes — a file
// saved from Burp, or a program's scope downloaded from HackerOne — in one
// write, after showing what is about to be added. A target decides whether
// traffic is collected at all, so hundreds of them arriving is not something
// to do without asking.
func (p *targetsPane) importScope() {
	picked, ok := pickScopeFile(p.root)
	if !ok {
		return
	}
	if !confirmScopeImport(p.root, picked) {
		return
	}
	added, duplicates, err := p.store.AddAll(picked.imp.Targets)
	if err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't import", sentence(err.Error()))
		return
	}
	if len(added) > 0 {
		p.current = added[0].ID
	}
	reportScopeImport(p.root, picked, len(added), duplicates)
	p.changed()
}

func (p *targetsPane) editTarget(id string) {
	t, ok := p.store.Get(id)
	if !ok {
		return
	}
	edited, remove, ok := runTargetDialog(p.root, t, false)
	switch {
	case !ok:
		return
	case remove:
		p.current = ""
		p.report(p.store.Remove(id))
	default:
		p.current = id
		p.report(p.store.Save(edited))
	}
}

// report shows an error from the store, if any, and tells the window that
// targets changed.
func (p *targetsPane) report(err error) {
	if err != nil {
		qt.QMessageBox_Warning(p.root, "Couldn't save the target", sentence(err.Error()))
		return
	}
	p.changed()
}

func (p *targetsPane) changed() {
	if p.onChanged != nil {
		p.onChanged()
	}
}

// kindLabel is how a target's kind is written in the UI.
func kindLabel(k target.Kind) string {
	switch k {
	case target.KindDomain:
		return "Domain"
	case target.KindWildcard:
		return "Wildcard"
	case target.KindRegex:
		return "Regex"
	case target.KindIP:
		return "IP address"
	case target.KindCIDR:
		return "CIDR range"
	}
	return string(k)
}
