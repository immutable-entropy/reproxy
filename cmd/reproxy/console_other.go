//go:build !windows

package main

// Only Windows links the binary for a GUI subsystem that has to be given its
// standard streams back; everywhere else they are simply there. See
// console_windows.go.
func attachConsole() {}
