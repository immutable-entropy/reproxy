package randid

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestHexLengthAndEncoding(t *testing.T) {
	for _, n := range []int{1, 4, 6, 8, 16, 32} {
		s := Hex(n)
		if len(s) != 2*n {
			t.Errorf("Hex(%d) is %d characters, want %d", n, len(s), 2*n)
		}
		if _, err := hex.DecodeString(s); err != nil {
			t.Errorf("Hex(%d) = %q is not hex: %v", n, s, err)
		}
		if s != strings.ToLower(s) {
			t.Errorf("Hex(%d) = %q is not lowercase", n, s)
		}
	}
	if got := Hex(0); got != "" {
		t.Errorf("Hex(0) = %q", got)
	}
}

// TestHexIsRandom is a smoke test, not a statistical one: the point is
// that every caller does not get the same ID, which is what a silently
// failing rand.Read would have produced.
func TestHexIsRandom(t *testing.T) {
	const runs = 1000
	seen := make(map[string]bool, runs)
	for range runs {
		s := Hex(8)
		if seen[s] {
			t.Fatalf("Hex(8) repeated %q within %d calls", s, runs)
		}
		if s == strings.Repeat("0", 16) {
			t.Fatalf("Hex(8) returned all zeros, the shape of a failed read")
		}
		seen[s] = true
	}
}

func TestFill(t *testing.T) {
	var b [32]byte
	Fill(b[:])
	allZero := true
	for _, c := range b {
		if c != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("Fill left the buffer zeroed")
	}
	// A zero-length buffer must not panic.
	Fill(nil)
	Fill([]byte{})
}
