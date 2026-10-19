package app

import (
	"reflect"
	"testing"

	"reproxy/internal/entities"
	"reproxy/internal/target"
)

func TestEntitySeeds(t *testing.T) {
	got := entitySeeds([]target.Target{
		{ID: "a", Kind: target.KindDomain, Value: "App.Example.com"},
		{ID: "b", Kind: target.KindWildcard, Value: "*.example.org"},
		{ID: "c", Kind: target.KindIP, Value: "10.0.0.5"},
		{ID: "d", Kind: target.KindDomain, Value: "localhost"},
		{ID: "e", Kind: target.KindCIDR, Value: "10.0.0.0/8"},
		{ID: "f", Kind: target.KindRegex, Value: `.*\.test`},
		{ID: "g", Kind: target.KindWildcard, Value: "api-*.example.net"},
	})
	want := []entities.Seed{
		{Type: entities.TypeHost, Name: "app.example.com", TargetID: "a"},
		{Type: entities.TypeDomain, Name: "example.org", TargetID: "b"},
		{Type: entities.TypeHost, Name: "10.0.0.5", TargetID: "c"},
		{Type: entities.TypeHost, Name: "localhost", TargetID: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("seeds = %+v\nwant %+v", got, want)
	}
}

// Adding a target puts its entity in the Explorer at once; removing it
// lets go of the entity when nothing came of it.
func TestTargetsBecomeEntities(t *testing.T) {
	a, _ := newApp(t, freePort(t))
	tg, err := a.Targets().Create(target.KindDomain, "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, ok := a.Entities().Get("//shop.example.com")
	if !ok || !h.IsName() || h.Origin != entities.OriginTarget || h.TargetID != tg.ID || h.Parent != "example.com" {
		t.Fatalf("host = %+v, %v", h, ok)
	}
	if d, ok := a.Entities().Get("example.com"); !ok || d.Origin != entities.OriginTarget {
		t.Fatalf("domain = %+v, %v", d, ok)
	}
	if err := a.Targets().Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"//shop.example.com", "example.com"} {
		if _, ok := a.Entities().Get(key); ok {
			t.Errorf("the removed target's idle %s was kept", key)
		}
	}
}
