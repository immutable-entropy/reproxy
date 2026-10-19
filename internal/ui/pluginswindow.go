package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/config"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/manifest"
	"reproxy/internal/plugin/registry"
)

// Plugins window sections. Pass one to runPluginsWindow to open it there.
const (
	pluginsSection      = "Plugins"
	repositoriesSection = "Repositories"
)

// runPluginsWindow shows the Plugins window, laid out like Settings: the
// installed plugins in one section, the repositories they are added from in
// another. What the user changes is applied on Apply or OK, like a setting.
func runPluginsWindow(parent *qt.QWidget, ctrl Controller, open string) {
	runPagedDialog(pagedDialog{
		parent: parent,
		ctrl:   ctrl,
		title:  "Plugins",
		open:   open,
		makePages: func(dlg *qt.QDialog) []*settingsPage {
			plugins, repos := newPluginPages(ctrl, dlg.QObject)
			return []*settingsPage{plugins, repos}
		},
	})
}

// pluginsPage backs both sections of the Plugins window. The Plugins
// section is every plugin installed in the data directory's plugins folder
// (added from a repository, or put there by hand): whether it runs, its
// settings, and how it is doing. The Repositories section is where plugins
// are added from.
//
// Showing the Plugins section looks for plugins again, so a plugin dropped
// into the folder (or edited) shows up without a restart. What the user
// changes here is kept in the settings file (secret settings in the secrets
// file); the plugin's own plugin.json holds the defaults, and is never
// written.
type pluginsPage struct {
	ctrl   Controller
	parent *qt.QObject

	infos  map[string]plugin.Info
	order  []string
	drafts map[string]*pluginDraft
	// secrets are secret settings typed in this window and not yet saved,
	// by plugin then key; "" removes a secret.
	secrets map[string]map[string]string

	allowRelease *qt.QCheckBox
	allowHold    *qt.QCheckBox
	// repos is the repositories the browse window reads from, edited in the
	// Repositories section.
	repos    []string
	repoList *qt.QListWidget
	list     *qt.QListWidget
	// rows are the list's lines, by plugin, in order.
	rows []pluginRow

	// updates are the newer versions the repositories offer of installed
	// plugins, by name. They are looked for once each time the window
	// opens (and again after the browse window closes); checking says a
	// look is under way.
	updates      map[string]registry.Plugin
	checking     bool
	updatesBadge *qt.QLabel
	updateBar    *qt.QWidget
	updateText   *qt.QLabel
	updateBtn    *qt.QPushButton
	stack        *qt.QStackedWidget
	empty        *qt.QWidget
	detail       *qt.QWidget

	title       *qt.QLabel
	meta        *qt.QLabel
	description *qt.QLabel
	state       *qt.QLabel
	problems    *qt.QLabel
	enabled     *qt.QCheckBox
	hooks       *qt.QLabel
	stats       *qt.QLabel
	settingsBox *qt.QVBoxLayout
	form        *qt.QWidget
	logView     *codeEditor
	logShown    int
	reload      *qt.QPushButton
	openDir     *qt.QPushButton

	selected string
	loading  bool
	live     *qt.QTimer
}

// pluginRow is one line of the installed list: the plugin's name, its
// state, and a badge when an update is available.
type pluginRow struct {
	state *qt.QLabel
	badge *qt.QLabel
}

// pluginDraft is what the user has chosen for one plugin in this dialog.
type pluginDraft struct {
	status   manifest.Status            // "" keeps the plugin's default
	settings map[string]json.RawMessage // only the settings changed from the default
}

// newPluginPages builds the Plugins window's two sections, which share one
// pluginsPage: the plugins, and the repositories they come from.
func newPluginPages(ctrl Controller, parent *qt.QObject) (plugins, repositories *settingsPage) {
	p := &pluginsPage{
		ctrl: ctrl, parent: parent, infos: map[string]plugin.Info{}, drafts: map[string]*pluginDraft{},
		secrets: map[string]map[string]string{}, updates: map[string]registry.Plugin{},
	}
	plugins = &settingsPage{
		title:       pluginsSection,
		description: "TypeScript that runs on reProxy's events, and acts on traffic, findings and the Entity Explorer.",
		body:        p.buildPluginsBody(),
		load:        p.load,
		store:       p.store,
		shown:       p.shown,
		dirty:       func() bool { return len(p.secrets) > 0 },
		save:        p.saveSecrets,
	}
	repositories = &settingsPage{
		title:       repositoriesSection,
		description: "Where plugins are added from: GitHub repositories of reProxy plugins.",
		body:        p.buildRepositoriesBody(),
		load:        func(config.Config) {}, // filled with the Plugins section's load
		store:       func(c *config.Config) { c.Plugins.Repositories = append([]string(nil), p.repos...) },
	}
	return plugins, repositories
}

