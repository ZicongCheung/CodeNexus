//go:build !windows

package main

// hasConsole is always true where stdout is a real stream: the console build is
// the only build on these platforms.
func hasConsole() bool { return true }
