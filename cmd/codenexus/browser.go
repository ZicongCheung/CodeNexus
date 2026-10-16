package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/logx"
)

// openBrowser shows a URL in the operator's default browser.
//
// It exists so the first run ends with a working console on screen instead of
// an address the user has to copy out of the log. Failures are returned rather
// than logged: the caller still prints the address, so a headless machine
// degrades to the old behaviour instead of losing the information.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// rundll32 hands the URL to the shell's protocol handler and, unlike
		// `cmd /c start`, never flashes a console window.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// redirectLog sends the process log to a file, creating its directory. The
// windowed build calls this because it has no console to write to.
func redirectLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	logx.SetOutput(f)
	return nil
}

// consoleURL turns a listen address into something a browser can open. A
// wildcard bind is reachable as the loopback address; anything else is used as
// written.
func consoleURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:6828/panel/"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/panel/"
}

// humanDuration renders an uptime the way a status line wants it: short, and
// never "0s" for something that has been up for a few seconds.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
	}
}