func (p *pluginsPage) buildPluginsBody() *qt.QWidget {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	head := qt.NewQHBoxLayout2()
	head.SetContentsMargins(0, 0, 0, 0)
	head.AddWidget(sectionTitle("Installed"))
	p.updatesBadge = qt.NewQLabel2()
	setName(p.updatesBadge.QObject, "UpdateBadge")
	p.updatesBadge.SetToolTip("Newer versions of these plugins are in the repositories they were added from")
	p.updatesBadge.Hide()
	head.AddWidget(p.updatesBadge.QWidget)
	head.AddStretch()
	add := newIconButton(iconPlus, "Add plugins from a repository", p.browse)
	head.AddWidget(add.w)
	v.AddLayout(head.QLayout)
	v.AddWidget(helpText("Plugins are TypeScript that runs inside reProxy on the events they choose: " +
		"traffic passing, a message held by interception, a finding recorded. Add them from a " +
		"repository with the + above (see Repositories), or put your own in the plugins folder. " +
		"They are looked for again each time this section opens, and the repositories are asked " +
		"for newer versions of the ones added from them each time this window opens.").QWidget)

	row := qt.NewQHBoxLayout2()
	row.SetSpacing(12)

	left := qt.NewQVBoxLayout2()
	left.SetSpacing(6)
	p.list = qt.NewQListWidget2()
	setName(p.list.QObject, "MasterList")
	p.list.SetFixedWidth(230)
	p.list.SetMinimumHeight(240)
	p.list.SetFrameShape(qt.QFrame__NoFrame)
	p.list.OnCurrentRowChanged(func(r int) {
		if !p.loading && r >= 0 && r < len(p.order) {
			p.selectPlugin(p.order[r])
		}
	})
	left.AddWidget(p.list.QWidget)
	reload := qt.NewQPushButton3("Reload")
	reload.SetAutoDefault(false)
	reload.SetToolTip("Look for new and changed plugins")
	reload.OnClicked(p.rescan)
	folder := qt.NewQPushButton3("Open Plugins Folder")
	folder.SetAutoDefault(false)
	folder.SetToolTip("Where plugins are installed, and where your own go: " + plugin.UserDir(p.ctrl.DataDir()))
	folder.OnClicked(func() {
		dir := plugin.UserDir(p.ctrl.DataDir())
		_ = os.MkdirAll(dir, 0o755)
		qt.QDesktopServices_OpenUrl(qt.QUrl_FromLocalFile(dir))
	})
	left.AddWidget(reload.QWidget)
	left.AddWidget(folder.QWidget)
	left.AddStretch()
	row.AddLayout(left.QLayout)

	p.stack = qt.NewQStackedWidget2()
	p.empty = qt.NewQWidget2()
	ev := qt.NewQVBoxLayout(p.empty)
	emptyLabel := qt.NewQLabel3("No plugin selected. Choose one on the left, or add plugins with the + above.")
	setName(emptyLabel.QObject, "EmptyState")
	emptyLabel.SetWordWrap(true)
	ev.AddStretch()
	ev.AddWidget(emptyLabel.QWidget)
	ev.AddStretch()
	p.detail = p.buildDetail()
	// The detail wraps to the width it is given rather than widening the
	// page: long hook filters and paths would otherwise scroll it sideways.
	p.stack.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	p.stack.AddWidget(p.empty)
	p.stack.AddWidget(p.detail)
	// A bordered panel sets the detail apart from the list.
	detailPanel := qt.NewQFrame2()
	setName(detailPanel.QObject, "Panel")
	detailPanel.SetFrameShape(qt.QFrame__StyledPanel)
	dp := qt.NewQVBoxLayout(detailPanel.QWidget)
	dp.SetContentsMargins(12, 10, 12, 10)
	dp.AddWidget(p.stack.QWidget)
	row.AddWidget2(detailPanel.QWidget, 1)
	v.AddLayout2(row.QLayout, 1)

	v.AddSpacing(6)
	v.AddWidget(sectionTitle("Interception"))
	p.allowRelease = qt.NewQCheckBox3("Let plugins forward or drop held messages")
	v.AddWidget(p.allowRelease.QWidget)
	v.AddWidget(helpText("A plugin on an intercept hook sees each held message before you do and may " +
		"edit it. With this on it may also send it on or drop it, and you never see it; off, " +
		"everything interception holds still reaches you.").QWidget)
	p.allowHold = qt.NewQCheckBox3("Let plugins hold traffic for themselves")
	v.AddWidget(p.allowHold.QWidget)
	v.AddWidget(helpText("A plugin can ask for the messages it targets to be held for it (hold: true), " +
		"even with interception off, so that it can rewrite or drop them on the way through. You " +
		"don't see those messages; the plugin's own filter decides which are held, and only they " +
		"wait on it. Off, plugins only see what your interception holds.").QWidget)

	// Counters and logs move while the window is open.
	p.live = qt.NewQTimer2(p.parent)
	p.live.SetInterval(1000)
	p.live.OnTimeout(p.refreshLive)
	return body
}

