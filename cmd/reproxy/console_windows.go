package main

import (
	"os"
	"syscall"
)

// On Windows the binary is linked for the GUI subsystem (-H windowsgui, set
// by build_binary in packaging/common.sh), because a desktop application
// started from the Start menu must not put a console window behind its own.
// The cost of that flag is that the process starts with no standard streams
// at all, which would silently swallow -version, -help, the plugin
// subcommands and everything a headless run logs.
//
// attachConsole gives them back. Started from a console, the process joins
// it and writes there like any command-line tool; started from Explorer or
// as a scheduled task there is no console to join and nothing changes. A
// stream the caller has already pointed somewhere -- reproxy -version >
// out.txt -- is left alone, so redirection still works.
//
// The one visible quirk is that cmd.exe does not wait for a GUI-subsystem
// process, so its prompt returns before the output arrives.
func attachConsole() {
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return // no console to attach to, or we already have one
	}
	os.Stdin = reopen(stdInputHandle, "CONIN$", os.Stdin)
	os.Stdout = reopen(stdOutputHandle, "CONOUT$", os.Stdout)
	os.Stderr = reopen(stdErrorHandle, "CONOUT$", os.Stderr)
}

// reopen points one standard stream at the console just attached to, unless
// it already leads somewhere -- AttachConsole leaves the process's standard
// handles as it found them, so an invalid one is how "nowhere" looks.
func reopen(which int, device string, current *os.File) *os.File {
	if h, err := syscall.GetStdHandle(which); err == nil && h != 0 && h != syscall.InvalidHandle {
		return current
	}
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return current
	}
	// Set the process handle as well as the variable, so anything that
	// reads it directly -- a child process, a terminal check -- agrees
	// with os.Stdout and friends.
	procSetStdHandle.Call(uintptr(uint32(which)), f.Fd())
	return f
}

// ATTACH_PARENT_PROCESS and the STD_*_HANDLE values from the Windows
// headers. The handle numbers are DWORDs, hence the conversion above.
const (
	attachParentProcess = ^uintptr(0) // (DWORD)-1
	stdInputHandle      = -10
	stdOutputHandle     = -11
	stdErrorHandle      = -12
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procSetStdHandle  = kernel32.NewProc("SetStdHandle")
)
