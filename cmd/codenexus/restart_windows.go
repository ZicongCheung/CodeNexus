//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the replacement process in its own group so that it
// outlives the process that spawned it: the whole point of a restart is that
// the old process is about to exit.
func detachProcess(cmd *exec.Cmd) {
	const createNewProcessGroup = 0x00000200
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}
