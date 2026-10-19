package target

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// mustSet compiles targets for a test, failing it if they don't.
func mustSet(t *testing.T, list ...Target) *Set {
	t.Helper()
	for i := range list {
		list[i] = list[i].Normalized()
		if err := list[i].Validate(); err != nil {
			t.Fatalf("target %d: %v", i, err)
		}
	}
	s, err := Compile(list)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// target builds one named target with extra rules above the default.
func target(name string, kind Kind, value string, rules ...Rule) Target {
	t := New(kind, value, Palette[0])
	t.Name = name
	t.Rules = append(rules, DefaultRule())
	return t
}

func TestTargetKinds(t *testing.T) {
	set := mustSet(t,
		target("Site", KindDomain, "example.test"),
		target("Staging", KindWildcard, "*.staging.test"),
		target("APIs", KindRegex, `api\d+\.corp\.test`),
		target("Host", KindIP, "198.51.100.7"),
		target("Lab", KindCIDR, "10.0.0.0/8"),
	)

	cases := []struct {
		host string
		ip   string
		want bool
	}{
		{"example.test", "", true},
		{"EXAMPLE.TEST:8443", "", true},    // case and port ignored
		{"notexample.test", "", false},     // exact means exact
		{"app.staging.test", "", true},     // wildcard
		{"staging.test", "", false},        // *. needs a label
		{"api7.corp.test", "", true},       // regex
		{"api7.corp.test.evil", "", false}, /* anchored */ // lookalike kept out
		{"198.51.100.7", "", true},         // IP as the host
		{"host.test", "10.1.2.3", true},    // CIDR via the connection
		{"host.test", "192.0.2.1", false},
		{"host.test", "", false},
	}
	for _, c := range cases {
		ip, _ := netip.ParseAddr(c.ip)
		if _, ok := set.Match(c.host, ip, "/"); ok != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.host, c.ip, ok, c.want)
		}
	}
}

// A domain left on the Domain kind but typed with glob characters is matched
// as a wildcard, so a "raw" *.example.com works without switching the kind.
func TestDomainWithGlobMatchesAsWildcard(t *testing.T) {
	set := mustSet(t,
		target("", KindDomain, "*.google.test"),   // raw wildcard on the Domain kind
		target("", KindDomain, "host?.corp.test"), // ? matches a single character
	)

	cases := []struct {
		host string
		want bool
	}{
		{"www.google.test", true},       // glob on a Domain entry now matches
		{"a.b.google.test", true},       // * crosses labels
		{"google.test", false},          // *. still needs a leading label
		{"www.google.test.evil", false}, // anchored: no lookalike
		{"host1.corp.test", true},       // ? matches one character
		{"host12.corp.test", false},     // ? is exactly one character
	}
	for _, c := range cases {
		if _, ok := set.Match(c.host, netip.Addr{}, "/"); ok != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.host, ok, c.want)
		}
	}
}

func TestRulesAreOrderedLikeFirewallRules(t *testing.T) {
	set := mustSet(t, target("Site", KindDomain, "example.test",
		Rule{Deny, `^/admin`},
		Rule{Allow, `^/admin/reports`}, // below the deny, so never reached
	))

	cases := []struct {
		path string
		want bool
	}{
		{"/", true},                 // the default rule allows the rest
		{"/orders/12?full=1", true}, // query is part of what rules see
		{"/admin", false},
		{"/admin/users", false},
		{"/admin/reports", false}, // the deny above wins
		{"/ADMIN", false},         // rules ignore case
	}
	for _, c := range cases {
		if _, ok := set.Match("example.test", netip.Addr{}, c.path); ok != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.path, ok, c.want)
		}
	}
}

