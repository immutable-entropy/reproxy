// Package texttools provides small, pure text transformations used by the
// editor's Tools context menu (Base64, URL encoding, ...).
//
// It has no UI dependency so the transforms can be tested directly and,
// later, reused by plugins.
package texttools

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Tool is one text transformation.
type Tool struct {
	// Name is shown in the menu.
	Name string
	// Apply transforms the input. An error is reported to the user and the
	// text is left unchanged.
	Apply func(string) (string, error)
}

// Tools returns the available transformations, in menu order.
func Tools() []Tool {
	return []Tool{
		{"Base64 Encode", func(s string) (string, error) { return base64Encode(s), nil }},
		{"Base64 Decode", base64Decode},
		{"URL Encode", func(s string) (string, error) { return urlEncode(s), nil }},
		{"URL Decode", urlDecode},
	}
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// base64Decode accepts both standard and URL-safe alphabets, with or without
// padding, and ignores surrounding whitespace.
func base64Decode(s string) (string, error) {
	t := strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(t); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not valid Base64")
}

// urlEncode percent-encodes every byte that isn't an RFC 3986 unreserved
// character, matching JavaScript's encodeURIComponent (so spaces become %20,
// not +).
func urlEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// urlDecode reverses percent-encoding. Unlike query unescaping it leaves '+'
// as a literal plus, so it round-trips urlEncode output.
func urlDecode(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated %% escape")
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("invalid %% escape %q", s[i:i+3])
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
