package target

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

// scope builds a Burp scope file around the entries given, as JSON.
func scope(t *testing.T, include, exclude string) *ScopeImport {
	t.Helper()
	body := `{"target":{"scope":{"advanced_mode":true,"include":[` + include +
		`],"exclude":[` + exclude + `]}}}`
	imp, err := ParseBurpScope(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseBurpScope: %v", err)
	}
	return imp
}

// entry writes one advanced-mode scope entry. The host and file are regular
// expressions full of backslashes, so the JSON is encoded rather than pasted.
func entry(host, file, port, protocol string) string {
	b, err := json.Marshal(map[string]any{
		"enabled": true, "host": host, "file": file, "port": port, "protocol": protocol,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// offEntry is entry with Burp's switch off.
func offEntry(host, file, port, protocol string) string {
	b, err := json.Marshal(map[string]any{
		"enabled": false, "host": host, "file": file, "port": port, "protocol": protocol,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// compile readies imported targets the way the store does, so a test can ask
// the compiled Set what is in scope.
func compile(t *testing.T, imp *ScopeImport) *Set {
	t.Helper()
	list := make([]Target, 0, len(imp.Targets))
	for i, got := range imp.Targets {
		got.ID, got.Color = NewID(), Palette[i%len(Palette)]
		got = got.Normalized()
		if err := got.Validate(); err != nil {
			t.Fatalf("target %q: %v", got.Value, err)
		}
		list = append(list, got)
	}
	set, err := Compile(list)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// inScope asks whether a request would be collected.
func inScope(set *Set, host, path string) bool {
	_, ok := set.Match(host, netip.Addr{}, path)
	return ok
}

// HackerOne writes an entry per scheme — http on 80 and https on 443 — and a
// target matches a host on any port, so the pair is one target.
func TestBurpScopeCollapsesTheSchemePair(t *testing.T) {
	imp := scope(t, strings.Join([]string{
		entry(`^example\.com$`, "^/.*", "^80$", "http"),
		entry(`^example\.com$`, "^/.*", "^443$", "https"),
		entry(`^api\.example\.com$`, "^/.*", "^443$", "https"),
	}, ","), "")
	if len(imp.Targets) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(imp.Targets), imp.Targets)
	}
	for _, got := range imp.Targets {
		if got.Kind != KindDomain {
			t.Errorf("%q: kind %q, want domain", got.Value, got.Kind)
		}
		if len(got.Rules) != 0 {
			t.Errorf("%q: got rules %+v, want none for a whole host", got.Value, got.Rules)
		}
	}
	if imp.Targets[0].Value != "example.com" || imp.Targets[1].Value != "api.example.com" {
		t.Errorf("values %q, %q", imp.Targets[0].Value, imp.Targets[1].Value)
	}
}

// A host pattern is read as the kind that says it best, and never as one
// that would cover more than the file did.
func TestBurpScopeHostPatterns(t *testing.T) {
	for _, c := range []struct {
		pattern string
		kind    Kind
		value   string
	}{
		{`^example\.com$`, KindDomain, "example.com"},
		{`^www\.example\.co\.uk$`, KindDomain, "www.example.co.uk"},
		{`^198\.51\.100\.7$`, KindIP, "198.51.100.7"},
		{`^.*\.example\.com$`, KindWildcard, "*.example.com"},
		{`^[^.]*\.example\.com$`, KindWildcard, "*.example.com"},
		// More than a domain or a wildcard: kept as the regex it is.
		{`^(dev|prod)\.example\.com$`, KindRegex, `(dev|prod)\.example\.com`},
		// Burp searches rather than anchoring, so an unanchored pattern must
		// stay as wide as it was once reproxy anchors it.
		{`example\.com`, KindRegex, `.*example\.com.*`},
		{`^example\.com`, KindRegex, `example\.com.*`},
		{`example\.com$`, KindRegex, `.*example\.com`},
	} {
		imp := scope(t, entry(c.pattern, "^/.*", "^443$", "https"), "")
		if len(imp.Targets) != 1 {
			t.Fatalf("%s: got %d targets", c.pattern, len(imp.Targets))
		}
		got := imp.Targets[0]
		if got.Kind != c.kind || got.Value != c.value {
			t.Errorf("%s -> %s %q, want %s %q", c.pattern, got.Kind, got.Value, c.kind, c.value)
		}
	}
}

// An unanchored pattern really does keep matching what Burp matched: the
// rewrite is checked against the compiled set, not just the string.
func TestBurpScopeUnanchoredPatternStillCoversSubdomains(t *testing.T) {
	set := compile(t, scope(t, entry(`example\.com`, "^/.*", "^443$", "https"), ""))
	for _, host := range []string{"example.com", "www.example.com", "a.b.example.com"} {
		if !inScope(set, host, "/") {
			t.Errorf("%s should be in scope", host)
		}
	}
}

// A file pattern narrows the target: the paths named are allowed and a Deny
// below them puts the rest out of scope, which is what the file meant.
func TestBurpScopeImportsPathRestrictions(t *testing.T) {
	imp := scope(t, strings.Join([]string{
		entry(`^example\.com$`, "^/api/.*", "^443$", "https"),
		entry(`^example\.com$`, "^/graphql", "^443$", "https"),
	}, ","), "")
	if len(imp.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(imp.Targets))
	}
	set := compile(t, imp)
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/api/orders", true},
		{"/graphql", true},
		{"/admin", false},
		{"/", false},
	} {
		if got := inScope(set, "example.com", c.path); got != c.want {
			t.Errorf("%s in scope = %v, want %v", c.path, got, c.want)
		}
	}
}

// An exclude for a host the includes name becomes a Deny above its allows.
func TestBurpScopeExcludeNarrowsAnIncludedHost(t *testing.T) {
	imp := scope(t,
		entry(`^example\.com$`, "^/.*", "^443$", "https"),
		entry(`^example\.com$`, "^/admin/.*", "^443$", "https"))
	if len(imp.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(imp.Targets), imp.Targets)
	}
	set := compile(t, imp)
	if !inScope(set, "example.com", "/orders") {
		t.Error("/orders should be in scope")
	}
	if inScope(set, "example.com", "/admin/users") {
		t.Error("/admin/users is excluded and should not be in scope")
	}
}

// An exclude for a host no include names becomes a target of its own that
// denies, ordered above the rest — which is how one host is taken back out
// of a wildcard.
func TestBurpScopeExcludeLiftsAHostOutOfAWildcard(t *testing.T) {
	imp := scope(t,
		entry(`^.*\.example\.com$`, "^/.*", "^443$", "https"),
		entry(`^secret\.example\.com$`, "^/.*", "^443$", "https"))
	set := compile(t, imp)
	if !inScope(set, "www.example.com", "/") {
		t.Error("www.example.com should be in scope")
	}
	if inScope(set, "secret.example.com", "/") {
		t.Error("secret.example.com is excluded and should not be in scope")
	}
}

// Excluding every path of an included host leaves nothing in scope, so no
// target is made and the report says why.
func TestBurpScopeWholeHostExclusionIsReported(t *testing.T) {
	imp := scope(t,
		strings.Join([]string{
			entry(`^example\.com$`, "^/.*", "^443$", "https"),
			entry(`^keep\.example\.com$`, "^/.*", "^443$", "https"),
		}, ","),
		entry(`^example\.com$`, "^/.*", "^443$", "https"))
	if len(imp.Targets) != 1 || imp.Targets[0].Value != "keep.example.com" {
		t.Fatalf("got %+v, want only keep.example.com", imp.Targets)
	}
	if len(imp.Skipped) != 1 || !strings.Contains(imp.Skipped[0].Reason, "excludes the whole host") {
		t.Errorf("skipped = %+v", imp.Skipped)
	}
}

// An entry Burp has switched off comes in switched off rather than being
// dropped, so the scope is all there to turn on.
func TestBurpScopeKeepsDisabledEntriesDisabled(t *testing.T) {
	imp := scope(t, strings.Join([]string{
		offEntry(`^off\.example\.com$`, "^/.*", "^443$", "https"),
		entry(`^on\.example\.com$`, "^/.*", "^443$", "https"),
	}, ","), "")
	if len(imp.Targets) != 2 {
		t.Fatalf("got %d targets", len(imp.Targets))
	}
	byValue := map[string]bool{}
	for _, got := range imp.Targets {
		byValue[got.Value] = got.Enabled
	}
	if byValue["off.example.com"] {
		t.Error("off.example.com should be disabled")
	}
	if !byValue["on.example.com"] {
		t.Error("on.example.com should be enabled")
	}
}

// Simple mode has no patterns: one prefix a URL has to start with.
func TestBurpScopeSimpleModePrefix(t *testing.T) {
	imp, err := ParseBurpScope(strings.NewReader(
		`{"target":{"scope":{"advanced_mode":false,"include":[` +
			`{"enabled":true,"prefix":"https://example.com/app/"},` +
			`{"enabled":true,"prefix":"https://10.0.0.5:8443/"}],"exclude":[]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Targets) != 2 {
		t.Fatalf("got %d targets: %+v", len(imp.Targets), imp.Targets)
	}
	if imp.Targets[0].Kind != KindDomain || imp.Targets[0].Value != "example.com" {
		t.Errorf("first = %s %q", imp.Targets[0].Kind, imp.Targets[0].Value)
	}
	// The path a prefix named can't be a pattern, so it is said in the notes
	// rather than quietly dropped or quietly enforced.
	if !strings.Contains(imp.Targets[0].Notes, "/app/") {
		t.Errorf("notes = %q, want the prefix path mentioned", imp.Targets[0].Notes)
	}
	// A port in a prefix is dropped, because a target matches every port.
	if imp.Targets[1].Kind != KindIP || imp.Targets[1].Value != "10.0.0.5" {
		t.Errorf("second = %s %q", imp.Targets[1].Kind, imp.Targets[1].Value)
	}
}

// A port that is not the usual 80/443 pair makes the target wider than the
// file was, so the import says so instead of being silently wrong.
func TestBurpScopeNotesAnUnusualPort(t *testing.T) {
	imp := scope(t, entry(`^example\.com$`, "^/.*", "^8443$", "https"), "")
	if len(imp.Targets) != 1 {
		t.Fatalf("got %d targets", len(imp.Targets))
	}
	if !strings.Contains(imp.Targets[0].Notes, "8443") {
		t.Errorf("notes = %q, want the port mentioned", imp.Targets[0].Notes)
	}
	imp = scope(t, entry(`^example\.com$`, "^/.*", "^443$", "https"), "")
	if n := imp.Targets[0].Notes; n != "" {
		t.Errorf("notes = %q, want none for the usual port", n)
	}
}

// The scope can sit at target.scope, at scope, or be the lists themselves,
// depending on whether the file came from the project, its options, or
// HackerOne.
func TestBurpScopeAcceptsTheNestingsItComesIn(t *testing.T) {
	for _, body := range []string{
		`{"target":{"scope":{"include":[` + entry(`^example\.com$`, "^/.*", "^443$", "https") + `]}}}`,
		`{"scope":{"include":[` + entry(`^example\.com$`, "^/.*", "^443$", "https") + `]}}`,
		`{"include":[` + entry(`^example\.com$`, "^/.*", "^443$", "https") + `]}`,
	} {
		imp, err := ParseBurpScope(strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if len(imp.Targets) != 1 || imp.Targets[0].Value != "example.com" {
			t.Errorf("%s -> %+v", body, imp.Targets)
		}
	}
}

// A file that isn't a scope is an error, not an empty import: a typo in the
// file picker shouldn't look like a scope with nothing in it.
func TestBurpScopeRejectsWhatIsNotAScopeFile(t *testing.T) {
	for _, body := range []string{
		`not json at all`,
		`{"proxy":{"request_listeners":[]}}`,
		`{"target":{"scope":{"advanced_mode":true,"include":[],"exclude":[]}}}`,
	} {
		if _, err := ParseBurpScope(strings.NewReader(body)); err == nil {
			t.Errorf("%s: expected an error", body)
		}
	}
}

// An entry with no host it can use is reported rather than dropped, so the
// count in the report always adds up.
func TestBurpScopeReportsEntriesItCannotUse(t *testing.T) {
	imp := scope(t, strings.Join([]string{
		entry(`^example\.com$`, "^/.*", "^443$", "https"),
		entry("", "^/.*", "^443$", "https"),
	}, ","), "")
	if len(imp.Targets) != 1 {
		t.Fatalf("got %d targets", len(imp.Targets))
	}
	if len(imp.Skipped) != 1 || !strings.Contains(imp.Skipped[0].Reason, "no host") {
		t.Errorf("skipped = %+v", imp.Skipped)
	}
}

// AddAll is the one write an import makes: every target gets an ID and a
// colour, a host already in the list is counted rather than added twice, and
// subscribers hear once however many came in. It also notifies without
// holding the lock, which an earlier version deadlocked on.
func TestAddAllImportsInOneWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(KindDomain, "example.com"); err != nil {
		t.Fatal(err)
	}
	notified := 0
	store.Subscribe(func() { notified++ })

	imp := scope(t, strings.Join([]string{
		entry(`^example\.com$`, "^/.*", "^443$", "https"), // already there
		entry(`^api\.example\.com$`, "^/.*", "^443$", "https"),
		entry(`^www\.example\.com$`, "^/.*", "^443$", "https"),
	}, ","), "")

	added, duplicates, err := store.AddAll(imp.Targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || duplicates != 1 {
		t.Fatalf("added %d, duplicates %d; want 2 and 1", len(added), duplicates)
	}
	if notified != 1 {
		t.Errorf("notified %d times, want 1", notified)
	}
	if n := len(store.List()); n != 3 {
		t.Errorf("store holds %d targets, want 3", n)
	}
	for _, got := range added {
		if got.ID == "" || got.Color == "" || got.Created.IsZero() {
			t.Errorf("%q: ID %q colour %q created %v", got.Value, got.ID, got.Color, got.Created)
		}
	}
	// The compiled set the proxy reads is live straight away.
	if _, ok := store.Set().MatchHost("api.example.com"); !ok {
		t.Error("api.example.com should be in the compiled set")
	}

	// Importing the same file again changes nothing.
	added, duplicates, err = store.AddAll(imp.Targets)
	if err != nil || len(added) != 0 || duplicates != 3 {
		t.Errorf("re-import: added %d, duplicates %d, err %v", len(added), duplicates, err)
	}
	if notified != 1 {
		t.Errorf("a no-op import notified; count now %d", notified)
	}

	// It survives a reload from the file it wrote.
	again, err := NewStore(filepath.Join(dir, "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.List()); n != 3 {
		t.Errorf("reloaded %d targets, want 3", n)
	}
}

// An invalid target writes nothing at all, so a bad file can't half-import.
func TestAddAllWritesNothingIfOneIsInvalid(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AddAll([]Target{
		{Kind: KindDomain, Value: "good.example.com", Enabled: true},
		{Kind: KindCIDR, Value: "not-a-range", Enabled: true},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("store holds %d targets, want none", n)
	}
}

// Adopt is how an imported target is made ready, in the wizard as well as in
// AddAll: it keeps what the import worked out and fills in what only a live
// target has.
func TestAdoptKeepsWhatTheImportDecided(t *testing.T) {
	imported := Target{
		Kind:  KindWildcard,
		Value: "*.example.com",
		Notes: "from the scope file",
		Rules: []Rule{{Action: Deny, Pattern: "^/admin"}},
		// A scope file can describe a target that is switched off.
		Enabled: false,
	}
	got := Adopt(imported, Palette[2])

	if got.ID == "" || got.Color != Palette[2] || got.Created.IsZero() {
		t.Errorf("identity not filled in: %+v", got)
	}
	if got.Kind != KindWildcard || got.Value != "*.example.com" {
		t.Errorf("address changed: %s %q", got.Kind, got.Value)
	}
	if got.Enabled {
		t.Error("an entry disabled in the file should come in disabled")
	}
	if got.Notes != "from the scope file" {
		t.Errorf("notes = %q", got.Notes)
	}
	// The rules are kept, with the undeletable default put back last.
	if len(got.Rules) != 2 || got.Rules[0].Action != Deny || !got.Rules[1].IsDefault() {
		t.Errorf("rules = %+v", got.Rules)
	}
	// A new target holds its requests, as one the user adds does.
	if !got.Intercept.Requests {
		t.Error("expected the same interception default New gives")
	}
	if err := got.Validate(); err != nil {
		t.Errorf("adopted target is invalid: %v", err)
	}

	// With no rules of its own it gets the default, and the kind is detected
	// from the value just as New does.
	plain := Adopt(Target{Value: "10.0.0.0/8", Enabled: true}, Palette[0])
	if plain.Kind != KindCIDR {
		t.Errorf("kind = %q, want cidr", plain.Kind)
	}
	if len(plain.Rules) != 1 || !plain.Rules[0].IsDefault() {
		t.Errorf("rules = %+v, want just the default", plain.Rules)
	}
}