func (p *pluginsPage) buildRepositoriesBody() *qt.QWidget {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	head := qt.NewQHBoxLayout2()
	head.SetContentsMargins(0, 0, 0, 0)
	head.AddWidget(sectionTitle("Repositories"))
	head.AddStretch()
	browse := qt.NewQPushButton3("Browse Plugins…")
	browse.SetAutoDefault(false)
	browse.SetToolTip("See the plugins these repositories offer, and add them")
	browse.OnClicked(p.browse)
	head.AddWidget(browse.QWidget)
	v.AddLayout(head.QLayout)
	v.AddWidget(helpText("Each is a GitHub repository with a plugin.json at its root, or in directories " +
		"under it, src/ or plugins/. A plugin added from one is installed in the plugins folder, " +
		"disabled until you enable it in the Plugins section.").QWidget)

	p.repoList = qt.NewQListWidget2()
	setName(p.repoList.QObject, "MasterList")
	p.repoList.SetMinimumHeight(200)
	p.repoList.SetFrameShape(qt.QFrame__NoFrame)
	v.AddWidget(p.repoList.QWidget)
	repoBtns := qt.NewQHBoxLayout2()
	repoBtns.SetSpacing(6)
	addRepo := qt.NewQPushButton3("Add Repository…")
	addRepo.SetAutoDefault(false)
	addRepo.OnClicked(p.addRepository)
	removeRepo := qt.NewQPushButton3("Remove")
	removeRepo.SetAutoDefault(false)
	removeRepo.OnClicked(p.removeRepository)
	repoBtns.AddWidget(addRepo.QWidget)
	repoBtns.AddWidget(removeRepo.QWidget)
	repoBtns.AddStretch()
	v.AddLayout(repoBtns.QLayout)
	return body
}

func (p *pluginsPage) buildDetail() *qt.QWidget {
	w := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(w)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(8)

	p.title = qt.NewQLabel2()
	setName(p.title.QObject, "SettingsSection")
	p.meta = qt.NewQLabel2()
	setName(p.meta.QObject, "Muted")
	p.meta.SetTextInteractionFlags(qt.TextSelectableByMouse)
	p.meta.SetWordWrap(true)
	p.description = qt.NewQLabel2()
	p.description.SetWordWrap(true)
	p.state = qt.NewQLabel2()
	p.problems = qt.NewQLabel2()
	setName(p.problems.QObject, "ErrorText")
	p.problems.SetWordWrap(true)
	p.problems.SetTextInteractionFlags(qt.TextSelectableByMouse)

	p.enabled = qt.NewQCheckBox3("Enabled")
	p.enabled.OnToggled(func(on bool) {
		if p.loading {
			return
		}
		d := p.draft(p.selected)
		in := p.infos[p.selected]
		want := manifest.Disabled
		if on {
			want = manifest.Enabled
		}
		d.status = want
		if in.Manifest != nil && in.Manifest.Status == want {
			d.status = "" // the default: nothing to keep
		}
	})

	p.updateBar = qt.NewQFrame2().QWidget
	setName(p.updateBar.QObject, "UpdateBar")
	p.updateBar.SetAttribute(qt.WA_StyledBackground)
	ub := qt.NewQHBoxLayout(p.updateBar)
	ub.SetContentsMargins(10, 6, 6, 6)
	ub.SetSpacing(8)
	p.updateText = qt.NewQLabel2()
	p.updateText.SetWordWrap(true)
	ub.AddWidget2(p.updateText.QWidget, 1)
	p.updateBtn = qt.NewQPushButton3("Update")
	p.updateBtn.SetAutoDefault(false)
	setName(p.updateBtn.QObject, "PrimaryButton")
	p.updateBtn.OnClicked(func() { p.update(p.selected) })
	ub.AddWidget(p.updateBtn.QWidget)
	p.updateBar.Hide()

	actions := qt.NewQHBoxLayout2()
	actions.SetSpacing(8)
	actions.AddWidget(p.enabled.QWidget)
	actions.AddStretch()
	p.reload = qt.NewQPushButton3("Reload")
	p.reload.SetAutoDefault(false)
	p.reload.SetToolTip("Load the plugin again from its files (and clear a fault)")
	p.reload.OnClicked(func() {
		name := p.selected
		go func() {
			_ = p.ctrl.Plugins().Reload(name)
			mainthread.Start(p.reloadInfos)
		}()
	})
	p.openDir = qt.NewQPushButton3("Open Folder")
	p.openDir.SetAutoDefault(false)
	p.openDir.OnClicked(func() {
		if in, ok := p.infos[p.selected]; ok {
			qt.QDesktopServices_OpenUrl(qt.QUrl_FromLocalFile(in.Path))
		}
	})
	actions.AddWidget(p.reload.QWidget)
	actions.AddWidget(p.openDir.QWidget)

	p.hooks = qt.NewQLabel2()
	p.hooks.SetWordWrap(true)
	p.hooks.SetTextFormat(qt.RichText)
	p.stats = qt.NewQLabel2()
	setName(p.stats.QObject, "Muted")

	v.AddWidget(p.title.QWidget)
	v.AddWidget(p.meta.QWidget)
	v.AddWidget(p.description.QWidget)
	v.AddWidget(p.state.QWidget)
	v.AddWidget(p.updateBar)
	v.AddWidget(p.problems.QWidget)
	v.AddLayout(actions.QLayout)
	v.AddSpacing(4)
	v.AddWidget(sectionTitle("Hooks"))
	v.AddWidget(p.hooks.QWidget)
	v.AddWidget(p.stats.QWidget)
	v.AddSpacing(4)
	v.AddWidget(sectionTitle("Settings"))
	p.settingsBox = qt.NewQVBoxLayout2()
	v.AddLayout(p.settingsBox.QLayout)
	v.AddSpacing(4)
	p.logView = newCodeEditor(editorOptions{title: "Log", wrap: true})
	p.logView.setReadOnly(true)
	p.logView.root.SetMinimumHeight(160)
	v.AddWidget2(p.logView.root, 1)
	return w
}

