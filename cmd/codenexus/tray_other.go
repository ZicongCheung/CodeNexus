//go:build !windows

package main

import "errors"

// trayActions is what the tray menu would offer. It is declared on every
// platform so the caller does not need build tags of its own.
type trayActions struct {
	ConsoleURL string
	ConfigPath string
	LogPath    string
	Dir        string
	StatusText func() string
	Quit       func()
}

// startTray reports that the tray is a Windows-only surface. The other
// platforms keep the console window, where the same information is visible
// anyway, and building them stays free of GUI dependencies.
func startTray(trayActions) error {
	return errors.New("tray: only available on Windows")
}
