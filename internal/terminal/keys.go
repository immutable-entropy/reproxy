package terminal

import (
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Mod is the modifiers held with a key.
type Mod uint16

// The modifiers a key can carry.
const (
	ModShift Mod = Mod(uv.ModShift)
	ModAlt   Mod = Mod(uv.ModAlt)
	ModCtrl  Mod = Mod(uv.ModCtrl)
	ModMeta  Mod = Mod(uv.ModMeta)
)

// Code is a key: a printable rune, or one of the named keys below.
type Code rune

// The named keys a view can send. Their values are the emulator's own, so
// what reaches the shell is exactly what it expects for the mode it is in
// (application cursor keys, say).
const (
	KeyUp      = Code(uv.KeyUp)
	KeyDown    = Code(uv.KeyDown)
	KeyLeft    = Code(uv.KeyLeft)
	KeyRight   = Code(uv.KeyRight)
	KeyHome    = Code(uv.KeyHome)
	KeyEnd     = Code(uv.KeyEnd)
	KeyPgUp    = Code(uv.KeyPgUp)
	KeyPgDown  = Code(uv.KeyPgDown)
	KeyInsert  = Code(uv.KeyInsert)
	KeyDelete  = Code(uv.KeyDelete)
	KeyEnter   = Code(uv.KeyEnter)
	KeyTab     = Code(uv.KeyTab)
	KeyBackspc = Code(uv.KeyBackspace)
	KeyEscape  = Code(uv.KeyEscape)
	KeySpace   = Code(uv.KeySpace)
	KeyF1      = Code(uv.KeyF1)
	KeyF2      = Code(uv.KeyF2)
	KeyF3      = Code(uv.KeyF3)
	KeyF4      = Code(uv.KeyF4)
	KeyF5      = Code(uv.KeyF5)
	KeyF6      = Code(uv.KeyF6)
	KeyF7      = Code(uv.KeyF7)
	KeyF8      = Code(uv.KeyF8)
	KeyF9      = Code(uv.KeyF9)
	KeyF10     = Code(uv.KeyF10)
	KeyF11     = Code(uv.KeyF11)
	KeyF12     = Code(uv.KeyF12)
)

// Key sends one key press to the shell. The emulator turns it into the
// right sequence for the modes the program running has set.
func (s *Session) Key(code Code, mod Mod) {
	s.mu.Lock()
	if !s.stopped.Load() {
		s.emu.SendKey(vt.KeyPressEvent{Code: rune(code), Mod: uv.KeyMod(mod)})
	}
	s.mu.Unlock()
}