// ---- Data flow ---------------------------------------------------------------

func (p *pluginsPage) load(c config.Config) {
	p.allowRelease.SetChecked(c.Plugins.AllowRelease)
	p.allowHold.SetChecked(c.Plugins.AllowHold)
	p.drafts = map[string]*pluginDraft{}
	for name, o := range c.Plugins.Overrides {
		d := &pluginDraft{status: manifest.Status(o.Status), settings: map[string]json.RawMessage{}}
		for k, v := range o.Settings {
			d.settings[k] = v
		}
		p.drafts[name] = d
	}
	p.secrets = map[string]map[string]string{}
	p.repos = append([]string(nil), c.Plugins.Repositories...)
	p.refreshRepos()
	p.reloadInfos()
	p.checkUpdates() // load runs once, as the window opens
}

func (p *pluginsPage) store(c *config.Config) {
	c.Plugins.AllowRelease = p.allowRelease.IsChecked()
	c.Plugins.AllowHold = p.allowHold.IsChecked()
	c.Plugins.Repositories = append([]string(nil), p.repos...)
	var out map[string]config.PluginOverride
	names := make([]string, 0, len(p.drafts))
	for name := range p.drafts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d := p.drafts[name]
		if d.status == "" && len(d.settings) == 0 {
			continue
		}
		if out == nil {
			out = map[string]config.PluginOverride{}
		}
		o := config.PluginOverride{Status: string(d.status)}
		if len(d.settings) > 0 {
			o.Settings = map[string]json.RawMessage{}
			for k, v := range d.settings {
				o.Settings[k] = v
			}
		}
		out[name] = o
	}
	c.Plugins.Overrides = out
}

func (p *pluginsPage) saveSecrets() error {
	for name, values := range p.secrets {
		if err := p.ctrl.SavePluginSecrets(name, values); err != nil {
			return err
		}
	}
	p.secrets = map[string]map[string]string{}
	p.buildSettingsForm() // "set" / "not set" follows what was saved
	return nil
}

func (p *pluginsPage) draft(name string) *pluginDraft {
	d, ok := p.drafts[name]
	if !ok {
		d = &pluginDraft{settings: map[string]json.RawMessage{}}
		p.drafts[name] = d
	}
	return d
}

// shown looks for plugins again, off the UI thread, and starts the live
// counters.
func (p *pluginsPage) shown() {
	p.rescan()
	p.live.Start2()
}

func (p *pluginsPage) rescan() {
	go func() {
		p.ctrl.Plugins().Rescan()
		mainthread.Start(p.reloadInfos)
	}()
}

// reloadInfos refills the list from the manager, keeping the selection.
func (p *pluginsPage) reloadInfos() {
	list := p.ctrl.Plugins().List()
	p.infos = map[string]plugin.Info{}
	p.order = p.order[:0]
	for _, in := range list {
		p.infos[in.Name] = in
		p.order = append(p.order, in.Name)
	}
	p.loading = true
	p.list.Clear()
	p.rows = p.rows[:0]
	row := -1
	for i, name := range p.order {
		in := p.infos[name]
		item := qt.NewQListWidgetItem()
		item.SetToolTip(in.Path)
		w, r := p.buildRow(name)
		item.SetSizeHint(qt.NewQSize2(0, pluginRowHeight))
		p.list.AddItemWithItem(item)
		p.list.SetItemWidget(item, w)
		p.rows = append(p.rows, r)
		if name == p.selected {
			row = i
		}
	}
	p.refreshRows()
	p.loading = false
	if len(p.order) == 0 {
		p.stack.SetCurrentWidget(p.empty)
		p.selected = ""
		return
	}
	if row < 0 {
		row = 0
	}
	p.list.SetCurrentRow(row)
	p.selectPlugin(p.order[row])
}

// pluginRowHeight is the height of a line in the installed list, the
// list's item padding included.
const pluginRowHeight = 38