func TestFirstTargetCoveringTheHostOwnsIt(t *testing.T) {
	first := target("First", KindWildcard, "*.example.test", Rule{Deny, `^/health`})
	second := target("Second", KindDomain, "api.example.test")
	set := mustSet(t, first, second)

	m, ok := set.Match("api.example.test", netip.Addr{}, "/orders")
	if !ok || m.Name != "First" {
		t.Fatalf("Match = %+v, %v; want First", m, ok)
	}
	// A deny in the owning target is final: Second does not rescue it.
	if _, ok := set.Match("api.example.test", netip.Addr{}, "/health"); ok {
		t.Error("a denied request was claimed by a later target")
	}
	// A host the first doesn't cover falls through to the second.
	only := mustSet(t, target("Other", KindDomain, "other.test"), second)
	if m, ok := only.Match("api.example.test", netip.Addr{}, "/"); !ok || m.Name != "Second" {
		t.Fatalf("Match = %+v, %v; want Second", m, ok)
	}
}

func TestDisabledAndEmptyMatchNothing(t *testing.T) {
	if !(&Set{}).Empty() {
		t.Error("the empty set should report Empty")
	}
	if _, ok := (*Set)(nil).Match("example.test", netip.Addr{}, "/"); ok {
		t.Error("a nil set matched")
	}
	off := target("Parked", KindDomain, "example.test")
	off.Enabled = false
	if _, ok := mustSet(t, off).Match("example.test", netip.Addr{}, "/"); ok {
		t.Error("a disabled target matched")
	}
}

// An unnamed target is labelled with its address, and Match carries that
// label so the UI can mark a flow either way.
func TestLabelFallsBackToTheAddress(t *testing.T) {
	named := target("Acme staging", KindDomain, "staging.acme.test")
	if named.Label() != "Acme staging" {
		t.Errorf("Label = %q", named.Label())
	}
	bare := target("", KindCIDR, "10.0.0.0/8")
	if bare.Label() != "10.0.0.0/8" {
		t.Errorf("Label = %q, want the address", bare.Label())
	}
	if bare.Display() != "10.0.0.0/8" {
		t.Errorf("Display = %q", bare.Display())
	}
	if wild := target("", KindWildcard, "*.acme.test"); wild.Display() != "wildcard:*.acme.test" {
		t.Errorf("Display = %q", wild.Display())
	}
	m, ok := mustSet(t, bare).Match("host.test", netip.MustParseAddr("10.1.2.3"), "/")
	if !ok || m.Name != "10.0.0.0/8" {
		t.Errorf("Match = %+v, %v", m, ok)
	}
}

func TestDetectKind(t *testing.T) {
	cases := map[string]Kind{
		"example.test":     KindDomain,
		"":                 KindDomain,
		"*.example.test":   KindWildcard,
		"host?.test":       KindWildcard,
		"198.51.100.7":     KindIP,
		"::1":              KindIP,
		"10.0.0.0/8":       KindCIDR,
		"2001:db8::/32":    KindCIDR,
		"example.test/api": KindDomain, // a path isn't a CIDR range
	}
	for value, want := range cases {
		if got := DetectKind(value); got != want {
			t.Errorf("DetectKind(%q) = %q, want %q", value, got, want)
		}
	}
	// A value saved with no kind is detected on the way in.
	if got := (Target{Value: "10.0.0.0/8"}).Normalized().Kind; got != KindCIDR {
		t.Errorf("Normalized kind = %q", got)
	}
}

func TestNormalizeKeepsTheDefaultRuleLast(t *testing.T) {
	tg := New(KindDomain, " example.test ", Palette[0])
	tg.Name = " Scope "
	tg.Rules = []Rule{DefaultRule(), {Deny, " ^/admin "}, {Allow, "  "}}
	tg = tg.Normalized()

	if tg.Name != "Scope" || tg.Value != "example.test" {
		t.Errorf("Name = %q, Value = %q", tg.Name, tg.Value)
	}
	if len(tg.Rules) != 2 || tg.Rules[0].Pattern != "^/admin" || !tg.Rules[1].IsDefault() {
		t.Fatalf("Rules = %+v", tg.Rules)
	}
	// Removing every rule still leaves the undeletable one.
	tg.Rules = nil
	if r := tg.Normalized().Rules; len(r) != 1 || !r[0].IsDefault() {
		t.Errorf("Rules = %+v", r)
	}
}

