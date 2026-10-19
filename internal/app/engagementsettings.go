package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/target"
)

// Settings come in two layers: the user's own (config.json, a.cfg) and the
// open engagement's overrides (its settings.json, a.eng). What is in force
// is a.cfg.Effective(a.eng), worked out again whenever either layer changes
// — on Apply in either part of Settings, and when an engagement is opened
// or closed — so the upstream chain, whether traffic must belong to a
// target, and the suppression list follow the engagement that is open.

// errNoEngagement is returned when engagement settings are saved with no
// engagement open to keep them in.
var errNoEngagement = errors.New("no engagement is open")

// effectiveLocked is the configuration in force. The caller holds a.mu.
func (a *App) effectiveLocked() config.Config { return a.cfg.Effective(a.eng) }

// Effective implements ui.Controller: the user's settings with the open
// engagement's overrides applied.
func (a *App) Effective() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.effectiveLocked()
}

// applyEffectiveLocked puts the settings in force into effect: the
// upstream dialer is rebuilt (a password may have changed even when the
// chain didn't), the entities store is told the suppression list when it
// changed, and the plugin dispatcher which entities' hooks run. The caller
// holds a.mu.
func (a *App) applyEffectiveLocked() {
	eff := a.effectiveLocked()
	a.rebuildUpstream(eff.Upstream)
	if !a.suppressionsApplied || !reflect.DeepEqual(eff.Targets.Suppressed, a.appliedSuppressed) {
		a.appliedSuppressed = eff.Targets.Suppressed
		a.suppressionsApplied = true
		a.entities.SetSuppressed(target.CompileSuppressions(eff.Targets.Suppressed).Suppressed)
	}
	if a.plugins != nil {
		a.plugins.Dispatcher().SetAllEntities(eff.Targets.AllEntityHooks)
	}
}

// followEngagementSettings reads the open engagement's overrides and puts
// them into effect. It is the engagement manager's subscriber, and runs
// once at startup. Overrides that can't be read are reported and ignored,
// so the engagement still opens, on the global settings.
func (a *App) followEngagementSettings() {
	var e config.EngagementSettings
	if eng, ok := a.engagements.Current(); ok {
		loaded, err := config.LoadEngagementSettings(eng.SettingsPath())
		if err != nil {
			a.log.Error("the engagement's own settings are ignored", "err", err)
		} else {
			e = loaded
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.eng = e
	a.applyEffectiveLocked()
}

// EngagementSettings implements ui.Controller: the open engagement's
// overrides and its name. ok is false when no engagement is open.
func (a *App) EngagementSettings() (settings config.EngagementSettings, name string, ok bool) {
	eng, open := a.engagements.Current()
	if !open {
		return config.EngagementSettings{}, "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.eng, eng.Label(), true
}

// ApplyEngagementSettings implements ui.Controller: it checks the overrides
// against the global settings they combine with, saves them in the open
// engagement's folder and puts them into effect. On error nothing changes.
func (a *App) ApplyEngagementSettings(e config.EngagementSettings) error {
	eng, ok := a.engagements.Current()
	if !ok {
		return errNoEngagement
	}
	if err := e.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// The combination must hold too: an engagement's chain can't point
	// back at the proxy's own listen address, for one.
	if err := a.cfg.Effective(e).Validate(); err != nil {
		return err
	}
	if err := config.SaveEngagementSettings(eng.SettingsPath(), e); err != nil {
		return fmt.Errorf("saving the engagement's settings: %w", err)
	}
	a.eng = e
	a.applyEffectiveLocked()
	return nil
}

// requiredLocked reports whether traffic has to belong to a target to be
// collected, the engagement's override included. The caller holds a.mu.
func (a *App) requiredLocked() bool {
	if a.eng.TargetsRequired != nil {
		return *a.eng.TargetsRequired
	}
	return a.cfg.Targets.Required
}

// SuppressHost adds one host to the user's suppression
// list (Settings → Global → Targets) or, with engagementOnly, the open
// engagement's (Settings → Engagement → Targets). Either takes it out of
// Related at once. A host already suppressed by the list it would join is
// left as it is.
func (a *App) SuppressHost(host string, engagementOnly bool) error {
	return a.SuppressHosts([]string{host}, engagementOnly)
}

// SuppressHosts implements ui.Controller: SuppressHost for many hosts at
// once, saved and applied once (each apply rescans the Explorer). Hosts
// the list already suppresses are left as they are.
func (a *App) SuppressHosts(hosts []string, engagementOnly bool) error {
	add := func(list []target.Suppression) ([]target.Suppression, bool) {
		out := append([]target.Suppression(nil), list...)
		changed := false
		for _, h := range hosts {
			h = entities.BareHost(strings.TrimSpace(h))
			if h == "" || target.CompileSuppressions(out).Suppressed(h) {
				continue
			}
			out = append(out, target.Suppression{Pattern: h, Note: "Suppressed from Related", Added: time.Now()})
			changed = true
		}
		return out, changed
	}
	if engagementOnly {
		e, _, ok := a.EngagementSettings()
		if !ok {
			return errNoEngagement
		}
		list, changed := add(e.Suppressed)
		if !changed {
			return nil
		}
		e.Suppressed = list
		return a.ApplyEngagementSettings(e)
	}
	next := a.Settings()
	list, changed := add(next.Targets.Suppressed)
	if !changed {
		return nil
	}
	next.Targets.Suppressed = list
	return a.ApplySettings(next)
}
