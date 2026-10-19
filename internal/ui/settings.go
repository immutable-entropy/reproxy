package ui

import (
	"reflect"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/config"
)

// Settings sections. Pass one to runSettingsDialog to open it there.
const (
	settingsGeneral  = "General"
	settingsProxy    = "Traffic Proxy"
	settingsOutbound = "Outbound Traffic"
	settingsBrowser  = "Browser"
	settingsTargets  = "Targets"
)

// Settings groups: the user's own settings, and those the open engagement
// overrides (see config.EngagementSettings).
const (
	settingsGroupGlobal     = "Global"
	settingsGroupEngagement = "Engagement"
)

// settingsPage is one section of the settings window. load fills the
// widgets from a config; store copies the widgets back into one. Each page
// owns its fields; adding a section means adding a page to
// runSettingsDialog.
type settingsPage struct {
	title       string
	description string
	// group is the heading the page is listed under in the navigation
	// ("Global", or the open engagement); pages with none are listed
	// without headings. disabled greys the page out in the navigation, as
	// the engagement's pages are with no engagement open.
	group    string
	disabled string // why, shown as the tooltip; "" means enabled
	body     *qt.QWidget
	load     func(config.Config)
	store    func(*config.Config)

	// The rest are optional. shown runs each time the page is brought to
	// the front. dirty and save are for what a page keeps outside
	// config.json (secrets): dirty reports unsaved changes, and save writes
	// them, before the settings are applied.
	shown func()
	dirty func() bool
	save  func() error
}

// liveSettings are the settings the main window has to act on itself when
// they change, rather than reading them again later: they alter the window
// as it stands. Both are called on Apply or OK, only when the setting
// actually changed.
type liveSettings struct {
	theme func(themeKind)
	nav   func(navSide)
}

// runSettingsDialog shows the modal settings window, IDE style: sections on
// the left, the selected section's options on the right. Changes are
// applied through the controller on Apply or OK; if that fails the error is
// shown and nothing changes.
func runSettingsDialog(parent *qt.QWidget, ctrl Controller, open string, live liveSettings) {
	agentsSettings, agents := newAgentsPage(ctrl)
	outboundSettings, upstreams := newOutboundPage(ctrl)
	// The open engagement's overrides are saved to its folder, not
	// config.json: the pages save them themselves (see
	// settings_engagement.go).
	engagementPages := newEngagementPages(ctrl)

	// Agent profiles and keys are saved separately from config.Config, so
	// they have their own baseline for the dirty check and apply. Upstream
	// proxy passwords are kept in the secrets file too, so they have one as
	// well.
	type agentState struct {
		agents config.Agents
		keys   map[string]string
	}
	snapshotAgents := func() agentState {
		a, k := agents.agentsAndKeys()
		return agentState{a, k}
	}
	var agentBaseline agentState
	var passwordBaseline map[string]string

	runPagedDialog(pagedDialog{
		parent: parent,
		ctrl:   ctrl,
		title:  "Settings",
		open:   open,
		makePages: func(*qt.QDialog) []*settingsPage {
			global := []*settingsPage{
				newGeneralPage(),
				newProxyPage(ctrl),
				outboundSettings,
				newTargetsSettingsPage(ctrl),
				newFindingsPage(),
				newBrowserPage(ctrl.InstalledBrowsers()),
				agentsSettings,
			}
			for _, p := range global {
				p.group = settingsGroupGlobal
			}
			return append(global, engagementPages...)
		},
		loaded: func() {
			agentBaseline = snapshotAgents()
			passwordBaseline = upstreams.passwords()
		},
		dirty: func() bool {
			return !reflect.DeepEqual(snapshotAgents(), agentBaseline) ||
				!reflect.DeepEqual(upstreams.passwords(), passwordBaseline)
		},
		// Passwords go first: ApplySettings rebuilds the upstream dialer and
		// reads them back out of the secrets file as it does. If the
		// settings are then rejected, a password for a proxy that was never
		// applied is all that is left behind.
		before: func() error {
			if pw := upstreams.passwords(); !reflect.DeepEqual(pw, passwordBaseline) {
				if err := ctrl.SaveUpstreamPasswords(pw); err != nil {
					return err
				}
				passwordBaseline = pw
			}
			return nil
		},
		after: func(prev, next config.Config) error {
			if a, k := agents.agentsAndKeys(); !reflect.DeepEqual(agentState{a, k}, agentBaseline) {
				if err := ctrl.SaveAgentConfig(a, k); err != nil {
					return err
				}
				agentBaseline = agentState{a, k}
			}
			// Settings only take visible effect now, on Apply/OK — including
			// the theme and the navigation bar's position, neither of which
			// is previewed as it is picked.
			if live.theme != nil && themeKindFor(next.Theme) != themeKindFor(prev.Theme) {
				live.theme(themeKindFor(next.Theme))
			}
			if live.nav != nil && navSideFor(next.NavPosition) != navSideFor(prev.NavPosition) {
				live.nav(navSideFor(next.NavPosition))
			}
			return nil
		},
	})
}