// buildRow is one plugin's line in the list: its name, its state (filled
// by refreshRows) and an update badge. The list's item padding spaces it.
func (p *pluginsPage) buildRow(name string) (*qt.QWidget, pluginRow) {
	w := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(w)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(6)
	label := qt.NewQLabel3(name)
	label.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	label.SetToolTip(name) // the list is narrow: a long name reads here
	state := qt.NewQLabel2()
	setName(state.QObject, "Muted")
	badge := qt.NewQLabel3("Update")
	setName(badge.QObject, "UpdateBadge")
	badge.Hide()
	h.AddWidget2(label.QWidget, 1)
	h.AddWidget(state.QWidget)
	h.AddWidget(badge.QWidget)
	return w, pluginRow{state: state, badge: badge}
}

// refreshRows brings each line's state and update badge up to date, and
// the count of updates beside the heading.
func (p *pluginsPage) refreshRows() {
	for i, name := range p.order {
		if i >= len(p.rows) {
			break
		}
		r := p.rows[i]
		if text := stateWord(p.infos[name], p.drafts[name]); r.state.Text() != text {
			r.state.SetText(text)
		}
		u, ok := p.updates[name]
		r.badge.SetVisible(ok)
		// The line is only so wide. A badge gives the ordinary states
		// ("running", "stopped") their room back — the detail panel says
		// what the selected plugin is doing anyway — while a state that
		// wants attention stays whatever else is on the line.
		r.state.SetVisible(!ok || wantsAttention(p.infos[name], p.drafts[name]))
		if ok {
			tip := fmt.Sprintf("Version %s is available (%s is installed)", u.Version, u.InstalledVersion)
			if u.InstalledByHand {
				tip += "\nThis copy wasn't installed from a repository: updating replaces your own files"
			}
			r.badge.SetToolTip(tip)
		}
	}
	if n := len(p.updates); n > 0 {
		p.updatesBadge.SetText(fmt.Sprintf("%d %s available", n, plural(n, "update", "updates")))
		p.updatesBadge.Show()
	} else {
		p.updatesBadge.Hide()
	}
}

// wantsAttention reports whether a plugin's state has to be shown in the
// list even when the line is short of room: something went wrong, or the
// user has changed it in the dialog and not applied it yet.
func wantsAttention(in plugin.Info, d *pluginDraft) bool {
	if in.State == plugin.StateError || in.State == plugin.StateFaulted {
		return true
	}
	return d != nil && d.status != "" && d.status != in.Status
}

// stateWord is the plugin's state for the list: what it is doing now, or
// what it will do once the dialog is applied.
func stateWord(in plugin.Info, d *pluginDraft) string {
	switch in.State {
	case plugin.StateError:
		return "problem"
	case plugin.StateFaulted:
		return "faulted"
	}
	if d != nil && d.status != "" && d.status != in.Status {
		return "will be " + string(d.status)
	}
	if in.State == plugin.StateRunning {
		return "running"
	}
	return "disabled"
}

func (p *pluginsPage) selectPlugin(name string) {
	in, ok := p.infos[name]
	if !ok {
		return
	}
	p.selected = name
	p.stack.SetCurrentWidget(p.detail)
	p.loading = true
	defer func() { p.loading = false }()

	man := in.Manifest
	if man != nil {
		p.title.SetText(man.Name + "  " + man.Version)
		p.description.SetText(man.Description)
	} else {
		p.title.SetText(name)
		p.description.SetText("")
	}
	where := in.Path
	if man != nil && man.Author.Name != "" {
		author := man.Author.Name
		if man.Author.Email != "" {
			author += " <" + man.Author.Email + ">"
		}
		where += "  ·  by " + author
	}
	p.meta.SetText(where)
	p.problems.SetVisible(len(in.Problems) > 0)
	p.problems.SetText(strings.Join(in.Problems, "\n"))
	p.showUpdate()

	status := in.Status
	if d := p.drafts[name]; d != nil && d.status != "" {
		status = d.status
	}
	p.enabled.SetEnabled(man != nil)
	p.enabled.SetChecked(status == manifest.Enabled)

	p.hooks.SetText(hookSummary(man))
	p.buildSettingsForm()
	p.logShown = -1
	p.showLive(in)
}

// refreshLive updates the list's states, and the selected plugin's state,
// counters and log.
func (p *pluginsPage) refreshLive() {
	for _, in := range p.ctrl.Plugins().List() {
		if _, ok := p.infos[in.Name]; ok {
			p.infos[in.Name] = in
		}
	}
	p.refreshRows()
	if p.selected == "" {
		return
	}
	in, ok := p.ctrl.Plugins().Get(p.selected)
	if !ok {
		return
	}
	p.showLive(in)
}

