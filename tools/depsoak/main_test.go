package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/BurntSushi/toml": "github.com/!burnt!sushi/toml",
		"golang.org/x/net":           "golang.org/x/net",
		"v1.0.0-RC1":                 "v1.0.0-!r!c1",
	} {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadAllow(t *testing.T) {
	name := filepath.Join(t.TempDir(), "allow")
	os.WriteFile(name, []byte("# header\n\ngolang.org/x/net v0.60.0 fixes GO-2026-6617 # trailing\n"), 0o644)
	allow, err := readAllow(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := allow[modVer{"golang.org/x/net", "v0.60.0"}]; got != "fixes GO-2026-6617" {
		t.Errorf("reason = %q", got)
	}
	if len(allow) != 1 {
		t.Errorf("got %d entries, want 1", len(allow))
	}

	os.WriteFile(name, []byte("golang.org/x/net v0.60.0\n"), 0o644)
	if _, err := readAllow(name); err == nil {
		t.Error("a line with no reason was accepted")
	}

	if allow, err := readAllow(filepath.Join(t.TempDir(), "missing")); err != nil || len(allow) != 0 {
		t.Errorf("missing file: %v, %v", allow, err)
	}
}
