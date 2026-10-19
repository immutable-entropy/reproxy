package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"reproxy/internal/plugin/registry"
)

// runPluginBrowser opens the window listing the plugins the given
// repositories offer, each with an Add button that downloads it into the
// plugins directory (disabled, for the user to review and enable), or an
// Update to button for an installed plugin the repository has a newer
// version of. It blocks until the window is closed and returns the names
// installed and the names updated.
//
// Fetching and downloading touch the network, so they run on a goroutine and
// hand their results back to the UI thread; the window shows what it is doing
// meanwhile.
func runPluginBrowser(parent *qt.QWidget, ctrl Controller, repos []string) (installed, updated []string) {
	b := &pluginBrowser{ctrl: ctrl, repos: repos, installed: map[string]bool{}, updated: map[string]bool{}}
	b.run(parent)
	return sortedNames(b.installed), sortedNames(b.updated)
}

type pluginBrowser struct {
	ctrl  Controller
	repos []string

	dlg     *qt.QDialog
	status  *qt.QLabel
	list    *qt.QWidget
	listBox *qt.QVBoxLayout
	refresh *qt.QPushButton

	available []registry.Plugin
	installed map[string]bool
	updated   map[string]bool
	loading   bool
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (b *pluginBrowser) run(parent *qt.QWidget) {
	b.dlg = qt.NewQDialog(parent)
	b.dlg.SetWindowTitle("Add Plugins")
	b.dlg.Resize(720, 520)
	b.dlg.SetMinimumSize2(560, 380)

	root := qt.NewQVBoxLayout(b.dlg.QWidget)
	root.SetContentsMargins(0, 0, 0, 0)
	root.SetSpacing(0)

	head := qt.NewQWidget2()
	setName(head.QObject, "PanelHeader")
	head.SetAttribute(qt.WA_StyledBackground)
	hl := qt.NewQVBoxLayout(head)
	hl.SetContentsMargins(20, 14, 20, 12)
	hl.SetSpacing(4)
	title := qt.NewQLabel3("Add plugins from a repository")
	setName(title.QObject, "SettingsPageTitle")
	hl.AddWidget(title.QWidget)
	warn := qt.NewQLabel3("Plugins run code on your computer. Add only plugins from repositories you trust; " +
		"an added plugin arrives disabled, so you can review it before enabling it.")
	setName(warn.QObject, "Muted")
	warn.SetWordWrap(true)
	hl.AddWidget(warn.QWidget)
	root.AddWidget(head)

	b.status = qt.NewQLabel3("")
	setName(b.status.QObject, "Muted")
	b.status.SetWordWrap(true)
	b.status.SetContentsMargins(20, 8, 20, 0)
	root.AddWidget(b.status.QWidget)

	// A scroll area holding one card per available plugin.
	b.list = qt.NewQWidget2()
	b.listBox = qt.NewQVBoxLayout(b.list)
	b.listBox.SetContentsMargins(16, 8, 16, 8)
	b.listBox.SetSpacing(8)
	b.listBox.AddStretch()
	scroll := qt.NewQScrollArea2()
	scroll.SetWidgetResizable(true)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	scroll.SetWidget(b.list)
	root.AddWidget2(scroll.QWidget, 1)

	footer := qt.NewQWidget2()
	setName(footer.QObject, "SettingsFooter")
	footer.SetAttribute(qt.WA_StyledBackground)
	fl := qt.NewQHBoxLayout(footer)
	fl.SetContentsMargins(16, 10, 16, 10)
	b.refresh = qt.NewQPushButton3("Refresh")
	b.refresh.SetAutoDefault(false)
	b.refresh.OnClicked(b.load)
	closeBtn := qt.NewQPushButton3("Close")
	closeBtn.SetDefault(true)
	setName(closeBtn.QObject, "PrimaryButton")
	closeBtn.OnClicked(b.dlg.Accept)
	fl.AddWidget(b.refresh.QWidget)
	fl.AddStretch()
	fl.AddWidget(closeBtn.QWidget)
	root.AddWidget(footer)

	b.load()
	b.dlg.Exec()
	b.dlg.DeleteLater()
}

// load fetches the available plugins from the repositories, off the UI
// thread.
func (b *pluginBrowser) load() {
	if b.loading {
		return
	}
	if len(b.repos) == 0 {
		b.status.SetText("No repositories configured. Add one in the Repositories section.")
		return
	}
	b.loading = true
	b.refresh.SetEnabled(false)
	b.status.SetText("Loading plugins from " + plural(len(b.repos), "repository", "repositories") + "…")
	repos := append([]string(nil), b.repos...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		plugins, errs := b.ctrl.AvailablePlugins(ctx, repos)
		mainthread.Start(func() {
			b.loading = false
			b.refresh.SetEnabled(true)
			b.available = plugins
			b.render(errs)
		})
	}()
}

// render rebuilds the list of plugin cards.
func (b *pluginBrowser) render(errs []error) {
	// Clear existing cards (everything but the trailing stretch).
	for b.listBox.Count() > 1 {
		item := b.listBox.TakeAt(0)
		if w := item.Widget(); w != nil {
			w.DeleteLater()
		}
	}
	msgs := []string{}
	if len(b.available) > 0 {
		msgs = append(msgs, fmt.Sprintf("%d plugin%s available.", len(b.available), pluralS(len(b.available))))
	} else if len(errs) == 0 {
		msgs = append(msgs, "No plugins found in the configured repositories.")
	}
	for _, err := range errs {
		msgs = append(msgs, err.Error())
	}
	b.status.SetText(strings.Join(msgs, "\n"))

	for i := range b.available {
		b.listBox.InsertWidget(b.listBox.Count()-1, b.card(b.available[i]))
	}
}

// card is one available plugin: its name, version, source and description,
// with an Add button (or an Installed/Added label).
func (b *pluginBrowser) card(p registry.Plugin) *qt.QWidget {
	frame := qt.NewQFrame2()
	setName(frame.QObject, "Panel")
	frame.SetFrameShape(qt.QFrame__StyledPanel)
	h := qt.NewQHBoxLayout(frame.QWidget)
	h.SetContentsMargins(12, 10, 12, 10)
	h.SetSpacing(12)

	text := qt.NewQVBoxLayout2()
	text.SetSpacing(2)
	heading := p.Name
	if p.Version != "" {
		heading += "  " + p.Version
	}
	name := qt.NewQLabel3(heading)
	setName(name.QObject, "SettingsSection")
	text.AddWidget(name.QWidget)
	if p.Description != "" {
		desc := qt.NewQLabel3(p.Description)
		desc.SetWordWrap(true)
		text.AddWidget(desc.QWidget)
	}
	source := repoShortName(p.Repo)
	if p.Author != "" {
		source += "  ·  by " + p.Author
	}
	src := qt.NewQLabel3(source)
	setName(src.QObject, "Muted")
	text.AddWidget(src.QWidget)
	h.AddLayout2(text.QLayout, 1)

	installed := p.Installed || b.installed[p.Name]
	btn := qt.NewQPushButton3("Add")
	btn.SetAutoDefault(false)
	if p.Update && !b.updated[p.Name] {
		btn.SetText("Update to " + p.Version)
		btn.SetToolTip(p.InstalledVersion + " is installed")
		plugin := p
		btn.OnClicked(func() { b.update(plugin, btn) })
	} else if installed {
		btn.SetText("Installed")
		btn.SetEnabled(false)
	} else {
		plugin := p
		btn.OnClicked(func() { b.install(plugin, btn) })
	}
	h.AddWidget(btn.QWidget)
	return frame.QWidget
}

// install downloads a plugin, off the UI thread, and marks it installed.
func (b *pluginBrowser) install(p registry.Plugin, btn *qt.QPushButton) {
	btn.SetEnabled(false)
	btn.SetText("Adding…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		name, err := b.ctrl.InstallPlugin(ctx, p)
		mainthread.Start(func() {
			if err != nil {
				btn.SetEnabled(true)
				btn.SetText("Add")
				qt.QMessageBox_Warning(b.dlg.QWidget, "Couldn't add "+p.Name, err.Error())
				return
			}
			b.installed[name] = true
			btn.SetText("Added")
			b.status.SetText(name + " added, disabled. Enable it in the Plugins section once you've reviewed it.")
		})
	}()
}

// update replaces an installed plugin with this newer version, off the UI
// thread. Clicking Update to is the user's go-ahead, so it isn't asked again.
func (b *pluginBrowser) update(p registry.Plugin, btn *qt.QPushButton) {
	btn.SetEnabled(false)
	btn.SetText("Updating…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, err := b.ctrl.UpdatePlugin(ctx, p)
		mainthread.Start(func() {
			if err != nil {
				btn.SetEnabled(true)
				btn.SetText("Update to " + p.Version)
				qt.QMessageBox_Warning(b.dlg.QWidget, "Couldn't update "+p.Name, err.Error())
				return
			}
			b.updated[p.Name] = true
			btn.SetText("Updated")
			b.status.SetText(p.Name + " updated to " + p.Version + ".")
		})
	}()
}

func repoShortName(repoURL string) string {
	if owner, repo, ok := registry.ParseGitHub(repoURL); ok {
		return owner + "/" + repo
	}
	return repoURL
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
