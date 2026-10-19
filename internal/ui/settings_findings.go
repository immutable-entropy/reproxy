package ui

import (
	"fmt"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/config"
	"reproxy/internal/findings"
)

// settingsFindings is added to the settings dialog's page list.
const settingsFindings = "Findings"

// findingTypesPage is the Settings → Findings page: the finding types, each
// with the colour its pill is drawn in.
//
// The types are the user's own taxonomy and live in the settings file, so
// they follow them from one engagement to the next. A finding stores type
// IDs, which is why a type's ID is fixed once it exists: renaming or
// recolouring a type changes every finding's pill, while removing one leaves
// its findings showing the bare ID — said plainly in the page's help text
// rather than discovered later.
//
// Plugins will be able to declare types of their own at runtime; those are
// not edited here and are not written to the settings file.
type findingTypesPage struct {
	drafts   []typeDraft
	selected int

	list   *qt.QListWidget
	stack  *qt.QStackedWidget
	empty  *qt.QWidget
	editor *qt.QWidget

	name    *qt.QLineEdit
	swatch  *colorSwatch
	idLabel *qt.QLabel

	loading bool // suppresses field handlers while a draft is loaded
}

// typeDraft is one type being edited. A type added in this dialog has no id
// yet: it takes one from its name when the settings are applied, so the id
// reads like the name rather than like whatever the row was called when it
// was added. A type that came from the settings file keeps the id it has —
// findings refer to it by nothing else.
type typeDraft struct {
	t     findings.Type
	fresh bool
}

// id is what the Id row shows: the real id, or the one this type will take
// from its name. The settings dialog is modal, so no finding can be tagged
// with a fresh type before its id settles.
func (d typeDraft) id() string {
	if !d.fresh {
		return d.t.ID
	}
	if slug := findings.Slug(d.t.Name); slug != "" {
		return slug
	}
	return "type"
}

func newFindingsPage() *settingsPage {
	p := &findingTypesPage{selected: -1}

	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Finding types"))
	v.AddWidget(helpText("A type classifies a finding, like a tag: a finding can carry several, " +
		"and the Findings list can be filtered by them. Each type's colour is the background of " +
		"its pill in the list.").QWidget)

	row := qt.NewQHBoxLayout2()
	row.SetSpacing(12)

	left := qt.NewQVBoxLayout2()
	left.SetSpacing(6)
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "MasterList")
	p.list.SetFixedWidth(220)
	p.list.OnCurrentRowChanged(func(r int) {
		if !p.loading {
			p.selectDraft(r)
		}
	})
	left.AddWidget(p.list.QWidget)

	btns := qt.NewQHBoxLayout2()
	add := qt.NewQToolButton2()
	add.SetText("+")
	add.SetToolTip("Add a finding type")
	add.OnClicked(p.addType)
	remove := qt.NewQToolButton2()
	remove.SetText("−")
	remove.SetToolTip("Remove the selected type")
	remove.OnClicked(p.removeSelected)
	btns.AddWidget(add.QWidget)
	btns.AddWidget(remove.QWidget)
	btns.AddStretch()
	left.AddLayout(btns.QLayout)
	row.AddLayout(left.QLayout)

	p.stack = qt.NewQStackedWidget2()
	p.empty = qt.NewQWidget2()
	ev := qt.NewQVBoxLayout(p.empty)
	emptyLabel := qt.NewQLabel3("No type selected. Use + to add one.")
	setName(emptyLabel.QObject, "EmptyState")
	ev.AddStretch()
	ev.AddWidget(emptyLabel.QWidget)
	ev.AddStretch()
	p.editor = p.buildEditor()
	p.stack.AddWidget(p.empty)
	p.stack.AddWidget(p.editor)
	detailPanel := qt.NewQFrame2()
	setName(detailPanel.QObject, "Panel")
	detailPanel.SetFrameShape(qt.QFrame__StyledPanel)
	dp := qt.NewQVBoxLayout(detailPanel.QWidget)
	dp.SetContentsMargins(12, 10, 12, 10)
	dp.AddWidget(p.stack.QWidget)
	row.AddWidget2(detailPanel.QWidget, 1)

	v.AddLayout2(row.QLayout, 1)
	v.AddSpacing(6)
	v.AddWidget(helpText("Findings refer to a type by its id, which is set from the name when the " +
		"type is created and then stays put. Renaming or recolouring a type updates every finding " +
		"that carries it; removing one leaves those findings showing its id, so they are never " +
		"lost — add a type with the same id back and they read as before.").QWidget)

	return &settingsPage{
		title:       settingsFindings,
		description: "The types a finding can be classified with.",
		body:        body,
		load:        func(c config.Config) { p.load(c) },
		store:       func(c *config.Config) { p.store(c) },
	}
}

