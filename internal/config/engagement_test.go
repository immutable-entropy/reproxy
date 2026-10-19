package config

import (
	"path/filepath"
	"reflect"
	"testing"

	"reproxy/internal/target"
	"reproxy/internal/upstream"
)

func TestEffectiveAppliesOverrides(t *testing.T) {
	c := Default()
	c.Targets.Suppressed = []target.Suppression{{Pattern: "a.test"}}
	c.Upstream = upstream.Chain{Enabled: true, Proxies: []upstream.Proxy{{ID: "g", Enabled: true, Kind: upstream.HTTP, Host: "global.test", Port: 8080}}}

	// Nothing overridden: the global settings stand.
	if got := c.Effective(EngagementSettings{}); !reflect.DeepEqual(got, c) {
		t.Errorf("empty overrides changed the config")
	}

	off := false
	own := upstream.Chain{}
	got := c.Effective(EngagementSettings{
		Upstream: &own, TargetsRequired: &off,
		Suppressed: []target.Suppression{{Pattern: "b.test"}, {Pattern: "A.test"}},
	})
	if got.Targets.Required || got.Upstream.Enabled || len(got.Upstream.Proxies) != 0 {
		t.Errorf("overrides not applied: %+v", got)
	}
	var patterns []string
	for _, s := range got.Targets.Suppressed {
		patterns = append(patterns, s.Pattern)
	}
	if !reflect.DeepEqual(patterns, []string{"a.test", "b.test"}) {
		t.Errorf("suppressed = %v, want the global list plus the engagement's", patterns)
	}
	if len(c.Targets.Suppressed) != 1 {
		t.Error("Effective changed the global list")
	}
}

func TestEngagementSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if e, err := LoadEngagementSettings(path); err != nil || e.Upstream != nil || e.TargetsRequired != nil {
		t.Fatalf("missing file: %+v, %v", e, err)
	}
	on := true
	want := EngagementSettings{TargetsRequired: &on, Suppressed: []target.Suppression{{Pattern: "*.cdn.test", Note: "n"}}}
	if err := SaveEngagementSettings(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEngagementSettings(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, %v; want %+v", got, err, want)
	}
	if err := SaveEngagementSettings(path, EngagementSettings{Suppressed: []target.Suppression{{Pattern: "10.0.0.0/99"}}}); err == nil {
		t.Error("an invalid suppression was saved")
	}
}
