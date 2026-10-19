package secrets

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestAPIKeysRoundTripAndRemove(t *testing.T) {
	s := New(t.TempDir())
	if k, err := s.APIKey("p1"); err != nil || k != "" {
		t.Fatalf("missing file: %q, %v", k, err)
	}
	if err := s.SetAPIKey("p1", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAPIKey("p2", "sk-2"); err != nil {
		t.Fatal(err)
	}
	// A second Store on the same directory (another process) sees them.
	other := New(strings.TrimSuffix(s.Path(), "/"+FileName))
	if k, _ := other.APIKey("p1"); k != "sk-1" {
		t.Fatalf("p1 = %q", k)
	}
	if err := s.SetAPIKey("p1", ""); err != nil {
		t.Fatal(err)
	}
	if k, _ := other.APIKey("p1"); k != "" {
		t.Fatalf("p1 after remove = %q", k)
	}
	if k, _ := other.APIKey("p2"); k != "sk-2" {
		t.Fatalf("p2 = %q", k)
	}
}

func TestMCPTokenIsStableUntilRegenerated(t *testing.T) {
	s := New(t.TempDir())
	a, err := s.MCPToken()
	if err != nil || !strings.HasPrefix(a, "rpx_") || len(a) < 40 {
		t.Fatalf("token %q, %v", a, err)
	}
	if b, _ := s.MCPToken(); b != a {
		t.Fatalf("token changed: %q then %q", a, b)
	}
	c, err := s.RegenerateMCPToken()
	if err != nil || c == a {
		t.Fatalf("regenerate = %q, %v", c, err)
	}
	if d, _ := s.MCPToken(); d != c {
		t.Fatalf("after regenerate got %q, want %q", d, c)
	}
}

func TestFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	s := New(t.TempDir())
	if err := s.SetAPIKey("p", "k"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o", perm)
	}
}

func TestProxyPasswords(t *testing.T) {
	s := New(t.TempDir())

	if pw, err := s.ProxyPassword("nope"); err != nil || pw != "" {
		t.Fatalf("ProxyPassword on an empty store = %q, %v", pw, err)
	}
	if err := s.SetProxyPasswords(map[string]string{"a": "one", "b": "two", "c": ""}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"a": "one", "b": "two", "c": ""} {
		if pw, err := s.ProxyPassword(id); err != nil || pw != want {
			t.Errorf("ProxyPassword(%q) = %q, %v; want %q", id, pw, err, want)
		}
	}

	// Saving again replaces the whole set, so a proxy removed from the
	// chain loses its password rather than lingering in the file.
	if err := s.SetProxyPasswords(map[string]string{"b": "three"}); err != nil {
		t.Fatal(err)
	}
	if pw, _ := s.ProxyPassword("a"); pw != "" {
		t.Errorf("password for a removed proxy survived: %q", pw)
	}
	if pw, _ := s.ProxyPassword("b"); pw != "three" {
		t.Errorf("ProxyPassword(b) = %q, want three", pw)
	}
}

func TestProxyPasswordsKeepOtherSecrets(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetAPIKey("profile", "sk-test"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.MCPToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxyPasswords(map[string]string{"a": "one"}); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.APIKey("profile"); k != "sk-test" {
		t.Errorf("API key = %q after saving proxy passwords", k)
	}
	if got, _ := s.MCPToken(); got != tok {
		t.Error("MCP token changed when proxy passwords were saved")
	}
}

func TestUpdateProxyPasswordsLeavesOthersAlone(t *testing.T) {
	s := New(t.TempDir())
	if err := s.UpdateProxyPasswords(map[string]string{"global": "g", "eng": "e"}); err != nil {
		t.Fatal(err)
	}
	// Saving one chain mentions only its own proxies.
	if err := s.UpdateProxyPasswords(map[string]string{"global": "g2", "gone": ""}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"global": "g2", "eng": "e", "gone": ""} {
		if got, err := s.ProxyPassword(id); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", id, got, err, want)
		}
	}
	if err := s.UpdateProxyPasswords(map[string]string{"eng": ""}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ProxyPassword("eng"); got != "" {
		t.Errorf("removed password still there: %q", got)
	}
}