func (p *findingTypesPage) buildEditor() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(0, 0, 0, 0)

	form := newSettingsForm()
	p.name = qt.NewQLineEdit2()
	p.name.SetPlaceholderText("Credential")
	p.name.OnTextEdited(func(s string) {
		p.edit(func(d *typeDraft) { d.t.Name = s })
	})

	// Colour: the swatch shows it, the button opens the colour picker.
	p.swatch = newColorSwatch("#000000", 18)
	pick := qt.NewQPushButton3("Choose colour…")
	pick.SetAutoDefault(false)
	pick.OnClicked(p.pickColor)
	colorRow := qt.NewQWidget2()
	cr := qt.NewQHBoxLayout(colorRow)
	cr.SetContentsMargins(0, 0, 0, 0)
	cr.SetSpacing(8)
	cr.AddWidget(p.swatch.w)
	cr.AddWidget(pick.QWidget)
	cr.AddStretch()

	p.idLabel = qt.NewQLabel2()
	setName(p.idLabel.QObject, "Muted")
	p.idLabel.SetTextInteractionFlags(qt.TextSelectableByMouse)

	form.AddRow3("Name", p.name.QWidget)
	form.AddRow3("Colour", colorRow)
	form.AddRow3("Id", p.idLabel.QWidget)
	v.AddLayout(form.QLayout)
	v.AddStretch()
	return w
}

// ---- Data flow -------------------------------------------------------------

func (p *findingTypesPage) load(c config.Config) {
	p.drafts = p.drafts[:0]
	for _, t := range c.Findings.Types {
		p.drafts = append(p.drafts, typeDraft{t: t})
	}
	p.refreshList()
	if len(p.drafts) > 0 {
		p.list.SetCurrentRow(0)
		p.selectDraft(0)
	} else {
		p.list.SetCurrentRow(-1)
		p.selectDraft(-1)
	}
}

func (p *findingTypesPage) store(c *config.Config) {
	out := make([]findings.Type, 0, len(p.drafts))
	used := make(map[string]bool, len(p.drafts))
	for i := range p.drafts {
		t := p.drafts[i].t
		t.Name = strings.TrimSpace(t.Name)
		if t.Name == "" {
			continue // a nameless type would show as a blank pill
		}
		if p.drafts[i].fresh {
			t.ID = uniqueID(p.drafts[i].id(), used)
		}
		used[t.ID] = true
		out = append(out, t)
	}
	c.Findings.Types = out
}

func (p *findingTypesPage) refreshList() {
	p.loading = true
	for p.list.Count() > 0 {
		p.list.TakeItem(0)
	}
	for _, d := range p.drafts {
		p.list.AddItem(typeListLabel(d))
	}
	p.loading = false
}

func typeListLabel(d typeDraft) string {
	if name := strings.TrimSpace(d.t.Name); name != "" {
		return name
	}
	return d.id()
}