// pagedDialog describes a modal window laid out like Settings: sections on
// the left, the selected section's page on the right, and Cancel, Apply and
// OK below. Every page edits part of one config.Config, which is applied
// through the controller on Apply or OK. The Plugins window is one too.
type pagedDialog struct {
	parent *qt.QWidget
	ctrl   Controller
	title  string
	// open is the title of the page to show first.
	open string
	// makePages builds the pages; it is handed the dialog, for anything that
	// needs a parent.
	makePages func(dlg *qt.QDialog) []*settingsPage

	// The rest are optional, for what the dialog saves outside
	// config.Config. loaded runs once the pages have been filled; dirty
	// reports unsaved changes the pages don't; before runs ahead of
	// ApplySettings and after once it succeeds, given the config as it was
	// and as it now is.
	loaded func()
	dirty  func() bool
	before func() error
	after  func(prev, next config.Config) error
}

func runPagedDialog(d pagedDialog) {
	dlg := qt.NewQDialog(d.parent)
	dlg.SetWindowTitle(d.title)
	dlg.Resize(960, 620)
	dlg.SetMinimumSize2(720, 480)

	ctrl := d.ctrl
	current := ctrl.Settings()
	pages := d.makePages(dlg)
	for _, p := range pages {
		p.load(current)
	}
	if d.loaded != nil {
		d.loaded()
	}

	root := qt.NewQVBoxLayout(dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	// Body: navigation | page.
	body := qt.NewQHBoxLayout2()
	body.SetSpacing(0)

	nav := qt.NewQListWidget2()
	setName(nav.QObject, "SettingsNav")
	nav.SetFixedWidth(200)
	nav.SetHorizontalScrollBarPolicy(qt.ScrollBarAlwaysOff)

	// The navigation lists the pages under their group's heading, when
	// they have one. rowPage maps a row back to its page (-1: a heading).
	stack := qt.NewQStackedWidget2()
	var rowPage []int
	group := ""
	for i, p := range pages {
		if p.group != "" && p.group != group {
			group = p.group
			head := qt.NewQListWidgetItem7(group, nav)
			head.SetFlags(qt.NoItemFlags)
			font := head.Font()
			font.SetBold(true)
			font.SetPointSizeF(font.PointSizeF() * 0.85)
			head.SetFont(font)
			height := 28
			if len(rowPage) > 0 {
				height = 40 // room above every heading but the first
			}
			head.SetTextAlignment(int(qt.AlignLeft | qt.AlignBottom))
			head.SetSizeHint(qt.NewQSize2(0, height))
			rowPage = append(rowPage, -1)
		}
		item := qt.NewQListWidgetItem7(p.title, nav)
		item.SetSizeHint(qt.NewQSize2(0, 32))
		if p.disabled != "" {
			item.SetFlags(qt.NoItemFlags)
			item.SetToolTip(p.disabled)
		}
		rowPage = append(rowPage, i)
		stack.AddWidget(wrapPage(p))
	}
	nav.OnCurrentRowChanged(func(row int) {
		if row < 0 || row >= len(rowPage) || rowPage[row] < 0 {
			return
		}
		i := rowPage[row]
		stack.SetCurrentIndex(i)
		if pages[i].shown != nil {
			pages[i].shown()
		}
	})

	body.AddWidget(nav.QWidget)
	body.AddWidget2(stack.QWidget, 1)
	root.AddLayout2(body.QLayout, 1)

	// Footer: error message, then buttons.
	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	f := qt.NewQHBoxLayout(footer)
	f.SetContentsMargins(16, 10, 16, 10)
	f.SetSpacing(8)

	errLabel := qt.NewQLabel2()
	setName(errLabel.QObject, "ErrorText")
	errLabel.SetWordWrap(true)
	okBtn := qt.NewQPushButton3("OK")
	okBtn.SetDefault(true)
	setName(okBtn.QObject, "PrimaryButton")
	cancelBtn := qt.NewQPushButton3("Cancel")
	cancelBtn.SetAutoDefault(false)
	applyBtn := qt.NewQPushButton3("Apply")
	applyBtn.SetAutoDefault(false)
	f.AddWidget2(errLabel.QWidget, 1)
	f.AddWidget(cancelBtn.QWidget)
	f.AddWidget(applyBtn.QWidget)
	f.AddWidget(okBtn.QWidget)
	root.AddWidget(footer)

	// compose builds a config from the current widget state over the
	// last-applied one, so fields no page manages are preserved.
	compose := func() config.Config {
		next := current
		for _, p := range pages {
			p.store(&next)
		}
		return next
	}
	// baseline is the composed config at the last apply (or dialog open), so
	// "changed" is measured against what was last saved, not against the raw
	// config (which normalises nil/empty slices differently than store does).
	baseline := compose()
	pagesDirty := func() bool {
		for _, p := range pages {
			if p.dirty != nil && p.dirty() {
				return true
			}
		}
		return false
	}
	dirty := func() bool {
		return !reflect.DeepEqual(compose(), baseline) ||
			(d.dirty != nil && d.dirty()) ||
			pagesDirty()
	}

	apply := func() bool {
		next := compose()
		fail := func(err error) bool {
			errLabel.SetText(err.Error())
			return false
		}
		if d.before != nil {
			if err := d.before(); err != nil {
				return fail(err)
			}
		}
		for _, p := range pages {
			if p.save != nil && p.dirty != nil && p.dirty() {
				if err := p.save(); err != nil {
					return fail(err)
				}
			}
		}
		if err := ctrl.ApplySettings(next); err != nil {
			return fail(err)
		}
		if d.after != nil {
			if err := d.after(current, next); err != nil {
				return fail(err)
			}
		}
		current = next
		baseline = compose()
		errLabel.SetText("")
		applyBtn.SetEnabled(false) // just saved: nothing pending
		return true
	}
	applyBtn.SetEnabled(false) // nothing changed yet
	applyBtn.OnClicked(func() { apply() })
	okBtn.OnClicked(func() {
		if !dirty() {
			dlg.Accept()
			return
		}
		if apply() {
			dlg.Accept()
		}
	})
	cancelBtn.OnClicked(dlg.Reject)

	// Enable Apply only while there are unsaved changes. Polling keeps this
	// simple and correct across every control, including the dynamic rules
	// editor, without wiring a change signal to each widget.
	watch := qt.NewQTimer()
	watch.SetInterval(200)
	watch.OnTimeout(func() { applyBtn.SetEnabled(dirty()) })
	watch.Start2()

	// Open on the page asked for (the first with that title), or the
	// first page. Selecting a row other than 0 runs its shown hook.
	start := -1
	for row, i := range rowPage {
		if i >= 0 && pages[i].disabled == "" && (start < 0 || (pages[i].title == d.open && pages[rowPage[start]].title != d.open)) {
			start = row
		}
	}
	nav.SetCurrentRow(start)
	if start == 0 && pages[0].shown != nil {
		pages[0].shown() // row 0 is already current: no change signal
	}

	dlg.Exec()
	watch.Stop() // stop polling before the widgets go away
	dlg.DeleteLater()
}

// wrapPage adds the page title and a scroll area around a page body.
func wrapPage(p *settingsPage) *qt.QWidget {
	content := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(content)
	v.SetContentsMargins(28, 22, 28, 22)
	v.SetSpacing(6)

	title := qt.NewQLabel3(p.title)
	setName(title.QObject, "SettingsPageTitle")
	v.AddWidget(title.QWidget)
	if p.description != "" {
		desc := qt.NewQLabel3(p.description)
		setName(desc.QObject, "Muted")
		desc.SetWordWrap(true)
		v.AddWidget(desc.QWidget)
	}
	v.AddSpacing(14)
	v.AddWidget(p.body)
	v.AddStretch()

	scroll := qt.NewQScrollArea2()
	setName(scroll.QObject, "SettingsPage")
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(content)
	return scroll.QWidget
}

// sectionTitle returns a bold heading for a group of options on a page.
func sectionTitle(text string) *qt.QWidget {
	l := qt.NewQLabel3(text)
	setName(l.QObject, "SettingsSection")
	return l.QWidget
}

// helpText returns a muted, wrapping explanation label.
func helpText(text string) *qt.QLabel {
	l := qt.NewQLabel3(text)
	setName(l.QObject, "Muted")
	l.SetWordWrap(true)
	return l
}

func newGeneralPage() *settingsPage {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Appearance"))
	form := newSettingsForm()
	themeSel := qt.NewQComboBox2()
	for _, t := range themeOptions {
		themeSel.AddItem(t.name)
	}
	themeSel.SetMinimumWidth(240)
	form.AddRow3("Theme", themeSel.QWidget)
	v.AddLayout(form.QLayout)
	v.AddWidget(helpText("Light is the default. Dark is an IDE-style dark theme; " +
		"Matrix is dark with bright phosphor green. The theme changes when you " +
		"click Apply or OK.").QWidget)

	v.AddSpacing(10)
	v.AddWidget(sectionTitle("Navigation"))
	navForm := newSettingsForm()
	navSel := qt.NewQComboBox2()
	fillCombo(navSel, navSideOptions)
	navSel.SetMinimumWidth(240)
	navForm.AddRow3("Anchor the navigation bar", navSel.QWidget)
	v.AddLayout(navForm.QLayout)
	v.AddWidget(helpText("The navigation bar switches between Targets, Traffic, Replay, " +
		"Explorer and Findings. Hidden leaves the window to the current section. " +
		"View → Navigation anchors the bar too, and shows the section names beside " +
		"the icons.").QWidget)

	return &settingsPage{
		title:       settingsGeneral,
		description: "General preferences.",
		body:        body,
		load: func(c config.Config) {
			kind := themeKindFor(c.Theme)
			for i, t := range themeOptions {
				if t.kind == kind {
					themeSel.SetCurrentIndex(i)
					break
				}
			}
			selectOption(navSel, navSideOptions, navSideFor(c.NavPosition))
		},
		store: func(c *config.Config) {
			if i := themeSel.CurrentIndex(); i >= 0 && i < len(themeOptions) {
				c.Theme = string(themeOptions[i].kind)
			}
			c.NavPosition = string(selectedOption(navSel, navSideOptions))
		},
	}
}
