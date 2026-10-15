//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the replacement process in its own session so that it
// outlives the process that spawned it: the whole point of a restart is that
// the old process is about to exit.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