func (p *findingTypesPage) selectDraft(row int) {
	p.selected = row
	if row < 0 || row >= len(p.drafts) {
		p.stack.SetCurrentWidget(p.empty)
		return
	}
	p.stack.SetCurrentWidget(p.editor)
	d := p.drafts[row]
	p.loading = true
	p.name.SetText(d.t.Name)
	p.swatch.setColor(d.t.Color)
	p.showID(d)
	p.loading = false
}

// showID writes the Id row. A type that is already in the settings file
// shows its id plainly; one added here shows the id it is about to take.
func (p *findingTypesPage) showID(d typeDraft) {
	if d.fresh {
		p.idLabel.SetText(d.id() + "  (set when you apply)")
		return
	}
	p.idLabel.SetText(d.t.ID)
}

// edit applies a change to the selected type and keeps its row's label in
// step.
func (p *findingTypesPage) edit(mutate func(*typeDraft)) {
	if p.loading || p.selected < 0 || p.selected >= len(p.drafts) {
		return
	}
	mutate(&p.drafts[p.selected])
	if item := p.list.Item(p.selected); item != nil {
		item.SetText(typeListLabel(p.drafts[p.selected]))
	}
	p.showID(p.drafts[p.selected]) // a fresh type's id follows its name
}

func (p *findingTypesPage) pickColor() {
	if p.selected < 0 || p.selected >= len(p.drafts) {
		return
	}
	current := qt.NewQColor6(p.drafts[p.selected].t.Color)
	chosen := qt.QColorDialog_GetColor3(current, p.editor, "Finding type colour")
	if chosen == nil || !chosen.IsValid() {
		return // the user cancelled
	}
	hex := strings.ToLower(chosen.Name())
	p.edit(func(d *typeDraft) { d.t.Color = hex })
	p.swatch.setColor(hex)
}

// addType appends a new type with the next unused palette colour and
// selects it for naming. Its id follows the name until the settings are
// applied (see typeDraft).
func (p *findingTypesPage) addType() {
	used := make([]string, 0, len(p.drafts))
	for _, d := range p.drafts {
		used = append(used, d.t.Color)
	}
	p.drafts = append(p.drafts, typeDraft{
		t: findings.Type{
			Name:  p.uniqueName("New type"),
			Color: findings.PickColor(used),
		},
		fresh: true,
	})
	p.refreshList()
	p.list.SetCurrentRow(len(p.drafts) - 1)
	p.selectDraft(len(p.drafts) - 1)
	p.name.SetFocus()
	p.name.SelectAll()
}

func (p *findingTypesPage) removeSelected() {
	if p.selected < 0 || p.selected >= len(p.drafts) {
		return
	}
	d := p.drafts[p.selected]
	if ok, _ := confirm(p.list.QWidget, confirmOptions{
		title:   "Remove finding type",
		heading: "Remove \"" + typeListLabel(d) + "\"?",
		body: []string{
			"Findings already tagged with it keep the tag and show its id until a " +
				"type with that id exists again.",
		},
		cancel: "Cancel",
		ok:     "Remove the Type",
	}); !ok {
		return
	}
	p.drafts = append(p.drafts[:p.selected], p.drafts[p.selected+1:]...)
	p.refreshList()
	if len(p.drafts) == 0 {
		p.list.SetCurrentRow(-1)
		p.selectDraft(-1)
		return
	}
	row := min(p.selected, len(p.drafts)-1)
	p.list.SetCurrentRow(row)
	p.selectDraft(row)
}

// uniqueID makes sure two types never share an id, since a finding refers to
// a type by nothing else.
func uniqueID(base string, taken map[string]bool) string {
	if base == "" {
		base = "type"
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if !taken[id] {
			return id
		}
	}
}

func (p *findingTypesPage) uniqueName(base string) string {
	taken := func(name string) bool {
		for _, d := range p.drafts {
			if strings.EqualFold(d.t.Name, name) {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s %d", base, i)
		if !taken(name) {
			return name
		}
	}
}
