package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"reproxy/internal/target"
	"reproxy/internal/upstream"
)

// EngagementSettings are the settings an engagement overrides: kept in the
// engagement's folder (package engagement says where), so they travel with
// it, and applied over the user's own settings while it is open.
//
// Each field left unset inherits the global setting; that is what the
// "Use the global setting" boxes in Settings → Engagement mean. The
// suppression list is the exception: an engagement's entries are added to
// the global list rather than replacing it.
//
// Passwords for an engagement's upstream proxies are not here: like the
// global chain's they are in the secrets file, so a copied engagement
// carries its proxies but not their credentials.
type EngagementSettings struct {
	// Upstream replaces the global chain of upstream proxies.
	Upstream *upstream.Chain `json:"upstream,omitempty"`
	// TargetsRequired replaces Targets.Required.
	TargetsRequired *bool `json:"targets_required,omitempty"`
	// Suppressed is added to Targets.Suppressed.
	Suppressed []target.Suppression `json:"suppressed,omitempty"`
}

// Validate reports whether e can be applied.
func (e EngagementSettings) Validate() error {
	if e.Upstream != nil {
		if err := e.Upstream.Validate(); err != nil {
			return fmt.Errorf("upstream proxy: %w", err)
		}
	}
	if err := target.ValidateSuppressions(e.Suppressed); err != nil {
		return fmt.Errorf("targets: %w", err)
	}
	return nil
}

// Effective is c with the engagement's overrides applied: the settings
// actually in force while the engagement is open.
func (c Config) Effective(e EngagementSettings) Config {
	if e.Upstream != nil {
		c.Upstream = *e.Upstream
	}
	if e.TargetsRequired != nil {
		c.Targets.Required = *e.TargetsRequired
	}
	if len(e.Suppressed) > 0 {
		merged := append([]target.Suppression(nil), c.Targets.Suppressed...)
		c.Targets.Suppressed, _ = target.MergeSuppressions(merged, e.Suppressed)
	}
	return c
}

// LoadEngagementSettings reads an engagement's settings file. A missing
// file means nothing is overridden.
func LoadEngagementSettings(path string) (EngagementSettings, error) {
	var e EngagementSettings
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return e, fmt.Errorf("reading engagement settings: %w", err)
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return EngagementSettings{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := e.Validate(); err != nil {
		return EngagementSettings{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// SaveEngagementSettings writes an engagement's settings file.
func SaveEngagementSettings(path string, e EngagementSettings) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return writeJSON(path, e)
}
