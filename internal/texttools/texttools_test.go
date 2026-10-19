package texttools

import "testing"

func toolByName(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %q", name)
	return Tool{}
}

func apply(t *testing.T, name, in string) (string, error) {
	t.Helper()
	return toolByName(t, name).Apply(in)
}

func TestBase64RoundTrip(t *testing.T) {
	cases := []string{"", "hello", `{"user":"qa","role":"admin"}`, "spaces and symbols !@#$%^&*()", "ünïcödé ☃"}
	for _, in := range cases {
		enc, _ := apply(t, "Base64 Encode", in)
		dec, err := apply(t, "Base64 Decode", enc)
		if err != nil || dec != in {
			t.Errorf("round trip %q: enc=%q dec=%q err=%v", in, enc, dec, err)
		}
	}
	if enc, _ := apply(t, "Base64 Encode", "hi"); enc != "aGk=" {
		t.Errorf("Base64 Encode(hi) = %q", enc)
	}
}

func TestBase64DecodeAcceptsVariants(t *testing.T) {
	// standard, raw (no padding), url-safe, and with surrounding whitespace.
	for _, in := range []string{"aGVsbG8=", "aGVsbG8", "  aGVsbG8=\n", "YQ=="} {
		if _, err := apply(t, "Base64 Decode", in); err != nil {
			t.Errorf("decode %q failed: %v", in, err)
		}
	}
	if _, err := apply(t, "Base64 Decode", "not base64!!"); err == nil {
		t.Error("expected error on invalid base64")
	}
}

func TestURLEncodeMatchesComponentSemantics(t *testing.T) {
	enc, _ := apply(t, "URL Encode", "a b&c=d/e?f")
	if enc != "a%20b%26c%3Dd%2Fe%3Ff" {
		t.Errorf("URL Encode = %q", enc)
	}
	// Unreserved characters are left alone.
	if got, _ := apply(t, "URL Encode", "A-z0_9.~"); got != "A-z0_9.~" {
		t.Errorf("unreserved encoded: %q", got)
	}
}

func TestURLDecode(t *testing.T) {
	dec, err := apply(t, "URL Decode", "a%20b%2Bc")
	if err != nil || dec != "a b+c" { // %2B -> '+', literal '+' preserved
		t.Fatalf("URL Decode = %q, %v", dec, err)
	}
	if _, err := apply(t, "URL Decode", "bad%zz"); err == nil {
		t.Error("expected error on bad escape")
	}
	if _, err := apply(t, "URL Decode", "trailing%2"); err == nil {
		t.Error("expected error on truncated escape")
	}
}

func TestURLRoundTrip(t *testing.T) {
	in := "https://stage.example.com/p?q=a b&x=1+2"
	enc, _ := apply(t, "URL Encode", in)
	dec, err := apply(t, "URL Decode", enc)
	if err != nil || dec != in {
		t.Fatalf("round trip: %q -> %q -> %q (%v)", in, enc, dec, err)
	}
}