func (p *pluginsPage) showLive(in plugin.Info) {
	state := map[plugin.State]string{
		plugin.StateRunning:  "Running",
		plugin.StateDisabled: "Disabled",
		plugin.StateFaulted:  "Stopped: it kept failing. Fix it and press Reload.",
		plugin.StateError:    "Can't run: see the problems below.",
	}[in.State]
	p.state.SetText(state)
	s := in.Stats
	text := fmt.Sprintf("%d calls · %d errors · %d timed out · %d events dropped", s.Delivered, s.Errors, s.Timeouts, s.Dropped)
	if s.Dropped > 0 {
		text += " (it couldn't keep up)"
	}
	p.stats.SetText(text)
	if len(in.Log) != p.logShown {
		p.logShown = len(in.Log)
		var b strings.Builder
		for _, l := range in.Log {
			fmt.Fprintf(&b, "%s %-5s %s\n", l.Time.Format(time.TimeOnly), strings.ToLower(l.Level.String()), l.Msg)
		}
		p.logView.setText(b.String())
		sb := p.logView.edit.VerticalScrollBar()
		sb.SetValue(sb.Maximum())
	}
}

// hookSummary lists a plugin's targets: hook, function and filter.
func hookSummary(man *manifest.Manifest) string {
	if man == nil || len(man.Targets) == 0 {
		return "None."
	}
	var b strings.Builder
	for _, t := range man.Targets {
		fmt.Fprintf(&b, "<div><code>%s</code> → <code>%s</code>", htmlEscape(string(t.Hook)), htmlEscape(t.Function))
		if t.Self {
			b.WriteString(" <i>(sees its own traffic)</i>")
		}
		if t.Hold {
			b.WriteString(" <i>(holds the traffic it matches)</i>")
		}
		if f := matchSummary(t.Match); f != "" {
			fmt.Fprintf(&b, "<div style=\"margin-left:16px; color:gray\">%s</div>", htmlEscape(f))
		}
		b.WriteString("</div>")
	}
	return b.String()
}

