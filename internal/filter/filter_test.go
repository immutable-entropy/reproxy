package filter

import "testing"

func TestEmptyMatchesEverything(t *testing.T) {
	for _, text := range []string{"", "   ", "\t"} {
		m, err := Compile(text, false)
		if err != nil {
			t.Fatalf("Compile(%q): %v", text, err)
		}
		if !m.MatchesAll() {
			t.Errorf("Compile(%q): MatchesAll = false", text)
		}
		if !m.Match("anything at all") {
			t.Errorf("Compile(%q): did not match", text)
		}
	}
	// The nil matcher behaves like an empty query.
	var nilM *Matcher
	if !nilM.MatchesAll() || !nilM.Match("x") {
		t.Error("nil matcher should match everything")
	}
}

func TestPlainIsCaseInsensitiveSubstring(t *testing.T) {
	m, err := Compile("API.STAGE", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.MatchesAll() {
		t.Error("non-empty query reported as match-all")
	}
	subject := "GET https://api.stage.example.com/users 200 OK"
	if !m.Match(subject) {
		t.Errorf("plain filter should match %q case-insensitively", subject)
	}
	if m.Match("GET https://cdn.example.com/app.js 200 OK") {
		t.Error("plain filter matched an unrelated host")
	}
}

func TestRegexMode(t *testing.T) {
	m, err := Compile(`\b50\d\b`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("GET https://host/x 503 Service Unavailable") {
		t.Error("regex should match a 5xx status")
	}
	if m.Match("GET https://host/x 200 OK") {
		t.Error("regex should not match a 200")
	}
}

func TestRegexCaseSensitiveByDefault(t *testing.T) {
	sensitive, _ := Compile("GET", true)
	if sensitive.Match("get https://host/x 200 OK") {
		t.Error("regex mode should be case-sensitive by default")
	}
	insensitive, err := Compile("(?i)get", true)
	if err != nil {
		t.Fatal(err)
	}
	if !insensitive.Match("get https://host/x 200 OK") {
		t.Error("(?i) should make the regex case-insensitive")
	}
}

func TestInvalidRegexReturnsError(t *testing.T) {
	if _, err := Compile("a(b", true); err == nil {
		t.Fatal("expected a parse error for an unbalanced group")
	}
	// The same text is a perfectly valid plain substring.
	if _, err := Compile("a(b", false); err != nil {
		t.Errorf("plain mode should never error: %v", err)
	}
}
