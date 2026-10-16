//go:build windows

package main

import "golang.org/x/sys/windows"

var procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// hasConsole reports whether the process is attached to a console window. The
// windowed build has none, which is how it decides to send the log to a file
// instead of nowhere.
func hasConsole() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd != 0
}