func TestValidateRejectsBadValuesAndPatterns(t *testing.T) {
	bad := []Target{
		New(KindDomain, "", Palette[0]), // no address
		New(KindDomain, "x", "blue"),    // not a colour
		New(KindRegex, "(", Palette[0]), // unparseable host regex
		New(KindCIDR, "10.0.0.0", Palette[0]),
		New(KindIP, "not-an-ip", Palette[0]),
		New("soup", "x", Palette[0]),
		target("x", KindDomain, "example.test", Rule{Allow, "("}),
		target("x", KindDomain, "example.test", Rule{"maybe", "/x"}),
	}
	for i, tg := range bad {
		if err := tg.Normalized().Validate(); err == nil {
			t.Errorf("case %d: %+v was accepted", i, tg)
		}
	}
	// A nameless target is fine: the address is the label.
	if err := New(KindDomain, "example.test", Palette[0]).Normalized().Validate(); err != nil {
		t.Errorf("a nameless target was rejected: %v", err)
	}
}

func TestPickColorAvoidsThoseInUse(t *testing.T) {
	if c := PickColor(nil); c != Palette[0] {
		t.Errorf("PickColor(nil) = %s", c)
	}
	if c := PickColor([]string{Palette[0], Palette[1]}); c != Palette[2] {
		t.Errorf("PickColor = %s, want %s", c, Palette[2])
	}
	// Every colour taken: it cycles rather than returning nothing.
	if c := PickColor(Palette); c == "" {
		t.Error("PickColor returned no colour")
	}
}

func TestStoreRoundTripAndMatching(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Set().Empty() {
		t.Fatal("a fresh store should match nothing")
	}

	changes := 0
	defer s.Subscribe(func() { changes++ })()

	tg, err := s.Create("", "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Color != Palette[0] || !tg.Enabled || tg.Kind != KindDomain || len(tg.Rules) != 1 {
		t.Fatalf("Create = %+v", tg)
	}
	if m, ok := s.Set().Match("example.test", netip.Addr{}, "/"); !ok || m.ID != tg.ID {
		t.Fatalf("Match = %+v, %v", m, ok)
	}
	if next := s.NextColor(); next != Palette[1] {
		t.Errorf("NextColor = %s, want %s", next, Palette[1])
	}

	tg.Name = "Acme"
	if err := s.Save(tg); err != nil {
		t.Fatal(err)
	}
	if changes != 2 {
		t.Errorf("subscribers called %d times, want 2", changes)
	}

	// A second store on the same file sees the same targets.
	again, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if list := again.List(); len(list) != 1 || list[0].Name != "Acme" {
		t.Fatalf("reloaded %+v", list)
	}

	// An invalid save changes nothing.
	broken := tg
	broken.Rules = []Rule{{Allow, "("}, DefaultRule()}
	if err := s.Save(broken); err == nil {
		t.Error("an unparseable rule was saved")
	}
	if _, ok := s.Set().Match("example.test", netip.Addr{}, "/"); !ok {
		t.Error("a failed save disturbed the live set")
	}

	if err := s.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	if !s.Set().Empty() || len(s.List()) != 0 {
		t.Error("Remove left the target behind")
	}
}

// Open swaps the file the store works on, which is how opening an engagement
// brings its own targets in.
func TestStoreOpenSwapsFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a", "targets.json")
	second := filepath.Join(dir, "b", "targets.json")

	s, err := NewStore(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(KindDomain, "first.test"); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(second, []Target{New(KindDomain, "second.test", Palette[3])}); err != nil {
		t.Fatal(err)
	}

	changes := 0
	defer s.Subscribe(func() { changes++ })()
	if err := s.Open(second); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Errorf("Open notified %d times, want 1", changes)
	}
	if s.Path() != second {
		t.Errorf("Path = %q", s.Path())
	}
	if list := s.List(); len(list) != 1 || list[0].Value != "second.test" {
		t.Fatalf("after Open: %+v", list)
	}
	if _, ok := s.Set().Match("first.test", netip.Addr{}, "/"); ok {
		t.Error("the previous file's targets still match")
	}
	// Back again: the first file is untouched by the detour.
	if err := s.Open(first); err != nil {
		t.Fatal(err)
	}
	if list := s.List(); len(list) != 1 || list[0].Value != "first.test" {
		t.Fatalf("after reopening: %+v", list)
	}
}