// matchSummary describes a target's filter in a line: "in scope · 2xx ·
// text/html, application/json".
func matchSummary(m *manifest.Match) string {
	if m == nil {
		return ""
	}
	var parts []string
	add := func(label string, list []string) {
		if len(list) > 0 {
			parts = append(parts, label+strings.Join(list, ", "))
		}
	}
	switch m.Scope {
	case "in_scope":
		parts = append(parts, "in scope")
	case "out_of_scope":
		parts = append(parts, "out of scope")
	}
	add("targets ", m.Targets)
	add("", m.Methods)
	add("hosts ", m.Hosts)
	add("paths ", m.Paths)
	if m.PathRegex != "" {
		parts = append(parts, "path ~ "+m.PathRegex)
	}
	add("status ", m.Status)
	add("", m.ContentTypes)
	add("from ", m.Sources)
	add("types ", m.Types)
	return strings.Join(parts, " · ")
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- Settings form -------------------------------------------------------------

// buildSettingsForm lays out the selected plugin's settings, one editor per
// setting by type, each with a Reset to its default.
func (p *pluginsPage) buildSettingsForm() {
	if p.form != nil {
		p.form.Hide()
		p.form.DeleteLater()
		p.form = nil
	}
	in, ok := p.infos[p.selected]
	p.form = qt.NewQWidget2()
	p.settingsBox.AddWidget(p.form)
	if !ok || in.Manifest == nil || len(in.Manifest.Settings) == 0 {
		v := qt.NewQVBoxLayout(p.form)
		v.SetContentsMargins(0, 0, 0, 0)
		v.AddWidget(helpText("This plugin has no settings.").QWidget)
		return
	}
	form := newSettingsForm()
	p.form.SetLayout(form.QLayout)
	form.SetFieldGrowthPolicy(qt.QFormLayout__ExpandingFieldsGrow)
	for i := range in.Manifest.Settings {
		s := &in.Manifest.Settings[i]
		label := s.Label
		if label == "" {
			label = s.Key
		}
		field := p.settingField(in, s)
		field.SetToolTip(s.Description)
		form.AddRow3(label, field)
	}
}

// value is a setting's value as the dialog stands: the user's change, or
// the default.
func (p *pluginsPage) value(s *manifest.Setting) json.RawMessage {
	if d := p.drafts[p.selected]; d != nil {
		if v, ok := d.settings[s.Key]; ok {
			return v
		}
	}
	if len(s.Default) > 0 {
		return s.Default
	}
	return s.Zero()
}

// setValue records a change; a value equal to the default is no change.
func (p *pluginsPage) setValue(s *manifest.Setting, v json.RawMessage) {
	if p.loading {
		return
	}
	d := p.draft(p.selected)
	def := s.Default
	if len(def) == 0 {
		def = s.Zero()
	}
	if jsonEqual(v, def) {
		delete(d.settings, s.Key)
		return
	}
	d.settings[s.Key] = v
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

func (p *pluginsPage) settingField(in plugin.Info, s *manifest.Setting) *qt.QWidget {
	row := qt.NewQWidget2()
	h := qt.NewQHBoxLayout(row)
	h.SetContentsMargins(0, 0, 0, 0)
	h.SetSpacing(6)
	var reset func()

	var cur any
	_ = json.Unmarshal(p.value(s), &cur)
	switch s.Type {
	case manifest.TypeBoolean:
		box := qt.NewQCheckBox2()
		on, _ := cur.(bool)
		box.SetChecked(on)
		box.OnToggled(func(on bool) { b, _ := json.Marshal(on); p.setValue(s, b) })
		h.AddWidget(box.QWidget)
		h.AddStretch()
		reset = func() { var d bool; _ = json.Unmarshal(defaultOf(s), &d); box.SetChecked(d) }
	case manifest.TypeNumber:
		spin := qt.NewQDoubleSpinBox2()
		spin.SetRange(-1e9, 1e9)
		spin.SetDecimals(2)
		n, _ := cur.(float64)
		spin.SetValue(n)
		spin.OnValueChanged(func(v float64) { b, _ := json.Marshal(v); p.setValue(s, b) })
		h.AddWidget(spin.QWidget)
		h.AddStretch()
		reset = func() { var d float64; _ = json.Unmarshal(defaultOf(s), &d); spin.SetValue(d) }
	case manifest.TypeEnum:
		combo := qt.NewQComboBox2()
		for _, o := range s.Options {
			combo.AddItem(o)
		}
		str, _ := cur.(string)
		combo.SetCurrentText(str)
		combo.OnCurrentTextChanged(func(t string) { b, _ := json.Marshal(t); p.setValue(s, b) })
		h.AddWidget(combo.QWidget)
		h.AddStretch()
		reset = func() { var d string; _ = json.Unmarshal(defaultOf(s), &d); combo.SetCurrentText(d) }
	case manifest.TypeList:
		edit := qt.NewQLineEdit2()
		var items []string
		if list, ok := cur.([]any); ok {
			for _, x := range list {
				if str, ok := x.(string); ok {
					items = append(items, str)
				}
			}
		}
		edit.SetText(strings.Join(items, ", "))
		edit.SetPlaceholderText("comma-separated")
		edit.OnTextEdited(func(t string) {
			out := []string{}
			for _, part := range strings.Split(t, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
			b, _ := json.Marshal(out)
			p.setValue(s, b)
		})
		h.AddWidget2(edit.QWidget, 1)
		reset = func() {
			var d []string
			_ = json.Unmarshal(defaultOf(s), &d)
			edit.SetText(strings.Join(d, ", "))
			p.setValue(s, defaultOf(s))
		}
	case manifest.TypeSecret:
		edit := qt.NewQLineEdit2()
		edit.SetEchoMode(qt.QLineEdit__Password)
		name := p.selected
		if pending, ok := p.secrets[name][s.Key]; ok {
			edit.SetText(pending)
		}
		if in.SecretSet[s.Key] {
			edit.SetPlaceholderText("set — type to replace")
		} else {
			edit.SetPlaceholderText("not set")
		}
		edit.OnTextEdited(func(t string) {
			if p.secrets[name] == nil {
				p.secrets[name] = map[string]string{}
			}
			p.secrets[name][s.Key] = t
		})
		h.AddWidget2(edit.QWidget, 1)
		clear := qt.NewQPushButton3("Clear")
		clear.SetAutoDefault(false)
		clear.SetToolTip("Remove the saved value")
		clear.OnClicked(func() {
			edit.SetText("")
			edit.SetPlaceholderText("cleared — removed when you apply")
			if p.secrets[name] == nil {
				p.secrets[name] = map[string]string{}
			}
			p.secrets[name][s.Key] = ""
		})
		h.AddWidget(clear.QWidget)
		return row // a secret has no default to reset to
	default: // string
		edit := qt.NewQLineEdit2()
		str, _ := cur.(string)
		edit.SetText(str)
		edit.OnTextEdited(func(t string) { b, _ := json.Marshal(t); p.setValue(s, b) })
		h.AddWidget2(edit.QWidget, 1)
		reset = func() {
			var d string
			_ = json.Unmarshal(defaultOf(s), &d)
			edit.SetText(d)
			p.setValue(s, defaultOf(s))
		}
	}
	resetBtn := qt.NewQToolButton2()
	resetBtn.SetText("Reset")
	resetBtn.SetToolTip("Back to the plugin's default")
	resetBtn.OnClicked(reset)
	h.AddWidget(resetBtn.QWidget)
	return row
}

func defaultOf(s *manifest.Setting) json.RawMessage {
	if len(s.Default) > 0 {
		return s.Default
	}
	return s.Zero()
}

// ---- Repositories & browsing ------------------------------------------------

// manifestDisabled is manifest.Disabled, for the draft status.
const manifestDisabled = manifest.Disabled

// registryParse is registry.ParseGitHub.
func registryParse(url string) (string, string, bool) { return registry.ParseGitHub(url) }

func (p *pluginsPage) refreshRepos() {
	for p.repoList.Count() > 0 {
		p.repoList.TakeItem(0)
	}
	for _, r := range p.repos {
		p.repoList.AddItem(r)
	}
}

func (p *pluginsPage) addRepository() {
	text := qt.QInputDialog_GetText(p.repoList.QWidget, "Add a plugin repository",
		"GitHub repository URL (https://github.com/owner/repo):")
	url := strings.TrimSpace(text)
	if url == "" {
		return
	}
	if _, _, ok := registryParse(url); !ok {
		qt.QMessageBox_Warning(p.repoList.QWidget, "Not a repository",
			url+" is not a GitHub repository URL (https://github.com/owner/repo).")
		return
	}
	for _, r := range p.repos {
		if r == url {
			return // already listed
		}
	}
	p.repos = append(p.repos, url)
	p.refreshRepos()
	p.repoList.SetCurrentRow(len(p.repos) - 1)
}

func (p *pluginsPage) removeRepository() {
	row := p.repoList.CurrentRow()
	if row < 0 || row >= len(p.repos) {
		return
	}
	p.repos = append(p.repos[:row], p.repos[row+1:]...)
	p.refreshRepos()
}

// browse opens the window listing the plugins the repositories offer. Any
// installed from it are recorded disabled, so the page adds a disabled draft
// for each (and reloads) when the window closes, and the change is saved with
// the Plugins window. Plugins updated from it are no longer offered as
// updates.
func (p *pluginsPage) browse() {
	installed, updated := runPluginBrowser(p.list.QWidget, p.ctrl, append([]string(nil), p.repos...))
	for _, name := range installed {
		if p.drafts[name] == nil {
			p.drafts[name] = &pluginDraft{status: manifestDisabled, settings: map[string]json.RawMessage{}}
		} else {
			p.drafts[name].status = manifestDisabled
		}
	}
	for _, name := range updated {
		delete(p.updates, name)
	}
	if len(installed) > 0 {
		p.selected = installed[len(installed)-1]
	}
	if len(installed)+len(updated) > 0 {
		p.reloadInfos()
	}
}

// ---- Updates -------------------------------------------------------------------

// checkUpdates asks the repositories, off the UI thread, for newer versions
// of the installed plugins, and badges those that have one. A repository
// that can't be reached just offers no updates: the browse window is where
// its error is shown.
func (p *pluginsPage) checkUpdates() {
	if p.checking || len(p.repos) == 0 {
		return
	}
	p.checking = true
	repos := append([]string(nil), p.repos...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		avail, _ := p.ctrl.AvailablePlugins(ctx, repos)
		mainthread.Start(func() {
			p.checking = false
			p.updates = map[string]registry.Plugin{}
			for _, a := range avail {
				if a.Update {
					p.updates[a.Name] = a
				}
			}
			p.refreshRows()
			p.showUpdate()
		})
	}()
}

// showUpdate shows the selected plugin's update, if it has one.
func (p *pluginsPage) showUpdate() {
	u, ok := p.updates[p.selected]
	p.updateBar.SetVisible(ok)
	if !ok {
		return
	}
	text := fmt.Sprintf("Version %s is available from %s. You have %s.", u.Version, repoShortName(u.Repo), u.InstalledVersion)
	if u.InstalledByHand {
		// The copy in the folder is the user's own: say whose files go.
		text += " This copy wasn't installed from a repository, so updating replaces your own files."
	}
	if p.infos[p.selected].Status == manifest.Enabled {
		text += " It stays enabled, and restarts on the new version."
	}
	p.updateText.SetText(text)
	p.updateBtn.SetEnabled(true)
	p.updateBtn.SetText("Update")
}

// update installs the newer version of a plugin, off the UI thread, once
// the user confirms it: an update is new code, so it is never applied
// without them.
func (p *pluginsPage) update(name string) {
	u, ok := p.updates[name]
	if !ok {
		return
	}
	msg := fmt.Sprintf("Update %s from %s to %s, downloaded from %s?", name, u.InstalledVersion, u.Version, repoShortName(u.Repo))
	if u.InstalledByHand {
		msg += "\n\nThis copy was NOT installed from a repository: you put it in the plugins " +
			"folder yourself. Updating replaces those files with the repository's, and anything " +
			"you changed in them is lost."
	}
	if p.infos[name].Status == manifest.Enabled {
		msg += "\n\nThe plugin is enabled: it will restart on the new version, with its settings as they are."
	}
	if qt.QMessageBox_Question(p.list.QWidget, "Update "+name, msg) != qt.QMessageBox__Yes {
		return
	}
	p.updateBtn.SetEnabled(false)
	p.updateBtn.SetText("Updating…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, err := p.ctrl.UpdatePlugin(ctx, u)
		mainthread.Start(func() {
			if err != nil {
				p.showUpdate()
				qt.QMessageBox_Warning(p.list.QWidget, "Couldn't update "+name, err.Error())
				return
			}
			delete(p.updates, name)
			p.reloadInfos()
		})
	}()
}
