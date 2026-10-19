package ui

import (
	"encoding/json"
	"fmt"
	"reflect"

	qt "github.com/mappu/miqt/qt6"

	"reproxy/internal/config"
)

// engagementSettings is Settings → Engagement: the settings the open
// engagement overrides (config.EngagementSettings), kept in its folder
// rather than config.json. Each page starts from "Use the global setting",
// which shows the global value greyed out; clearing it makes the value the
// engagement's own. The suppression list is the exception: an
// engagement's entries are added to the global list.
//
// The pages save themselves (settingsPage.dirty/save) through
// Controller.ApplyEngagementSettings, since what they edit isn't part of
// config.Config. Both pages share this draft, so either one saving saves
// both, and the other then reports nothing left to save.
//
// With no engagement open the pages are listed greyed out.
type engagementSettings struct {
	ctrl Controller
	// baseline is the overrides as last saved; pwBaseline the passwords
	// of the engagement's own chain as last saved.
	baseline   config.EngagementSettings
	pwBaseline map[string]string
	// global is the user's settings, shown where nothing is overridden.
	global config.Config

	useGlobalChain *qt.QCheckBox
	chain          *upstreamEditor

	useGlobalRequired *qt.QCheckBox
	required          *qt.QCheckBox
	globalSuppressed  *qt.QLabel
	suppressed        *suppressionEditor
}

func newEngagementPages(ctrl Controller) []*settingsPage {
	current, name, open := ctrl.EngagementSettings()
	if !open {
		why := "Open an engagement (File → Open Engagement) to give it settings of its own"
		stub := func(title string) *settingsPage {
			return &settingsPage{
				title: title, group: settingsGroupEngagement, disabled: why,
				body: helpText(why + ".").QWidget,
				load: func(config.Config) {}, store: func(*config.Config) {},
			}
		}
		return []*settingsPage{stub(settingsOutbound), stub(settingsTargets)}
	}

	s := &engagementSettings{ctrl: ctrl, baseline: current}
	group := settingsGroupEngagement + " · " + name
	outbound := &settingsPage{
		title: settingsOutbound,
		group: group,
		description: "The upstream proxies this engagement's connections leave through, when they " +
			"aren't the global ones: a client's VPN, jump host or proxy.",
		body:  s.buildOutbound(),
		load:  s.load,
		store: func(*config.Config) {},
		dirty: s.dirty,
		save:  s.save,
	}
	targets := &settingsPage{
		title:       settingsTargets,
		group:       group,
		description: "Whether this engagement collects only traffic that belongs to a target, and the hosts it never suggests as related.",
		body:        s.buildTargets(),
		load:        s.load,
		store:       func(*config.Config) {},
		shown:       s.suppressed.showSelected,
		dirty:       s.dirty,
		save:        s.save,
	}
	return []*settingsPage{outbound, targets}
}

func (s *engagementSettings) buildOutbound() *qt.QWidget {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Upstream Proxy Configuration"))
	s.useGlobalChain = qt.NewQCheckBox3("Use the global upstream chain")
	v.AddWidget(s.useGlobalChain.QWidget)
	v.AddWidget(helpText("On, this engagement goes out the way Settings → Global → Outbound Traffic says. " +
		"Off, it has a chain of its own, starting as a copy of the global one, which is used while " +
		"the engagement is open; the footer shows which chain is in force. The chain is saved in the " +
		"engagement's folder, its passwords on this machine only.").QWidget)
	s.chain = newUpstreamEditor(s.ctrl)
	v.AddWidget(s.chain.root)

	s.useGlobalChain.OnToggled(func(global bool) {
		switch {
		case global:
			s.chain.load(s.global.Upstream)
		case s.baseline.Upstream != nil:
			s.chain.load(*s.baseline.Upstream)
		default:
			s.chain.loadCopy(s.global.Upstream)
		}
		s.chain.root.SetEnabled(!global)
	})
	return body
}