func TestStoreMoveChangesPriority(t *testing.T) {
	s, _ := NewStore("")
	a, _ := s.Create(KindWildcard, "*.example.test")
	b, _ := s.Create(KindDomain, "api.example.test")
	a.Name, b.Name = "A", "B"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}

	if m, _ := s.Set().Match("api.example.test", netip.Addr{}, "/"); m.Name != "A" {
		t.Fatalf("first match = %q, want A", m.Name)
	}
	if err := s.Move(b.ID, -1); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Set().Match("api.example.test", netip.Addr{}, "/"); m.Name != "B" {
		t.Fatalf("after Move = %q, want B", m.Name)
	}
	if got := s.List()[0].Name; got != "B" {
		t.Errorf("List()[0] = %q", got)
	}
	// Past the end does nothing rather than erroring.
	if err := s.Move(b.ID, -5); err != nil {
		t.Fatal(err)
	}
	if got := s.List()[0].Name; got != "B" {
		t.Errorf("List()[0] = %q after a no-op move", got)
	}
}

func TestStoreRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := NewStore(path); err == nil {
		t.Fatal("a corrupt file was accepted")
	}
	os.WriteFile(path, []byte(`{"targets":[{"id":"x","kind":"domain","value":"x","color":"#3b6ef5",`+
		`"rules":[{"action":"allow","pattern":"("}]}]}`), 0o600)
	if _, err := NewStore(path); err == nil {
		t.Fatal("an unparseable rule was accepted")
	}
}

// A lookup belongs to the target covering its name, whatever that target's
// path rules say, or to one covering an address it resolved to.
func TestMatchHostIgnoresPathRules(t *testing.T) {
	site := target("Site", KindDomain, "app.example.test", Rule{Deny, `.*`})
	site.Intercept.DNS = true
	lan := target("LAN", KindCIDR, "10.0.0.0/24")
	set := mustSet(t, site, lan)

	if m, ok := set.MatchHost("App.Example.Test"); !ok || m.Name != "Site" || !m.Intercept.DNS {
		t.Fatalf("MatchHost = %+v, %v; want Site, with its DNS setting", m, ok)
	}
	if _, ok := set.MatchHost("db.internal"); ok {
		t.Fatal("an unknown name matched")
	}
	if m, ok := set.MatchHost("db.internal", netip.MustParseAddr("10.0.0.5")); !ok || m.Name != "LAN" {
		t.Fatalf("MatchHost by address = %+v, %v; want LAN", m, ok)
	}
}

func TestCreateAllAddsEveryValueOnceOrNothing(t *testing.T) {
	s, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	notified := 0
	s.Subscribe(func() { notified++ })
	added, err := s.CreateAll("", []string{"cdn.example", "203.0.113.7"})
	if err != nil || len(added) != 2 || notified != 1 {
		t.Fatalf("added %d, err %v, notified %d times; want 2, nil, once", len(added), err, notified)
	}
	if added[0].Color == added[1].Color {
		t.Error("two new targets were given the same colour")
	}
	if _, ok := s.Set().MatchHost("cdn.example"); !ok {
		t.Error("the new targets aren't in the match set")
	}
	if _, err := s.CreateAll("", []string{"ok.example", ""}); err == nil {
		t.Fatal("an invalid value was accepted")
	}
	if len(s.List()) != 2 {
		t.Errorf("%d targets after a refused batch, want 2", len(s.List()))
	}
}
