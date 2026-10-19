// Package randid makes the random identifiers and secrets reproxy hands
// out: target and agent-profile IDs, instance handles, approval IDs, the
// MCP bearer token, the per-process plugin password, a WebSocket mask.
//
// It exists to state one thing in one place. crypto/rand.Read "never
// returns an error, and always fills b entirely" — it calls
// io.ReadFull and crashes the program irrecoverably rather than handing
// back a short read. So there is no error to handle, and code that appears
// to handle one is misleading: a fallback is unreachable, and if it ever
// were reached it would be worse than the crash. target.NewID used to fall
// back to the clock, which would have made an ID an attacker could guess.
package randid

import (
	"crypto/rand"
	"encoding/hex"
)

// Hex returns n random bytes as a lowercase hex string, so 2n characters.
// Callers pick n for what the value has to resist: 4 bytes for a handle a
// person reads, 16 or more for anything a caller could otherwise guess.
func Hex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:errcheck // documented never to fail; it crashes instead
	return hex.EncodeToString(b)
}

// Fill fills b with random bytes.
func Fill(b []byte) {
	rand.Read(b) //nolint:errcheck // documented never to fail; it crashes instead
}