func (s *engagementSettings) buildTargets() *qt.QWidget {
	body := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(body)
	v.SetContentsMargins(0, 0, 0, 0)
	v.SetSpacing(10)

	v.AddWidget(sectionTitle("Scope"))
	s.useGlobalRequired = qt.NewQCheckBox3("Use the global setting")
	v.AddWidget(s.useGlobalRequired.QWidget)
	s.required = qt.NewQCheckBox3("Only collect traffic that belongs to a target")
	v.AddWidget(s.required.QWidget)
	v.AddWidget(requiredHelp().QWidget)
	s.useGlobalRequired.OnToggled(func(global bool) {
		if global {
			s.required.SetChecked(s.global.Targets.Required)
		}
		s.required.SetEnabled(!global)
	})

	v.AddSpacing(8)
	s.suppressed = newSuppressionEditor(s.ctrl)
	v.AddLayout(s.suppressed.header("Suppressed in This Engagement"))
	s.globalSuppressed = helpText("")
	v.AddWidget(s.globalSuppressed.QWidget)
	v.AddWidget2(s.suppressed.root, 1)
	return body
}

// load fills both pages: the overrides as saved, and the global settings
// where nothing is overridden. Each page's load calls it; it is the same
// either time.
func (s *engagementSettings) load(global config.Config) {
	s.global = global
	b := s.baseline

	s.useGlobalChain.BlockSignals(true)
	s.useGlobalChain.SetChecked(b.Upstream == nil)
	s.useGlobalChain.BlockSignals(false)
	s.pwBaseline = nil
	if b.Upstream != nil {
		s.chain.load(*b.Upstream)
		s.pwBaseline = s.chain.passwords()
	} else {
		s.chain.load(global.Upstream)
	}
	s.chain.root.SetEnabled(b.Upstream != nil)

	s.useGlobalRequired.BlockSignals(true)
	s.useGlobalRequired.SetChecked(b.TargetsRequired == nil)
	s.useGlobalRequired.BlockSignals(false)
	if b.TargetsRequired != nil {
		s.required.SetChecked(*b.TargetsRequired)
	} else {
		s.required.SetChecked(global.Targets.Required)
	}
	s.required.SetEnabled(b.TargetsRequired != nil)

	s.suppressed.load(b.Suppressed)
	s.globalSuppressed.SetText(fmt.Sprintf("Added to the %s suppressed everywhere "+
		"(Settings → Global → Targets), for this engagement only. The list is saved in the "+
		"engagement's folder, so it travels with it.", countOf(len(global.Targets.Suppressed), "entry", "entries")))
}

// draft is the overrides as the pages stand.
func (s *engagementSettings) draft() config.EngagementSettings {
	var d config.EngagementSettings
	if !s.useGlobalChain.IsChecked() {
		c := s.chain.chain()
		d.Upstream = &c
	}
	if !s.useGlobalRequired.IsChecked() {
		on := s.required.IsChecked()
		d.TargetsRequired = &on
	}
	d.Suppressed = s.suppressed.list()
	return d
}

// passwords is what saving does to the secrets file: the own chain's
// passwords, and "" for every proxy of the saved chain that is gone.
func (s *engagementSettings) passwords() map[string]string {
	out := map[string]string{}
	if s.baseline.Upstream != nil {
		for _, p := range s.baseline.Upstream.Proxies {
			out[p.ID] = ""
		}
	}
	if !s.useGlobalChain.IsChecked() {
		for id, pw := range s.chain.passwords() {
			out[id] = pw
		}
	}
	return out
}

func (s *engagementSettings) dirty() bool {
	if !sameJSON(s.draft(), s.baseline) {
		return true
	}
	return !s.useGlobalChain.IsChecked() && !reflect.DeepEqual(s.chain.passwords(), s.pwBaseline)
}

func (s *engagementSettings) save() error {
	if !s.dirty() {
		return nil // the other page saved both
	}
	d := s.draft()
	if err := s.ctrl.SaveUpstreamPasswords(s.passwords()); err != nil {
		return err
	}
	if err := s.ctrl.ApplyEngagementSettings(d); err != nil {
		return err
	}
	s.baseline = d
	s.pwBaseline = nil
	if d.Upstream != nil {
		s.pwBaseline = s.chain.passwords()
	}
	return nil
}

// sameJSON compares two values as they are saved, so a nil and an empty
// list (which save alike) are not a change.
func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
